package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"example.com/codefleet/demo"
	"example.com/codefleet/internal/axclient"
	"example.com/codefleet/internal/broker"
	"example.com/codefleet/internal/flow"
	"example.com/codefleet/internal/mockax"
	"example.com/codefleet/internal/model"
	"example.com/codefleet/internal/worker"
	"example.com/codefleet/web"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func Main(ctx context.Context, args []string) error {
	if len(args) == 0 {
		args = []string{"demo"}
	}
	if args[0] == "worker" {
		f := flag.NewFlagSet("worker", flag.ContinueOnError)
		root := f.String("root", "/workspace/code", "workspace")
		if e := f.Parse(args[1:]); e != nil {
			return e
		}
		return worker.Run(ctx, *root)
	}
	if args[0] != "demo" && args[0] != "run" {
		return fmt.Errorf("usage: codefleet demo [flags] | run [flags]")
	}
	f := flag.NewFlagSet(args[0], flag.ContinueOnError)
	listen := f.String("listen", "127.0.0.1:8088", "dashboard and artifact broker address")
	out := f.String("out", "runs", "output parent directory")
	backend := f.String("backend", "mock", "mock or ax")
	endpoint := f.String("endpoint", "", "AX gRPC endpoint, required for ax backend")
	mockAddr := f.String("mock-grpc", "127.0.0.1:0", "mock AX gRPC listen address")
	image := f.String("image", "", "command runner image, required for ax backend")
	workerBroker := f.String("worker-broker", "", "broker URL reachable FROM AX tasks")
	insecure := f.Bool("insecure", false, "plaintext gRPC for local port-forward only")
	ca := f.String("ca-file", "", "CA certificate for AX TLS")
	space := f.String("atespace", "default", "AX atespace")
	mode := f.String("mode", "fixture", "fixture or llm")
	bug := f.Bool("inject-bug", true, "fixture: inject fractional-cent bug to exercise repair")
	cycle := f.Bool("cycle", true, "suspend/resume backend after its checkpoint")
	delay := f.Int("delay-ms", 900, "demo pause after preparation; use 0 to measure overhead")
	timeout := f.Duration("timeout", 10*time.Minute, "entire graph deadline")
	exitOnDone := f.Bool("exit", false, "exit after graph completion (CI mode)")
	keep := f.Bool("keep-tasks", false, "retain AX tasks/workspaces after completion")
	repoAPI := f.String("repo-api", "", "Git URL for demo API repo (AX backend)")
	repoWeb := f.String("repo-web", "", "Git URL for demo web repo (AX backend)")
	repoDocs := f.String("repo-docs", "", "Git URL for demo docs repo (AX backend)")
	repoRef := f.String("repo-ref", "main", "Git branch/tag for demo repos")
	goal := f.String("goal", "Add integer-cent percentage coupons to the pricing API, browser preview and product docs. Floor fractional cents; reject invalid inputs.", "feature request")
	if e := f.Parse(args[1:]); e != nil {
		return e
	}
	if *backend != "mock" && *backend != "ax" {
		return fmt.Errorf("backend must be mock or ax")
	}
	if *mode != "fixture" && *mode != "llm" {
		return fmt.Errorf("mode must be fixture or llm")
	}
	if *mode == "llm" && (os.Getenv("OPENAI_BASE_URL") == "" || os.Getenv("OPENAI_MODEL") == "") {
		return fmt.Errorf("set OPENAI_BASE_URL and OPENAI_MODEL for llm mode")
	}
	if *backend == "ax" && (*endpoint == "" || *image == "" || *workerBroker == "") {
		return fmt.Errorf("AX backend requires --endpoint, --image and --worker-broker")
	}
	if *cycle && *delay < 300 {
		return fmt.Errorf("--cycle requires --delay-ms >= 300 to interrupt before result publication; use --cycle=false for benchmarks")
	}
	if *backend == "mock" && (*repoAPI != "" || *repoWeb != "" || *repoDocs != "") {
		return fmt.Errorf("mock clones local seeded demo repositories; remote URLs require --backend ax")
	}
	if *backend == "mock" && *keep {
		return fmt.Errorf("--keep-tasks applies to real AX; local mock stops with this process")
	}
	buf := make([]byte, 4)
	if _, e := rand.Read(buf); e != nil {
		return e
	}
	runID := "cf-" + hex.EncodeToString(buf)
	dir, e := filepath.Abs(filepath.Join(*out, runID))
	if e != nil {
		return e
	}
	b, e := broker.New(dir, runID, *mode, *backend)
	if e != nil {
		return e
	}
	b.Status = "running"
	seed := filepath.Join(dir, "seed")
	if e = Seed(ctx, seed); e != nil {
		return e
	}
	l, e := net.Listen("tcp", *listen)
	if e != nil {
		return e
	}
	url := "http://" + l.Addr().String()
	if *workerBroker == "" {
		*workerBroker = url
	}
	server := http.Server{Handler: b.Handler(web.Index), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(l)
	defer server.Close()
	fmt.Printf("Dashboard: %s\nOutput: %s\n", url, dir)
	var mock *mockax.Server
	if *backend == "mock" {
		binary, e := os.Executable()
		if e != nil {
			return e
		}
		mock, *endpoint, e = mockax.Start(*mockAddr, filepath.Join(dir, "actors"), seed, binary, b, "frontend")
		if e != nil {
			return e
		}
		defer mock.Stop()
		*insecure = true
		fmt.Printf("Mock AX gRPC: %s (local processes)\n", *endpoint)
	}
	config := axclient.Config{Endpoint: *endpoint, CAFile: *ca, Token: os.Getenv("AX_TOKEN"), Space: *space, Image: *image, WorkerBroker: *workerBroker, Mode: *mode, Goal: *goal, Insecure: *insecure, InjectBug: *bug, Cycle: *cycle, KeepTasks: *keep, DelayMS: *delay, RepoURLs: map[string]string{"api": *repoAPI, "web": *repoWeb, "docs": *repoDocs}, RepoRef: *repoRef}
	ex, e := axclient.New(config, b)
	if e != nil {
		return e
	}
	runCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	packet, runErr := flow.Run(runCtx, ex)
	if runErr == nil {
		runErr = Bundle(ctx, dir, seed, packet, b)
	}
	closeErr := ex.Close()
	if closeErr != nil && runErr == nil {
		runErr = closeErr
	}
	status := "completed"
	if runErr != nil {
		status = "failed"
		b.Event("workflow", "error", runErr.Error(), nil)
	}
	b.Finish(status)
	if e = Replay(dir, b); e != nil {
		return e
	}
	fmt.Printf("Replay: %s\n", filepath.Join(dir, "replay.html"))
	if *exitOnDone {
		return runErr
	}
	fmt.Println("Graph finished. Dashboard remains available; Ctrl-C to stop.")
	<-ctx.Done()
	return runErr
}
func Seed(ctx context.Context, root string) error {
	for repo, names := range demo.RepoFiles {
		p := filepath.Join(root, repo)
		if e := os.MkdirAll(p, 0700); e != nil {
			return e
		}
		for _, name := range names {
			if e := os.WriteFile(filepath.Join(p, name), []byte(demo.Read("repos/"+repo+"/"+name)), 0644); e != nil {
				return e
			}
		}
		for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "."}, {"-c", "user.name=CodeFleet Demo", "-c", "user.email=demo@example.invalid", "commit", "-qm", "Seed coupon feature repositories"}} {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = p
			if log, e := cmd.CombinedOutput(); e != nil {
				return fmt.Errorf("seed git: %w: %s", e, log)
			}
		}
	}
	return nil
}
func Bundle(ctx context.Context, dir, seed string, packet model.Packet, b *broker.Broker) error {
	root := filepath.Join(dir, "bundle")
	if e := os.MkdirAll(root, 0700); e != nil {
		return e
	}
	for repo := range demo.RepoFiles {
		cmd := exec.CommandContext(ctx, "git", "clone", "--quiet", "--no-hardlinks", filepath.Join(seed, repo), filepath.Join(root, repo))
		if log, e := cmd.CombinedOutput(); e != nil {
			return fmt.Errorf("bundle clone: %w: %s", e, log)
		}
	}
	changes := []model.Change{}
	for _, id := range packet.Tasks {
		r, ok := b.GetResult(id)
		if !ok {
			return fmt.Errorf("missing reviewed result")
		}
		for _, c := range r.Changes {
			if e := worker.Apply(root, c); e != nil {
				return e
			}
			changes = append(changes, c)
		}
	}
	for repo := range demo.RepoFiles {
		stage := exec.CommandContext(ctx, "git", "add", "-N", "--", ".")
		stage.Dir = filepath.Join(root, repo)
		if log, err := stage.CombinedOutput(); err != nil {
			return fmt.Errorf("prepare patch: %w: %s", err, log)
		}
		cmd := exec.CommandContext(ctx, "git", "diff", "--no-ext-diff", "--binary")
		cmd.Dir = filepath.Join(root, repo)
		p, e := cmd.Output()
		if e != nil {
			return e
		}
		if e = os.WriteFile(filepath.Join(dir, repo+".patch"), p, 0600); e != nil {
			return e
		}
	}
	manifest, _ := json.MarshalIndent(changes, "", "  ")
	if e := os.WriteFile(filepath.Join(dir, "artifacts.json"), manifest, 0600); e != nil {
		return e
	}
	body := "# Add percentage coupons across pricing API, browser preview and docs\n\nPrices use integer cents and floor fractional cents. Invalid amounts and discount percentages are rejected.\n\nValidation: independent Python unittest and node:test suites plus demo policy checks. The graph may perform one API-only repair before a fresh review.\n\nThis handoff includes reviewed file hashes and evidence. No remote MR was opened and no deployment was performed in Part 1.\n"
	if e := os.WriteFile(filepath.Join(dir, "MR.md"), []byte(body), 0600); e != nil {
		return e
	}
	b.Event("handoff", "bundle", "Reviewed code and per-repo patches written", map[string]any{"changes": len(changes)})
	return nil
}
func Replay(dir string, b *broker.Broker) error {
	p, e := json.Marshal(b.Snapshot())
	if e != nil {
		return e
	}
	script := "<script>window.__REPORT__=" + string(p) + ";</script>"
	html := strings.Replace(string(web.Index), "<script>", script+"<script>", 1)
	return os.WriteFile(filepath.Join(dir, "replay.html"), []byte(html), 0600)
}
