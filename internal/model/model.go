package model

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
	"time"
)

type Definition struct {
	ID      string   `json:"id"`
	Role    string   `json:"role"`
	Repos   []string `json:"repos"`
	Skill   string   `json:"skill"`
	Parents []string `json:"parents"`
}
type Change struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Before  string `json:"before"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Log    string `json:"log"`
}
type Result struct {
	Task             string   `json:"task"`
	Node             string   `json:"node"`
	Success          bool     `json:"success"`
	Summary          string   `json:"summary"`
	Changes          []Change `json:"changes"`
	Checks           []Check  `json:"checks"`
	ReusedCheckpoint bool     `json:"reused_checkpoint"`
	DurationMS       int64    `json:"duration_ms"`
	LLMCalls         int      `json:"llm_calls"`
	Error            string   `json:"error,omitempty"`
}
type LLMConfig struct {
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key,omitempty"`
}
type Job struct {
	Task      string     `json:"task"`
	Node      Definition `json:"node"`
	Goal      string     `json:"goal"`
	Mode      string     `json:"mode"`
	InjectBug bool       `json:"inject_bug"`
	DelayMS   int        `json:"delay_ms"`
	Inputs    []Result   `json:"inputs"`
	LLM       *LLMConfig `json:"llm,omitempty"`
	Token     string     `json:"-"`
}
type Event struct {
	Seq     int            `json:"seq"`
	At      time.Time      `json:"at"`
	Node    string         `json:"node"`
	Kind    string         `json:"kind"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}
type Packet struct {
	Tasks []string `json:"tasks"`
}

func Hash(s string) string { v := sha256.Sum256([]byte(s)); return hex.EncodeToString(v[:]) }
func Key(c Change) string  { return c.Repo + "/" + c.Path }
func ValidPath(repo, p string) error {
	if repo != "api" && repo != "web" && repo != "docs" {
		return fmt.Errorf("unknown repo %q", repo)
	}
	if p == "" || path.IsAbs(p) || path.Clean(p) != p || strings.Contains(p, "\\") || strings.HasPrefix(p, "../") || p == ".." || strings.Contains(p, "\x00") {
		return fmt.Errorf("unsafe path %q", p)
	}
	for _, s := range strings.Split(p, "/") {
		if strings.HasPrefix(s, ".") {
			return fmt.Errorf("hidden path %q", p)
		}
	}
	return nil
}
func Definitions() []Definition {
	return []Definition{
		{"plan", "planner", nil, "planning", nil},
		{"contract", "contract", []string{"api"}, "contract", []string{"plan"}},
		{"backend", "backend", []string{"api"}, "python", []string{"contract"}},
		{"frontend", "frontend", []string{"web"}, "javascript", []string{"contract"}},
		{"docs", "docs", []string{"docs"}, "documentation", []string{"contract"}},
		{"integrate", "integrator", []string{"api", "web", "docs"}, "integration", []string{"backend", "frontend", "docs"}},
		{"unit", "tester", []string{"api", "web"}, "testing", []string{"integrate"}},
		{"security", "security", []string{"api", "web"}, "security", []string{"integrate"}},
		{"review", "reviewer", nil, "review", []string{"unit", "security"}},
		{"repair", "repair", []string{"api"}, "python", []string{"review"}},
		{"retest", "tester", []string{"api", "web"}, "testing", []string{"repair"}},
		{"resecure", "security", []string{"api", "web"}, "security", []string{"repair"}},
		{"review_final", "reviewer", nil, "review", []string{"retest", "resecure"}},
		{"handoff", "release", nil, "release", []string{"review", "review_final"}},
	}
}
