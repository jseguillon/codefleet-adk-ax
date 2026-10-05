package taskrunner

import (
	"context"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRestoredWorkspaceIsNeverReinitialized(t *testing.T) {
	durable := t.TempDir()
	target := filepath.Join(durable, "api")
	w := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "api"}, Spec: &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: "pricing.py", Content: "initial"}}}}
	if e := Prepare(context.Background(), w, target, durable); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(target, "pricing.py"), []byte("agent changes before suspend"), 0600)
	w.Spec.Files[0].Content = "new task spec MUST NOT rewrite restored code"
	if e := Prepare(context.Background(), w, target, durable); e != nil {
		t.Fatal(e)
	}
	p, e := os.ReadFile(filepath.Join(target, "pricing.py"))
	if e != nil || string(p) != "agent changes before suspend" {
		t.Fatalf("restored changes lost: %s %v", p, e)
	}
	if _, e = os.Stat(filepath.Join(target, ".codefleet-initialized.json")); e != nil {
		t.Fatal("marker missing from durable volume")
	}
}

func TestRunnerStaysReadyAfterCommandExit(t *testing.T) {
	durable := t.TempDir()
	target := filepath.Join(durable, "api")
	w := &v1alpha1.Workspace{Metadata: &v1alpha1.ObjectMeta{Name: "api"}, Spec: &v1alpha1.WorkspaceSpec{}}
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := l.Addr().String()
	l.Close()
	task := &v1alpha1.Task{Spec: &v1alpha1.TaskSpec{Command: []string{"python3", "-c", "open('command-finished','w').write('done')"}, Workspaces: []*v1alpha1.WorkspaceRef{{Name: "api", Path: target}}}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{Task: task, Workspaces: []*v1alpha1.Workspace{w}, Listen: addr, DurableRoot: durable})
	}()
	defer cancel()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, e := os.Stat(filepath.Join(target, "command-finished")); e == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("command did not exit")
		}
		time.Sleep(10 * time.Millisecond)
	}
	r, e := http.Get("http://" + addr + "/readyz")
	if e != nil {
		t.Fatal(e)
	}
	r.Body.Close()
	if r.StatusCode != 200 {
		t.Fatalf("runner stopped serving after command exit: %d", r.StatusCode)
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("runner ignored SIGTERM context")
	}
}
func TestWorkspaceMustBeDurable(t *testing.T) {
	if e := Prepare(context.Background(), &v1alpha1.Workspace{}, t.TempDir(), t.TempDir()); e == nil {
		t.Fatal("non-durable workspace accepted")
	}
}
