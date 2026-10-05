package flow

import (
	"context"
	"encoding/json"
	"example.com/codefleet/internal/axclient"
	"example.com/codefleet/internal/model"
	"fmt"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/workflow"
	"google.golang.org/genai"
	"time"
)

func Merge(input any) (model.Packet, error) {
	if input == nil {
		return model.Packet{}, nil
	}
	if _, ok := input.(string); ok {
		return model.Packet{}, nil
	}
	if p, ok := input.(model.Packet); ok {
		return p, nil
	}
	if m, ok := input.(map[string]any); ok {
		// Stable order at fan-in is essential for replayable artifact merging.
		keys := []string{"backend", "frontend", "docs", "unit", "security", "retest", "resecure"}
		out := model.Packet{}
		seen := map[string]bool{}
		for _, k := range keys {
			v, ok := m[k]
			if !ok {
				continue
			}
			p, e := Merge(v)
			if e != nil {
				return out, e
			}
			for _, id := range p.Tasks {
				if !seen[id] {
					seen[id] = true
					out.Tasks = append(out.Tasks, id)
				}
			}
		}
		if len(out.Tasks) > 0 {
			return out, nil
		}
	}
	p, _ := json.Marshal(input)
	var packet model.Packet
	if e := json.Unmarshal(p, &packet); e != nil {
		return packet, e
	}
	return packet, nil
}
func Build(ex *axclient.Executor) (*workflow.Workflow, error) {
	defs := model.Definitions()
	nodes := map[string]workflow.Node{}
	retry := workflow.DefaultRetryConfig()
	retry.MaxAttempts = 3
	retry.InitialDelay = 100 * time.Millisecond
	retry.MaxDelay = time.Second
	retry.Jitter = 0
	retry.ShouldRetry = axclient.Transient
	for _, d := range defs {
		if d.ID == "review" || d.ID == "review_final" {
			continue
		}
		def := d
		nodes[d.ID] = workflow.NewFunctionNode(d.ID, func(ctx agent.Context, input any) (model.Packet, error) {
			p, e := Merge(input)
			if e != nil {
				return p, e
			}
			out, e := ex.Run(ctx, def, p)
			if e == nil {
				ex.Broker.Event(def.ID, "completed", "ADK node completed", nil)
			}
			return out, e
		}, workflow.NodeConfig{RetryConfig: retry, Timeout: 4 * time.Minute})
	}
	for _, id := range []string{"review", "review_final"} {
		var def model.Definition
		for _, d := range defs {
			if d.ID == id {
				def = d
			}
		}
		nodes[id] = workflow.NewFunctionNode(id, func(ctx agent.Context, input any) (*session.Event, error) {
			p, e := Merge(input)
			if e != nil {
				return nil, e
			}
			p, e = ex.Run(ctx, def, p)
			if e != nil {
				return nil, e
			}
			r, _ := ex.Broker.GetResult(p.Tasks[len(p.Tasks)-1])
			passed := true
			for _, c := range r.Checks {
				if !c.Passed {
					passed = false
				}
			}
			route := "approve"
			if !passed {
				if def.ID == "review_final" {
					return nil, fmt.Errorf("repair exhausted: fresh review still fails")
				}
				route = "repair"
			}
			ev := session.NewEvent(ctx, ctx.InvocationID())
			ev.Output = p
			ev.Routes = []string{route}
			ex.Broker.Event(def.ID, route, r.Summary, nil)
			return ev, nil
		}, workflow.NodeConfig{RetryConfig: retry, Timeout: 4 * time.Minute})
	}
	writers := workflow.NewJoinNode("join_writers")
	qa := workflow.NewJoinNode("join_qa")
	qa2 := workflow.NewJoinNode("join_recheck")
	b := workflow.NewEdgeBuilder().Add(workflow.Start, nodes["plan"]).Add(nodes["plan"], nodes["contract"])
	b.AddFanOut(nodes["contract"], nodes["backend"], nodes["frontend"], nodes["docs"]).AddFanIn(writers, nodes["backend"], nodes["frontend"], nodes["docs"]).Add(writers, nodes["integrate"])
	b.AddFanOut(nodes["integrate"], nodes["unit"], nodes["security"]).AddFanIn(qa, nodes["unit"], nodes["security"]).Add(qa, nodes["review"])
	b.AddRoute(nodes["review"], nodes["repair"], workflow.StringRoute("repair")).AddRoute(nodes["review"], nodes["handoff"], workflow.StringRoute("approve"))
	b.AddFanOut(nodes["repair"], nodes["retest"], nodes["resecure"]).AddFanIn(qa2, nodes["retest"], nodes["resecure"]).Add(qa2, nodes["review_final"]).AddRoute(nodes["review_final"], nodes["handoff"], workflow.StringRoute("approve"))
	return workflow.New("codefleet", b.Build(), workflow.WithMaxConcurrency(4))
}
func Run(ctx context.Context, ex *axclient.Executor) (model.Packet, error) {
	w, e := Build(ex)
	if e != nil {
		return model.Packet{}, e
	}
	a, e := agent.New(agent.Config{Name: "distributed_code", Description: "ADK graph dispatching repo-scoped AX coding tasks", Run: w.Run})
	if e != nil {
		return model.Packet{}, e
	}
	r, e := runner.NewInMemory("codefleet", a)
	if e != nil {
		return model.Packet{}, e
	}
	var out model.Packet
	for ev, err := range r.Run(ctx, "demo", ex.Broker.RunID, genai.NewContentFromText(ex.Config.Goal, genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			return out, err
		}
		if ev != nil && ev.Output != nil {
			if p, err := Merge(ev.Output); err == nil && len(p.Tasks) > 0 {
				out = p
			}
		}
	}
	if len(out.Tasks) == 0 {
		return out, fmt.Errorf("graph returned no final artifacts")
	}
	last, _ := ex.Broker.GetResult(out.Tasks[len(out.Tasks)-1])
	if last.Node != "handoff" {
		return out, fmt.Errorf("graph did not reach release handoff")
	}
	return out, nil
}
