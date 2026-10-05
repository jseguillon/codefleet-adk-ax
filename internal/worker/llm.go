package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"example.com/codefleet/internal/model"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A single bounded generation: the model proposes one file, then independent
// task sandboxes run the tests. The model never selects shell commands.
func generate(ctx context.Context, root string, j model.Job, current string) (string, error) {
	baseURL, modelID, apiKey := os.Getenv("OPENAI_BASE_URL"), os.Getenv("OPENAI_MODEL"), os.Getenv("OPENAI_API_KEY")
	if j.LLM != nil {
		baseURL = j.LLM.Endpoint
		modelID = j.LLM.Model
		apiKey = j.LLM.APIKey
	}
	if baseURL == "" || modelID == "" {
		return "", fmt.Errorf("llm mode requires OPENAI_BASE_URL and OPENAI_MODEL")
	}
	skill, e := os.ReadFile(filepath.Join(root, "skills", j.Node.Skill+".md"))
	if e != nil {
		return "", e
	}
	evidence := strings.Builder{}
	for _, r := range j.Inputs {
		evidence.WriteString(r.Summary + "\n")
		for _, check := range r.Checks {
			if !check.Passed {
				evidence.WriteString(check.Log + "\n")
			}
		}
	}
	prompt := fmt.Sprintf("Task: %s\nRole: %s\nSkill:\n%s\nCurrent file:\n%s\nDependency evidence:\n%s\nReturn JSON only: {\"content\":\"entire replacement file\"}. No markdown. Preserve API names. Do not change tests.", j.Goal, j.Node.Role, skill, current, evidence.String())
	body, _ := json.Marshal(map[string]any{"model": modelID, "messages": []map[string]string{{"role": "system", "content": "You are a coding specialist restricted to one file and one skill. Respond with a JSON object containing only content."}, {"role": "user", "content": prompt}}, "temperature": 0.1, "max_tokens": 4096, "stream": false})
	req, e := http.NewRequestWithContext(ctx, "POST", strings.TrimRight(baseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if e != nil {
		return "", e
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := http.Client{Timeout: 150 * time.Second}
	res, e := client.Do(req)
	if e != nil {
		return "", e
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", fmt.Errorf("LLM returned %s", res.Status)
	}
	var response struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&response); e != nil {
		return "", e
	}
	if len(response.Choices) != 1 {
		return "", fmt.Errorf("expected one LLM choice")
	}
	text := strings.TrimSpace(response.Choices[0].Message.Content)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	var patch struct {
		Content string `json:"content"`
	}
	if e = json.Unmarshal([]byte(text), &patch); e != nil {
		return "", fmt.Errorf("invalid model JSON: %w", e)
	}
	if len(patch.Content) == 0 || len(patch.Content) > 256<<10 {
		return "", fmt.Errorf("invalid proposed file size")
	}
	return patch.Content, nil
}
