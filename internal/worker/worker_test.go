package worker

import (
	"context"
	"encoding/json"
	"example.com/codefleet/internal/model"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyRejectsStaleArtifactAndTraversal(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "api"), 0700)
	os.WriteFile(filepath.Join(root, "api", "pricing.py"), []byte("base"), 0600)
	c := model.Change{Repo: "api", Path: "pricing.py", Before: model.Hash("base"), Content: "new", SHA256: model.Hash("new")}
	if e := Apply(root, c); e != nil {
		t.Fatal(e)
	}
	if e := Apply(root, c); e == nil {
		t.Fatal("stale base accepted")
	}
	for _, p := range []string{"../escape", "/etc/passwd", "a/../x", ".git/config", "a\\b", ""} {
		c.Path = p
		if e := Apply(root, c); e == nil {
			t.Fatalf("unsafe path accepted: %s", p)
		}
	}
	c.Path = "pricing.py"
	c.Before = model.Hash("new")
	c.SHA256 = "tampered"
	if e := Apply(root, c); e == nil {
		t.Fatal("tampered digest accepted")
	}
}
func TestApplyRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "api"), 0700)
	target := filepath.Join(t.TempDir(), "sensitive")
	os.WriteFile(target, []byte("base"), 0600)
	os.Symlink(target, filepath.Join(root, "api", "pricing.py"))
	c := model.Change{Repo: "api", Path: "pricing.py", Before: model.Hash("base"), Content: "new", SHA256: model.Hash("new")}
	if e := Apply(root, c); e == nil {
		t.Fatal("symlink accepted")
	}
}
func TestCompatibleLLMAPI(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "skills"), 0700)
	os.WriteFile(filepath.Join(root, "skills", "python.md"), []byte("Only edit pricing.py"), 0600)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad endpoint or auth")
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": "{\"content\":\"def apply_coupon(): pass\\n\"}"}}}})
	}))
	defer server.Close()
	j := model.Job{Node: model.Definition{Role: "backend", Skill: "python"}, LLM: &model.LLMConfig{Endpoint: server.URL + "/v1", Model: "test-model", APIKey: "test-key"}}
	out, e := generate(context.Background(), root, j, "old")
	if e != nil || out != "def apply_coupon(): pass\n" {
		t.Fatalf("output=%q err=%v", out, e)
	}
}
