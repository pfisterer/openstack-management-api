package estimate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Completer asks a chat model once: a system and a user message, the answer's JSON schema, the answer's text.
type Completer interface {
	Complete(ctx context.Context, system, user string, schema map[string]any) (string, error)
}

// ClientConfig is where the describing model is: an OpenAI-compatible endpoint (the platform's LiteLLM), a key and a model alias. Without URL or key the free-text description is off.
type ClientConfig struct {
	URL            string
	APIKey         string
	Model          string
	TimeoutSeconds int
}

// Enabled reports whether the description is configured.
func (c ClientConfig) Enabled() bool {
	return strings.TrimSpace(c.URL) != "" && strings.TrimSpace(c.APIKey) != ""
}

// OpenAIClient calls POST {URL}/chat/completions.
type OpenAIClient struct {
	cfg  ClientConfig
	http *http.Client
}

func NewOpenAIClient(cfg ClientConfig) *OpenAIClient {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = "chat-large"
	}
	return &OpenAIClient{cfg: cfg, http: &http.Client{Timeout: timeout}}
}

// maxAnswerBytes bounds what is read of an answer: the struct is small, a model that talks on is cut.
const maxAnswerBytes = 1 << 20

func (c *OpenAIClient) Complete(ctx context.Context, system, user string, schema map[string]any) (string, error) {
	body := map[string]any{
		"model":       c.cfg.Model,
		"temperature": 0,
		"max_tokens":  2000,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	}
	if schema != nil {
		body["response_format"] = map[string]any{
			"type":        "json_schema",
			"json_schema": map[string]any{"name": "estimate_input", "schema": schema},
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(c.cfg.URL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("describing model: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return "", fmt.Errorf("describing model: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		// The body of an error may echo the request, never the key; still only its start goes into the error.
		snippet := string(data)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return "", fmt.Errorf("describing model: HTTP %d: %s", resp.StatusCode, snippet)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("describing model: unreadable answer: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("describing model: empty answer")
	}
	return out.Choices[0].Message.Content, nil
}
