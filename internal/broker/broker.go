package broker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"example.com/codefleet/internal/model"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Broker struct {
	mu      sync.Mutex
	Jobs    map[string]model.Job
	Results map[string]model.Result
	Events  []model.Event
	Dir     string
	Status  string
	RunID   string
	Mode    string
	Backend string
	Nodes   []model.Definition
	Done    chan struct{}
}

func New(dir, runID, mode, backend string) (*Broker, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Broker{Jobs: map[string]model.Job{}, Results: map[string]model.Result{}, Dir: dir, RunID: runID, Mode: mode, Backend: backend, Status: "idle", Nodes: model.Definitions(), Done: make(chan struct{})}, nil
}
func (b *Broker) Event(node, kind, msg string, data map[string]any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	e := model.Event{Seq: len(b.Events) + 1, At: time.Now().UTC(), Node: node, Kind: kind, Message: msg, Data: data}
	b.Events = append(b.Events, e)
	f, err := os.OpenFile(filepath.Join(b.Dir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err == nil {
		_ = json.NewEncoder(f).Encode(e)
		_ = f.Close()
	}
	fmt.Printf("%-13s %-13s %s\n", node, kind, msg)
}
func (b *Broker) Register(j model.Job) (model.Job, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.Jobs[j.Task]; ok {
		return model.Job{}, fmt.Errorf("job already registered")
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return j, err
	}
	j.Token = hex.EncodeToString(buf)
	b.Jobs[j.Task] = j
	return j, nil
}
func (b *Broker) GetResult(id string) (model.Result, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.Results[id]
	return r, ok
}
func (b *Broker) GetJob(id string) (model.Job, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	r, ok := b.Jobs[id]
	return r, ok
}
func (b *Broker) Finish(status string) {
	b.mu.Lock()
	b.Status = status
	b.mu.Unlock()
	b.Event("workflow", status, "ADK graph "+status, nil)
	_ = b.Save()
	close(b.Done)
}
func (b *Broker) Snapshot() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	return map[string]any{"run_id": b.RunID, "mode": b.Mode, "backend": b.Backend, "status": b.Status, "nodes": b.Nodes, "events": append([]model.Event{}, b.Events...), "results": cloneResults(b.Results)}
}
func cloneResults(m map[string]model.Result) map[string]model.Result {
	n := map[string]model.Result{}
	for k, v := range m {
		n[k] = v
	}
	return n
}
func (b *Broker) Save() error {
	p, err := json.MarshalIndent(b.Snapshot(), "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.Dir, "report.json"), p, 0600)
}
func (b *Broker) Handler(web []byte) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(web)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, b.Snapshot()) })
	mux.HandleFunc("/jobs/", func(w http.ResponseWriter, r *http.Request) {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/jobs/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, r)
			return
		}
		id, action := parts[0], parts[1]
		j, ok := b.GetJob(id)
		if !ok || r.Header.Get("Authorization") != "Bearer "+j.Token {
			http.Error(w, "unauthorized", 401)
			return
		}
		switch {
		case r.Method == "GET" && action == "input":
			writeJSON(w, j)
		case r.Method == "POST" && action == "event":
			var e model.Event
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&e); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			b.Event(j.Node.ID, e.Kind, e.Message, e.Data)
			w.WriteHeader(204)
		case r.Method == "POST" && action == "result":
			var res model.Result
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&res); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if res.Task != id || res.Node != j.Node.ID {
				http.Error(w, "identity mismatch", 400)
				return
			}
			for _, c := range res.Changes {
				if err := model.ValidPath(c.Repo, c.Path); err != nil || c.SHA256 != model.Hash(c.Content) {
					http.Error(w, "invalid artifact", 400)
					return
				}
				found := false
				for _, repo := range j.Node.Repos {
					if repo == c.Repo {
						found = true
					}
				}
				if !found {
					http.Error(w, "repo outside task scope", 400)
					return
				}
			}
			b.mu.Lock()
			_, exists := b.Results[id]
			if !exists {
				b.Results[id] = res
			}
			b.mu.Unlock()
			if exists {
				http.Error(w, "immutable result already published", 409)
				return
			}
			b.Event(j.Node.ID, "result", res.Summary, map[string]any{"success": res.Success, "task": id, "llm_calls": res.LLMCalls})
			w.WriteHeader(204)
		default:
			http.Error(w, "unsupported operation", 405)
		}
	})
	return mux
}
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
