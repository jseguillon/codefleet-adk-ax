// Package taskrunner implements AX's command runner contract for this demo.
// All initialization markers live on the durable workspace volume.
package taskrunner

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type Config struct {
	Task        *v1alpha1.Task
	Workspaces  []*v1alpha1.Workspace
	Listen      string
	DurableRoot string
}

func Prepare(ctx context.Context, w *v1alpha1.Workspace, target, durable string) error {
	abs, e := filepath.Abs(target)
	if e != nil {
		return e
	}
	base, e := filepath.Abs(durable)
	if e != nil {
		return e
	}
	rel, e := filepath.Rel(base, abs)
	if e != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return fmt.Errorf("workspace outside durable root")
	}
	if e = os.MkdirAll(abs, 0755); e != nil {
		return e
	}
	marker := filepath.Join(abs, ".codefleet-initialized.json")
	if _, e = os.Stat(marker); e == nil {
		return nil
	} else if !os.IsNotExist(e) {
		return e
	}
	if w == nil {
		return fmt.Errorf("workspace definition missing")
	}
	// No autonomous bootstrap: toolchains are prebuilt in the command image.
	for _, repo := range w.GetSpec().GetGit() {
		if repo.Dir != "." {
			return fmt.Errorf("this runner supports one Git repo at workspace root")
		}
		if _, e = os.Stat(filepath.Join(abs, ".git")); os.IsNotExist(e) {
			args := []string{"clone", "--depth", "1"}
			if repo.Branch != "" {
				args = append(args, "--branch", repo.Branch)
			}
			args = append(args, "--", repo.Repo, abs)
			cmd := exec.CommandContext(ctx, "git", args...)
			if p, e := cmd.CombinedOutput(); e != nil {
				return fmt.Errorf("git clone: %w: %s", e, p)
			}
		}
	}
	for _, f := range w.GetSpec().GetFiles() {
		if f.Path == "" || filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path || strings.HasPrefix(f.Path, ".") || strings.Contains(f.Path, "\\") {
			return fmt.Errorf("unsafe inline file path")
		}
		p := filepath.Join(abs, f.Path)
		if e = os.MkdirAll(filepath.Dir(p), 0755); e != nil {
			return e
		}
		if e = os.WriteFile(p, []byte(f.Content), 0644); e != nil {
			return e
		}
	}
	info := map[string]string{"workspace": w.GetMetadata().GetName(), "initialized_at": time.Now().UTC().Format(time.RFC3339Nano)}
	p, _ := json.Marshal(info)
	if e = os.WriteFile(marker+".tmp", p, 0600); e != nil {
		return e
	}
	return os.Rename(marker+".tmp", marker)
}
func Run(ctx context.Context, c Config) error {
	if c.Task == nil || c.Task.Spec == nil {
		return fmt.Errorf("Task spec is required")
	}
	if c.Task.Spec.Debug {
		return fmt.Errorf("custom command runner does not implement guest services; debug must be false")
	}
	if len(c.Task.Spec.Command) == 0 {
		return fmt.Errorf("spec.command is required")
	}
	if c.DurableRoot == "" {
		c.DurableRoot = "/workspace"
	}
	if c.Listen == "" {
		c.Listen = ":80"
	}
	var ready atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			http.Error(w, "preparing workspace", 503)
			return
		}
		w.Write([]byte("ready"))
	})
	mux.HandleFunc("GET /metadata/v1alpha1/ax/task", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		p, _ := yaml.Marshal(c.Task)
		w.Write(p)
	})
	mux.HandleFunc("GET /metadata/v1alpha1/ax/workspaces", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		for _, ws := range c.Workspaces {
			p, _ := yaml.Marshal(ws)
			w.Write([]byte("---\n"))
			w.Write(p)
		}
	})
	l, e := net.Listen("tcp", c.Listen)
	if e != nil {
		return e
	}
	srv := http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go srv.Serve(l)
	defer srv.Close()
	byName := map[string]*v1alpha1.Workspace{}
	for _, w := range c.Workspaces {
		byName[w.GetMetadata().GetName()] = w
	}
	cwd := c.DurableRoot
	for i, ref := range c.Task.Spec.Workspaces {
		if ref.Goal != "" {
			return fmt.Errorf("command runner does not execute LLM bootstrap goals")
		}
		target := ref.Path
		if target == "" {
			target = filepath.Join(c.DurableRoot, ref.Name)
		}
		if e := Prepare(ctx, byName[ref.Name], target, c.DurableRoot); e != nil {
			return e
		}
		if i == 0 {
			cwd = target
		}
	}
	cmd := exec.Command(c.Task.Spec.Command[0], c.Task.Spec.Command[1:]...)
	cmd.Dir = cwd
	cmd.Env = os.Environ()
	for _, v := range c.Task.Spec.Env {
		cmd.Env = append(cmd.Env, v.Name+"="+v.Value)
	}
	cmd.Env = append(cmd.Env, "AX_METADATA_URL=http://127.0.0.1"+c.Listen)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if e = cmd.Start(); e != nil {
		return e
	}
	done := make(chan struct{})
	go func() {
		e := cmd.Wait()
		fmt.Fprintf(os.Stderr, "command exited: %v (runner stays online)\n", e)
		close(done)
	}()
	ready.Store(true)
	<-ctx.Done()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return nil
}
