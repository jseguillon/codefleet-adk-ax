package broker

import (
	"bytes"
	"encoding/json"
	"example.com/codefleet/internal/model"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestJobAuthenticationAndImmutableResult(t *testing.T) {
	b, e := New(t.TempDir(), "test", "fixture", "mock")
	if e != nil {
		t.Fatal(e)
	}
	j, e := b.Register(model.Job{Task: "task", Node: model.Definition{ID: "backend", Repos: []string{"api"}}})
	if e != nil {
		t.Fatal(e)
	}
	handler := b.Handler(nil)
	req := httptest.NewRequest("GET", "/jobs/task/input", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != 401 {
		t.Fatal("unauthenticated job visible")
	}
	post := func(r model.Result) int {
		p, _ := json.Marshal(r)
		req := httptest.NewRequest(http.MethodPost, "/jobs/task/result", bytes.NewReader(p))
		req.Header.Set("Authorization", "Bearer "+j.Token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		return w.Code
	}
	bad := model.Result{Task: "task", Node: "backend", Changes: []model.Change{{Repo: "web", Path: "coupon.mjs", Content: "x", SHA256: model.Hash("x")}}}
	if post(bad) != 400 {
		t.Fatal("out-of-scope artifact accepted")
	}
	good := model.Result{Task: "task", Node: "backend", Success: true}
	if post(good) != 204 || post(good) != 409 {
		t.Fatal("result publication is not immutable")
	}
}
