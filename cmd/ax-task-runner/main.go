package main

import (
	"context"
	"example.com/codefleet/internal/taskrunner"
	"fmt"
	"github.com/google/ax/pkg/apis/v1alpha1"
	"gopkg.in/yaml.v3"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var t v1alpha1.Task
	if e := yaml.Unmarshal([]byte(os.Getenv("AX_TASK_YAML")), &t); e != nil {
		fatal(e)
	}
	dec := yaml.NewDecoder(strings.NewReader(os.Getenv("AX_WORKSPACES_YAML")))
	ws := []*v1alpha1.Workspace{}
	for {
		var w v1alpha1.Workspace
		e := dec.Decode(&w)
		if e == io.EOF {
			break
		}
		if e != nil {
			fatal(e)
		}
		if w.Metadata != nil {
			ws = append(ws, &w)
		}
	}
	if e := taskrunner.Run(ctx, taskrunner.Config{Task: &t, Workspaces: ws}); e != nil {
		fatal(e)
	}
}
func fatal(e error) { fmt.Fprintln(os.Stderr, e); os.Exit(1) }
