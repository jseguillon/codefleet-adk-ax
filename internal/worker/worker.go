package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"example.com/codefleet/internal/model"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Client struct {
	URL, Task, Token string
	HTTP             *http.Client
}

func (c Client) Request(ctx context.Context, method, action string, v any, out any) error {
	var body io.Reader
	if v != nil {
		p, e := json.Marshal(v)
		if e != nil {
			return e
		}
		body = bytes.NewReader(p)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.URL, "/")+"/jobs/"+c.Task+"/"+action, body)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	res, e := c.HTTP.Do(req)
	if e != nil {
		return e
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		p, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		return fmt.Errorf("broker %s: %s", res.Status, string(p))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out)
	}
	return nil
}
func (c Client) Event(ctx context.Context, kind, msg string, data map[string]any) error {
	return c.Request(ctx, "POST", "event", model.Event{Kind: kind, Message: msg, Data: data}, nil)
}

type checkpoint struct {
	Task     string            `json:"task"`
	Baseline map[string]string `json:"baseline"`
	Result   *model.Result     `json:"result,omitempty"`
}

func Run(ctx context.Context, root string) error {
	started := time.Now()
	c := Client{URL: os.Getenv("CODEFLEET_BROKER"), Task: os.Getenv("CODEFLEET_TASK"), Token: os.Getenv("CODEFLEET_TOKEN"), HTTP: &http.Client{Timeout: 15 * time.Second}}
	var j model.Job
	if e := c.Request(ctx, "GET", "input", nil, &j); e != nil {
		return e
	}
	if e := os.MkdirAll(root, 0700); e != nil {
		return e
	}
	cpPath := filepath.Join(root, "checkpoint.json")
	var cp checkpoint
	reused := false
	if p, e := os.ReadFile(cpPath); e == nil {
		if e = json.Unmarshal(p, &cp); e != nil {
			return e
		}
		if cp.Task != j.Task {
			return fmt.Errorf("checkpoint belongs to another task")
		}
		reused = true
	} else if !os.IsNotExist(e) {
		return e
	}
	if e := c.Event(ctx, "worker_ready", "Scoped worker online", map[string]any{"repos": j.Node.Repos, "skill": j.Node.Skill, "reused_checkpoint": reused, "pid": os.Getpid()}); e != nil {
		return e
	}
	if cp.Result != nil {
		r := *cp.Result
		r.ReusedCheckpoint = true
		return c.Request(ctx, "POST", "result", r, nil)
	}
	if !reused {
		for _, r := range j.Inputs {
			for _, change := range r.Changes {
				if !contains(j.Node.Repos, change.Repo) {
					continue
				}
				if e := Apply(root, change); e != nil {
					return publishError(ctx, c, j, started, e)
				}
			}
		}
		cp = checkpoint{Task: j.Task, Baseline: map[string]string{}}
		for _, repo := range j.Node.Repos {
			for _, name := range editableFiles(repo) {
				p, e := os.ReadFile(filepath.Join(root, repo, name))
				if e != nil {
					return e
				}
				cp.Baseline[repo+"/"+name] = string(p)
			}
		}
		if e := atomicJSON(cpPath, cp); e != nil {
			return e
		}
	}
	if e := c.Event(ctx, "checkpoint", "Prepared inputs persisted; safe to restart", map[string]any{"reused": reused}); e != nil {
		return e
	}
	if j.DelayMS > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(j.DelayMS) * time.Millisecond):
		}
	}
	r := model.Result{Task: j.Task, Node: j.Node.ID, Success: true, ReusedCheckpoint: reused}
	err := execute(ctx, root, j, cp.Baseline, &r)
	if err != nil {
		r.Success = false
		r.Error = err.Error()
		r.Summary = "Worker failed: " + err.Error()
	}
	r.DurationMS = time.Since(started).Milliseconds()
	cp.Result = &r
	if e := atomicJSON(cpPath, cp); e != nil {
		return e
	}
	if e := c.Request(ctx, "POST", "result", r, nil); e != nil {
		return e
	}
	return nil
}
func publishError(ctx context.Context, c Client, j model.Job, start time.Time, e error) error {
	r := model.Result{Task: j.Task, Node: j.Node.ID, Success: false, Error: e.Error(), Summary: e.Error(), DurationMS: time.Since(start).Milliseconds()}
	return c.Request(ctx, "POST", "result", r, nil)
}
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
func editableFiles(repo string) []string {
	switch repo {
	case "api":
		return []string{"pricing.py"}
	case "web":
		return []string{"coupon.mjs"}
	case "docs":
		return []string{"README.md"}
	}
	return nil
}

// Apply rejects stale bases, traversal and symlink writes before accepting a file.
func Apply(root string, c model.Change) error {
	if e := model.ValidPath(c.Repo, c.Path); e != nil {
		return e
	}
	if model.Hash(c.Content) != c.SHA256 {
		return fmt.Errorf("artifact digest mismatch")
	}
	p := filepath.Join(root, c.Repo, c.Path)
	rel, e := filepath.Rel(root, p)
	if e != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("path escapes workspace")
	}
	for cur := p; cur != root; cur = filepath.Dir(cur) {
		st, e := os.Lstat(cur)
		if e == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink at %s", cur)
		}
		if e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	before, e := os.ReadFile(p)
	if e != nil && !os.IsNotExist(e) {
		return e
	}
	actual := ""
	if e == nil {
		actual = model.Hash(string(before))
	}
	if actual != c.Before {
		return fmt.Errorf("conflict on %s: base %s != %s", model.Key(c), actual, c.Before)
	}
	if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
		return e
	}
	tmp := p + ".codefleet-tmp"
	if e := os.WriteFile(tmp, []byte(c.Content), 0600); e != nil {
		return e
	}
	return os.Rename(tmp, p)
}
func atomicJSON(p string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	if e = os.WriteFile(p+".tmp", b, 0600); e != nil {
		return e
	}
	return os.Rename(p+".tmp", p)
}
func execute(ctx context.Context, root string, j model.Job, base map[string]string, r *model.Result) error {
	switch j.Node.Role {
	case "planner":
		r.Summary = "Coupon feature → contract → 3 repo-scoped implementations → integrate → tests + policy → review → targeted repair → handoff"
	case "contract":
		text := "{\"version\":1,\"inputs\":[\"total_cents:int>=0\",\"percent:int:0..100\"],\"boolean_inputs\":\"rejected\",\"rounding\":\"floor\",\"example\":{\"total_cents\":999,\"percent\":15,\"result\":849}}\n"
		c := model.Change{Repo: "api", Path: "contract.json", Content: text, SHA256: model.Hash(text)}
		if e := Apply(root, c); e != nil {
			return e
		}
		r.Changes = []model.Change{c}
		r.Summary = "Contract: integer cents; floor result; 999 × 85% = 849; validate bounds"
	case "backend", "frontend", "docs", "repair":
		repo := j.Node.Repos[0]
		name := editableFiles(repo)[0]
		text := fixture(j.Node.Role, j.InjectBug)
		if j.Mode == "llm" {
			var e error
			text, e = generate(ctx, root, j, base[repo+"/"+name])
			if e != nil {
				return e
			}
			r.LLMCalls = 1
		}
		c := model.Change{Repo: repo, Path: name, Before: model.Hash(base[repo+"/"+name]), Content: text, SHA256: model.Hash(text)}
		// Restore the original prepared input before an interrupted attempt is rerun.
		if e := os.WriteFile(filepath.Join(root, repo, name), []byte(base[repo+"/"+name]), 0600); e != nil {
			return e
		}
		if e := Apply(root, c); e != nil {
			return e
		}
		r.Changes = []model.Change{c}
		r.Summary = fmt.Sprintf("%s: %s/%s (%s)", j.Node.Role, repo, name, j.Mode)
	case "integrator":
		r.Summary = "Merged API, browser and docs artifacts; all base hashes matched"
	case "tester":
		for _, s := range []struct {
			name, dir string
			args      []string
		}{{"python-unittest", "api", []string{"python3", "-m", "unittest", "-v"}}, {"node-test", "web", []string{"node", "--test", "coupon.test.mjs"}}} {
			cmd := exec.CommandContext(ctx, s.args[0], s.args[1:]...)
			cmd.Dir = filepath.Join(root, s.dir)
			p, e := cmd.CombinedOutput()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.Checks = append(r.Checks, model.Check{Name: s.name, Passed: e == nil, Log: string(p)})
		}
		r.Summary = "Executed Python unittest and node:test suites"
	case "security":
		p, e := os.ReadFile(filepath.Join(root, "api", "pricing.py"))
		if e != nil {
			return e
		}
		bad := strings.Contains(string(p), "eval(") || strings.Contains(string(p), "exec(") || strings.Contains(string(p), "subprocess")
		cmd := exec.CommandContext(ctx, "python3", "-m", "py_compile", "pricing.py")
		cmd.Dir = filepath.Join(root, "api")
		log, e := cmd.CombinedOutput()
		r.Checks = append(r.Checks, model.Check{Name: "python-policy", Passed: e == nil && !bad, Log: string(log) + "\nNo eval/exec/subprocess permitted in pricing"})
		p, e = os.ReadFile(filepath.Join(root, "web", "coupon.mjs"))
		if e != nil {
			return e
		}
		r.Checks = append(r.Checks, model.Check{Name: "js-input-policy", Passed: strings.Contains(string(p), "Number.isSafeInteger"), Log: "Check safe integer validation exists (demo heuristic)"})
		r.Summary = "Demo policy checks completed"
	case "reviewer":
		checks := 0
		pass := true
		for _, in := range j.Inputs {
			if (j.Node.ID == "review" && (in.Node == "unit" || in.Node == "security")) || (j.Node.ID == "review_final" && (in.Node == "retest" || in.Node == "resecure")) {
				if !in.Success {
					pass = false
				}
				for _, c := range in.Checks {
					checks++
					if !c.Passed {
						pass = false
					}
					r.Checks = append(r.Checks, c)
				}
			}
		}
		if checks != 4 {
			return fmt.Errorf("review needs 4 fresh checks, received %d", checks)
		}
		if pass {
			r.Summary = "APPROVED: all 4 fresh checks passed"
		} else {
			r.Summary = "CHANGES_REQUESTED: repair API fractional cents; retain tests"
		}
	case "release":
		r.Summary = "Reviewed artifact bundle ready for MR; deployment lifecycle is Part 2"
	default:
		return fmt.Errorf("unknown role %q", j.Node.Role)
	}
	return nil
}
func fixture(role string, bug bool) string {
	switch role {
	case "backend", "repair":
		op := "//"
		if role == "backend" && bug {
			op = "/"
		}
		return "\"\"\"Coupon pricing in integer cents.\"\"\"\ndef apply_coupon(total_cents: int, percent: int) -> int:\n    if type(total_cents) is not int or type(percent) is not int:\n        raise ValueError('integer inputs required')\n    if total_cents < 0 or not 0 <= percent <= 100:\n        raise ValueError('invalid coupon input')\n    return total_cents * (100 - percent) " + op + " 100\n"
	case "frontend":
		return "export function previewCoupon(totalCents, percent) {\n  if (!Number.isSafeInteger(totalCents) || !Number.isSafeInteger(percent) || totalCents < 0 || percent < 0 || percent > 100) throw new Error('Invalid coupon input');\n  return Math.floor(totalCents * (100 - percent) / 100);\n}\n"
	case "docs":
		return "# CodeFleet Shop — Coupons\n\nPrices use integer cents. Supply total_cents >= 0 and an integer percent between 0 and 100. Booleans and invalid values are rejected. The final price is rounded down to whole cents.\n\nExample: 999 cents with a 15% coupon gives 849 cents (8.49 €). The browser preview follows the same contract as the API.\n"
	}
	return ""
}
