package axclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"example.com/codefleet/demo"
	"example.com/codefleet/internal/broker"
	"example.com/codefleet/internal/model"
	"fmt"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"os"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Endpoint, CAFile, Token, Space, Image, WorkerBroker, Mode, Goal string
	Insecure, InjectBug, Cycle, KeepTasks                           bool
	DelayMS                                                         int
	RepoURLs                                                        map[string]string
	RepoRef                                                         string
}
type Executor struct {
	Config     Config
	Broker     *broker.Broker
	Client     v1alpha1.AXClient
	conn       *grpc.ClientConn
	mu         sync.Mutex
	owned      map[string]bool
	workspaces map[string]bool
}

func New(c Config, b *broker.Broker) (*Executor, error) {
	var tc credentials.TransportCredentials
	if c.Insecure {
		tc = insecure.NewCredentials()
	} else {
		tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
		if c.CAFile != "" {
			p, e := os.ReadFile(c.CAFile)
			if e != nil {
				return nil, e
			}
			pool, e := x509.SystemCertPool()
			if e != nil {
				pool = x509.NewCertPool()
			}
			if !pool.AppendCertsFromPEM(p) {
				return nil, fmt.Errorf("invalid CA file")
			}
			tlsConfig.RootCAs = pool
		}
		tc = credentials.NewTLS(tlsConfig)
	}
	conn, e := grpc.NewClient(c.Endpoint, grpc.WithTransportCredentials(tc))
	if e != nil {
		return nil, e
	}
	return &Executor{Config: c, Broker: b, Client: v1alpha1.NewAXClient(conn), conn: conn, owned: map[string]bool{}, workspaces: map[string]bool{}}, nil
}
func (e *Executor) auth(ctx context.Context) context.Context {
	if e.Config.Token != "" {
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+e.Config.Token)
	}
	return ctx
}
func (e *Executor) Run(ctx context.Context, d model.Definition, packet model.Packet) (model.Packet, error) {
	name := e.Broker.RunID + "-" + d.ID
	j, exists := e.Broker.GetJob(name)
	if !exists {
		j = model.Job{Task: name, Node: d, Mode: e.Config.Mode, Goal: e.Config.Goal, DelayMS: e.Config.DelayMS, InjectBug: e.Config.InjectBug}
		for _, id := range packet.Tasks {
			r, ok := e.Broker.GetResult(id)
			if !ok {
				return packet, fmt.Errorf("missing dependency %s", id)
			}
			scoped := r
			scoped.Changes = nil
			for _, c := range r.Changes {
				for _, repo := range d.Repos {
					if repo == c.Repo {
						scoped.Changes = append(scoped.Changes, c)
					}
				}
			}
			j.Inputs = append(j.Inputs, scoped)
		}
		if e.Config.Mode == "llm" && (d.Role == "backend" || d.Role == "frontend" || d.Role == "docs" || d.Role == "repair") {
			j.LLM = &model.LLMConfig{Endpoint: os.Getenv("OPENAI_BASE_URL"), Model: os.Getenv("OPENAI_MODEL"), APIKey: os.Getenv("OPENAI_API_KEY")}
		}
		var err error
		j, err = e.Broker.Register(j)
		if err != nil {
			return packet, err
		}
	}
	e.mu.Lock()
	owned := e.owned[name]
	e.mu.Unlock()
	// Reconcile an uncertain CreateTask response using the unguessable job token.
	// An existing task with different credentials is never adopted or deleted.
	if !owned && exists {
		rpcCtx, cancel := context.WithTimeout(e.auth(ctx), 5*time.Second)
		t, err := e.Client.GetTask(rpcCtx, &v1alpha1.GetTaskRequest{Atespace: e.Config.Space, Name: name})
		cancel()
		if err == nil {
			matches := false
			for _, v := range t.GetSpec().GetEnv() {
				if v.Name == "CODEFLEET_TOKEN" && v.Value == j.Token {
					matches = true
				}
			}
			if !matches {
				return packet, fmt.Errorf("refusing to adopt unowned AX task %s", name)
			}
			e.mu.Lock()
			e.owned[name] = true
			e.mu.Unlock()
			owned = true
			if err = e.waitReady(ctx, name); err != nil {
				return packet, err
			}
			e.Broker.Event(d.ID, "reconciled", "Recovered task after uncertain CreateTask response", nil)
		} else if status.Code(err) != codes.NotFound {
			return packet, err
		}
	}
	if !owned {
		e.Broker.Event(d.ID, "creating", "Create AX task with scoped repos and skill", map[string]any{"repos": d.Repos, "skill": d.Skill, "task": name})
		began := time.Now()
		refs, err := e.bindings(ctx, name, d)
		if err != nil {
			return packet, err
		}
		task := &v1alpha1.Task{ApiVersion: "ax.io/v1alpha1", Kind: "Task", Metadata: &v1alpha1.ObjectMeta{Name: name, Atespace: e.Config.Space}, Spec: &v1alpha1.TaskSpec{Image: e.Config.Image, Command: []string{"/usr/local/bin/codefleet-worker", "worker", "--root", "/workspace/code"}, Workspaces: refs, Resources: &v1alpha1.ResourceReqs{Requests: &v1alpha1.ResourceList{Cpu: "1", Memory: "512Mi"}, Limits: &v1alpha1.ResourceList{Cpu: "2", Memory: "1Gi"}}, Env: []*v1alpha1.EnvVar{{Name: "CODEFLEET_BROKER", Value: e.Config.WorkerBroker}, {Name: "CODEFLEET_TASK", Value: name}, {Name: "CODEFLEET_TOKEN", Value: j.Token}}}}
		rpcCtx, cancel := context.WithTimeout(e.auth(ctx), 15*time.Second)
		_, err = e.Client.CreateTask(rpcCtx, &v1alpha1.CreateTaskRequest{Task: task})
		cancel()
		if err != nil {
			return packet, err
		}
		e.mu.Lock()
		e.owned[name] = true
		e.mu.Unlock()
		if err = e.waitReady(ctx, name); err != nil {
			return packet, err
		}
		e.Broker.Event(d.ID, "ready", "WorkspaceReady observed via AX gRPC", map[string]any{"start_ms": time.Since(began).Milliseconds(), "task": name})
	}
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()
	cycled := false
	for {
		if r, ok := e.Broker.GetResult(name); ok {
			if !r.Success {
				return packet, fmt.Errorf("worker %s failed: %s", name, r.Error)
			}
			return model.Packet{Tasks: append(append([]string{}, packet.Tasks...), name)}, nil
		}
		if e.Config.Cycle && d.ID == "backend" && !cycled && e.checkpointReady(d.ID) {
			began := time.Now()
			e.Broker.Event(d.ID, "suspending", "Suspend task after durable checkpoint", nil)
			rpcCtx, cancel := context.WithTimeout(e.auth(ctx), 20*time.Second)
			_, err := e.Client.SuspendTask(rpcCtx, &v1alpha1.SuspendTaskRequest{Atespace: e.Config.Space, Name: name})
			cancel()
			if err != nil {
				return packet, err
			}
			if err = e.waitPhase(ctx, name, "Suspended"); err != nil {
				return packet, err
			}
			e.Broker.Event(d.ID, "suspended", "Workspace preserved; process stopped", nil)
			rpcCtx, cancel = context.WithTimeout(e.auth(ctx), 20*time.Second)
			_, err = e.Client.ResumeTask(rpcCtx, &v1alpha1.ResumeTaskRequest{Atespace: e.Config.Space, Name: name})
			cancel()
			if err != nil {
				return packet, err
			}
			if err = e.waitReady(ctx, name); err != nil {
				return packet, err
			}
			e.Broker.Event(d.ID, "resumed", "New worker process; same persisted checkpoint", map[string]any{"cycle_ms": time.Since(began).Milliseconds()})
			cycled = true
		}
		select {
		case <-ctx.Done():
			return packet, ctx.Err()
		case <-ticker.C:
		}
		rpcCtx, cancel := context.WithTimeout(e.auth(ctx), 5*time.Second)
		t, err := e.Client.GetTask(rpcCtx, &v1alpha1.GetTaskRequest{Atespace: e.Config.Space, Name: name})
		cancel()
		if err != nil {
			return packet, err
		}
		if t.Status.GetPhase() == "Failed" {
			return packet, fmt.Errorf("AX task %s failed", name)
		}
	}
}
func (e *Executor) checkpointReady(node string) bool {
	state := e.Broker.Snapshot()
	for _, ev := range state["events"].([]model.Event) {
		if ev.Node == node && ev.Kind == "checkpoint" {
			return true
		}
	}
	return false
}
func (e *Executor) waitReady(ctx context.Context, name string) error {
	return e.wait(ctx, name, func(t *v1alpha1.Task) bool {
		if t.Status.GetPhase() != "Running" {
			return false
		}
		for _, c := range t.Status.Conditions {
			if c.Type == "WorkspaceReady" && c.Status == "True" {
				return true
			}
		}
		return false
	})
}
func (e *Executor) waitPhase(ctx context.Context, name, phase string) error {
	return e.wait(ctx, name, func(t *v1alpha1.Task) bool { return t.Status.GetPhase() == phase })
}
func (e *Executor) wait(ctx context.Context, name string, pred func(*v1alpha1.Task) bool) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		t, err := e.Client.GetTask(e.auth(ctx), &v1alpha1.GetTaskRequest{Atespace: e.Config.Space, Name: name})
		if err != nil {
			return err
		}
		if pred(t) {
			return nil
		}
		if t.Status.GetPhase() == "Failed" {
			return fmt.Errorf("task failed before readiness")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
func (e *Executor) bindings(ctx context.Context, prefix string, d model.Definition) ([]*v1alpha1.WorkspaceRef, error) {
	refs := []*v1alpha1.WorkspaceRef{}
	for _, repo := range d.Repos {
		w := &v1alpha1.Workspace{ApiVersion: "ax.io/v1alpha1", Kind: "Workspace", Metadata: &v1alpha1.ObjectMeta{Name: prefix + "-" + repo, Atespace: e.Config.Space}, Spec: &v1alpha1.WorkspaceSpec{}}
		if url := e.Config.RepoURLs[repo]; url != "" {
			w.Spec.Git = []*v1alpha1.GitRepo{{Name: repo, Repo: url, Branch: e.Config.RepoRef, Dir: ".", Depth: 1}}
		} else {
			for _, f := range demo.RepoFiles[repo] {
				w.Spec.Files = append(w.Spec.Files, &v1alpha1.File{Path: f, Content: demo.Read("repos/" + repo + "/" + f)})
			}
		}
		if err := e.updateWorkspace(ctx, w); err != nil {
			return nil, err
		}
		refs = append(refs, &v1alpha1.WorkspaceRef{Name: w.Metadata.Name, Path: "/workspace/code/" + repo})
	}
	w := &v1alpha1.Workspace{ApiVersion: "ax.io/v1alpha1", Kind: "Workspace", Metadata: &v1alpha1.ObjectMeta{Name: prefix + "-skills", Atespace: e.Config.Space}, Spec: &v1alpha1.WorkspaceSpec{Files: []*v1alpha1.File{{Path: d.Skill + ".md", Content: demo.Read("skills/" + d.Skill + ".md")}}, Skills: &v1alpha1.SkillsConfig{Path: "."}}}
	if err := e.updateWorkspace(ctx, w); err != nil {
		return nil, err
	}
	refs = append(refs, &v1alpha1.WorkspaceRef{Name: w.Metadata.Name, Path: "/workspace/code/skills"})
	return refs, nil
}
func (e *Executor) updateWorkspace(ctx context.Context, w *v1alpha1.Workspace) error {
	rpcCtx, cancel := context.WithTimeout(e.auth(ctx), 15*time.Second)
	defer cancel()
	_, err := e.Client.UpdateWorkspace(rpcCtx, &v1alpha1.UpdateWorkspaceRequest{Workspace: w})
	if err == nil {
		e.mu.Lock()
		e.workspaces[w.Metadata.Name] = true
		e.mu.Unlock()
	}
	return err
}
func (e *Executor) Close() error {
	defer e.conn.Close()
	if e.Config.KeepTasks {
		return nil
	}
	e.mu.Lock()
	names := []string{}
	ws := []string{}
	for n := range e.owned {
		names = append(names, n)
	}
	for n := range e.workspaces {
		ws = append(ws, n)
	}
	e.mu.Unlock()
	errs := []string{}
	for _, name := range names {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_, err := e.Client.DeleteTask(e.auth(ctx), &v1alpha1.DeleteTaskRequest{Atespace: e.Config.Space, Name: name})
		cancel()
		if err != nil && status.Code(err) != codes.NotFound {
			errs = append(errs, err.Error())
		}
	}
	for _, name := range ws {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := e.Client.DeleteWorkspace(e.auth(ctx), &v1alpha1.DeleteWorkspaceRequest{Atespace: e.Config.Space, Name: name})
		cancel()
		if err != nil && status.Code(err) != codes.NotFound {
			errs = append(errs, err.Error())
		}
	}
	e.Broker.Event("workflow", "cleanup", fmt.Sprintf("Deleted %d owned tasks and %d workspaces", len(names), len(ws)), nil)
	if len(errs) > 0 {
		return fmt.Errorf("cleanup: %s", strings.Join(errs, "; "))
	}
	return nil
}
func Transient(err error) bool {
	c := status.Code(err)
	return c == codes.Unavailable || c == codes.ResourceExhausted
}
