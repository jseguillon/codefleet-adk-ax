// Package mockax emulates AX's public gRPC surface with local processes.
// It is a protocol/lifecycle test service, not a VM security boundary.
package mockax

import (
	"context"
	"example.com/codefleet/internal/broker"
	"example.com/codefleet/internal/model"
	"fmt"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

type actor struct {
	task       *v1alpha1.Task
	cmd        *exec.Cmd
	done       chan struct{}
	root       string
	generation int
}
type Server struct {
	v1alpha1.UnimplementedAXServer
	mu                 sync.Mutex
	actors             map[string]*actor
	workspaces         map[string]*v1alpha1.Workspace
	Base, Seed, Binary string
	Broker             *broker.Broker
	FailCreateNode     string
	failed             bool
	grpc               *grpc.Server
}

func Start(addr, base, seed, binary string, b *broker.Broker, fail string) (*Server, string, error) {
	l, e := net.Listen("tcp", addr)
	if e != nil {
		return nil, "", e
	}
	s := &Server{actors: map[string]*actor{}, workspaces: map[string]*v1alpha1.Workspace{}, Base: base, Seed: seed, Binary: binary, Broker: b, FailCreateNode: fail, grpc: grpc.NewServer()}
	v1alpha1.RegisterAXServer(s.grpc, s)
	go s.grpc.Serve(l)
	return s, l.Addr().String(), nil
}
func clone(t *v1alpha1.Task) *v1alpha1.Task { return proto.Clone(t).(*v1alpha1.Task) }
func key(space, name string) string         { return space + "/" + name }
func (s *Server) UpdateWorkspace(ctx context.Context, r *v1alpha1.UpdateWorkspaceRequest) (*v1alpha1.Workspace, error) {
	w := r.GetWorkspace()
	if w.GetMetadata().GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "name required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaces[key(w.Metadata.Atespace, w.Metadata.Name)] = proto.Clone(w).(*v1alpha1.Workspace)
	return proto.Clone(w).(*v1alpha1.Workspace), nil
}
func (s *Server) GetWorkspace(ctx context.Context, r *v1alpha1.GetWorkspaceRequest) (*v1alpha1.Workspace, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workspaces[key(r.Atespace, r.Name)]
	if !ok {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	return proto.Clone(w).(*v1alpha1.Workspace), nil
}
func (s *Server) DeleteWorkspace(ctx context.Context, r *v1alpha1.DeleteWorkspaceRequest) (*v1alpha1.DeleteWorkspaceResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.workspaces, key(r.Atespace, r.Name))
	return &v1alpha1.DeleteWorkspaceResponse{}, nil
}
func (s *Server) CreateTask(ctx context.Context, r *v1alpha1.CreateTaskRequest) (*v1alpha1.Task, error) {
	t := r.GetTask()
	name := t.GetMetadata().GetName()
	if name == "" || strings.ContainsAny(name, "/\\.") {
		return nil, status.Error(codes.InvalidArgument, "invalid name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key(t.Metadata.Atespace, name)
	if _, ok := s.actors[k]; ok {
		return nil, status.Error(codes.FailedPrecondition, "task exists and is immutable")
	}
	j, ok := s.Broker.GetJob(name)
	if !ok {
		return nil, status.Error(codes.InvalidArgument, "mock only executes registered codefleet workers")
	}
	if j.Node.ID == s.FailCreateNode && !s.failed {
		s.failed = true
		s.Broker.Event(j.Node.ID, "retry", "Injected gRPC Unavailable before task creation", nil)
		return nil, status.Error(codes.Unavailable, "injected transient mock fault")
	}
	a := &actor{task: clone(t), root: filepath.Join(s.Base, name)}
	a.task.Metadata.CreationTimestamp = timestamppb.Now()
	a.task.Status = &v1alpha1.TaskStatus{Phase: "Pending", Actor: name, Id: name}
	s.actors[k] = a
	for _, ref := range t.Spec.Workspaces {
		if _, ok := s.workspaces[key(t.Metadata.Atespace, ref.Name)]; !ok {
			delete(s.actors, k)
			return nil, status.Error(codes.FailedPrecondition, "workspace absent")
		}
	}
	if e := s.prepare(a, j); e != nil {
		delete(s.actors, k)
		return nil, status.Error(codes.Internal, e.Error())
	}
	if e := s.launch(a, j); e != nil {
		delete(s.actors, k)
		return nil, status.Error(codes.Internal, e.Error())
	}
	return clone(a.task), nil
}
func (s *Server) prepare(a *actor, j model.Job) error {
	if e := os.MkdirAll(a.root, 0700); e != nil {
		return e
	}
	for _, repo := range j.Node.Repos {
		cmd := exec.Command("git", "clone", "--quiet", "--no-hardlinks", filepath.Join(s.Seed, repo), filepath.Join(a.root, repo))
		if p, e := cmd.CombinedOutput(); e != nil {
			return fmt.Errorf("seed clone: %w: %s", e, p)
		}
	}
	skills := filepath.Join(a.root, "skills")
	if e := os.MkdirAll(skills, 0700); e != nil {
		return e
	}
	for _, ref := range a.task.Spec.Workspaces {
		w := s.workspaces[key(a.task.Metadata.Atespace, ref.Name)]
		if strings.Contains(ref.Path, "skills") {
			for _, f := range w.Spec.Files {
				if filepath.Base(f.Path) != f.Path {
					return fmt.Errorf("invalid skill file")
				}
				if e := os.WriteFile(filepath.Join(skills, f.Path), []byte(f.Content), 0600); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func (s *Server) launch(a *actor, j model.Job) error {
	cmd := exec.Command(s.Binary, "worker", "--root", a.root)
	cmd.Env = os.Environ()
	for _, v := range a.task.Spec.Env {
		cmd.Env = append(cmd.Env, v.Name+"="+v.Value)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	f, e := os.OpenFile(filepath.Join(a.root, "worker.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	cmd.Stdout = f
	cmd.Stderr = f
	if e = cmd.Start(); e != nil {
		f.Close()
		return e
	}
	a.cmd = cmd
	a.done = make(chan struct{})
	a.generation++
	generation := a.generation
	done := a.done
	a.task.Status.Phase = "Running"
	a.task.Status.Conditions = []*v1alpha1.Condition{{Type: "WorkspaceReady", Status: "True", LastTransitionTime: timestamppb.Now()}}
	go func() {
		err := cmd.Wait()
		f.Close()
		s.mu.Lock()
		defer s.mu.Unlock()
		if a.generation == generation && a.task.Status.Phase == "Running" && err != nil {
			a.task.Status.Phase = "Failed"
			s.Broker.Event(j.Node.ID, "worker_exit", err.Error(), nil)
		}
		close(done)
	}()
	return nil
}
func (s *Server) GetTask(ctx context.Context, r *v1alpha1.GetTaskRequest) (*v1alpha1.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.actors[key(r.Atespace, r.Name)]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	return clone(a.task), nil
}
func (s *Server) ListTasks(ctx context.Context, r *v1alpha1.ListTasksRequest) (*v1alpha1.ListTasksResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := &v1alpha1.ListTasksResponse{}
	for _, a := range s.actors {
		if a.task.Metadata.Atespace == r.Atespace {
			res.Tasks = append(res.Tasks, clone(a.task))
		}
	}
	return res, nil
}
func (s *Server) SuspendTask(ctx context.Context, r *v1alpha1.SuspendTaskRequest) (*v1alpha1.Task, error) {
	s.mu.Lock()
	a, ok := s.actors[key(r.Atespace, r.Name)]
	if !ok {
		s.mu.Unlock()
		return nil, status.Error(codes.NotFound, "task not found")
	}
	if a.task.Status.Phase != "Running" {
		s.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "task not running")
	}
	a.task.Status.Phase = "Suspended"
	_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
	done := a.done
	s.mu.Unlock()
	select {
	case <-done:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
	return s.GetTask(ctx, &v1alpha1.GetTaskRequest{Atespace: r.Atespace, Name: r.Name})
}
func (s *Server) ResumeTask(ctx context.Context, r *v1alpha1.ResumeTaskRequest) (*v1alpha1.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.actors[key(r.Atespace, r.Name)]
	if !ok {
		return nil, status.Error(codes.NotFound, "task not found")
	}
	if a.task.Status.Phase != "Suspended" {
		return nil, status.Error(codes.FailedPrecondition, "task not suspended")
	}
	j, _ := s.Broker.GetJob(r.Name)
	if e := s.launch(a, j); e != nil {
		return nil, status.Error(codes.Internal, e.Error())
	}
	return clone(a.task), nil
}
func (s *Server) DeleteTask(ctx context.Context, r *v1alpha1.DeleteTaskRequest) (*v1alpha1.DeleteTaskResponse, error) {
	s.mu.Lock()
	a, ok := s.actors[key(r.Atespace, r.Name)]
	if !ok {
		s.mu.Unlock()
		return nil, status.Error(codes.NotFound, "task not found")
	}
	a.task.Status.Phase = "Deleted"
	if a.cmd != nil {
		_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGTERM)
	}
	delete(s.actors, key(r.Atespace, r.Name))
	done := a.done
	s.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-a.cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
	}
	return &v1alpha1.DeleteTaskResponse{}, nil
}
func (s *Server) WatchTask(r *v1alpha1.WatchTaskRequest, stream grpc.ServerStreamingServer[v1alpha1.WatchTaskResponse]) error {
	var last *v1alpha1.Task
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		t, e := s.GetTask(stream.Context(), &v1alpha1.GetTaskRequest{Atespace: r.Atespace, Name: r.Name})
		if e != nil {
			return e
		}
		if last == nil || !proto.Equal(last, t) {
			if e = stream.Send(&v1alpha1.WatchTaskResponse{Task: t, Action: "Updated"}); e != nil {
				return e
			}
			last = t
		}
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
		}
	}
}
func (s *Server) Stop() {
	s.mu.Lock()
	keys := []*v1alpha1.Task{}
	for _, a := range s.actors {
		keys = append(keys, clone(a.task))
	}
	s.mu.Unlock()
	for _, t := range keys {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		s.DeleteTask(ctx, &v1alpha1.DeleteTaskRequest{Atespace: t.Metadata.Atespace, Name: t.Metadata.Name})
		cancel()
	}
	s.grpc.Stop()
}
