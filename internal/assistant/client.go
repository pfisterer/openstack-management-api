package assistant

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/estimate"
)

// ClientConfig is where the model is, in the shape the estimator uses (an OpenAI-compatible endpoint, the platform's LiteLLM, a key, a model alias, a timeout per call): one shape for both, so the deployment configures them alike.
type ClientConfig = estimate.ClientConfig

// Message is one message of a chat in the OpenAI form: system, user, assistant (with the tool calls it made) or tool (the result of one call).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is a call the model asks for; Arguments is the JSON text it wrote.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// ToolDef is a tool offered to the model: name, description and the JSON schema of its arguments (the MCP tool's input schema as it is).
type ToolDef struct {
	Name        string
	Description string
	Parameters  any
}

// Reply is the model's answer to one turn: text, or tool calls, or both, and what the turn cost.
type Reply struct {
	Content          string
	ToolCalls        []ToolCall
	PromptTokens     int
	CompletionTokens int
}

// ChatModel is one turn of a chat with tools. The OpenAI client below is the real one; the tests script one.
type ChatModel interface {
	Chat(ctx context.Context, messages []Message, tools []ToolDef) (Reply, error)
}

// OpenAIClient calls POST {URL}/chat/completions with tools (tool_choice auto).
type OpenAIClient struct {
	cfg       ClientConfig
	http      *http.Client
	maxTokens int
}

// NewOpenAIClient is the client for cfg; maxTokens bounds one answer of the model.
func NewOpenAIClient(cfg ClientConfig, maxTokens int) *OpenAIClient {
	timeout := time.Duration(cfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = "assistant"
	}
	if maxTokens <= 0 {
		maxTokens = 1500
	}
	return &OpenAIClient{cfg: cfg, http: &http.Client{Timeout: timeout}, maxTokens: maxTokens}
}

// Model is the alias asked.
func (c *OpenAIClient) Model() string { return c.cfg.Model }

// maxAnswerBytes bounds what is read of an answer.
const maxAnswerBytes = 1 << 20

func (c *OpenAIClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (Reply, error) {
	body := map[string]any{
		"model":       c.cfg.Model,
		"temperature": 0,
		"max_tokens":  c.maxTokens,
		"messages":    messages,
	}
	if len(tools) > 0 {
		defs := make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			defs = append(defs, map[string]any{"type": "function", "function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Parameters}})
		}
		body["tools"] = defs
		body["tool_choice"] = "auto"
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Reply{}, err
	}
	url := strings.TrimRight(c.cfg.URL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return Reply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("User-Agent", "dhbw-portal-assistant/1")
	resp, err := c.http.Do(req)
	if err != nil {
		return Reply{}, fmt.Errorf("assistant model: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAnswerBytes))
	if err != nil {
		return Reply{}, fmt.Errorf("assistant model: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		// The body of an error may echo the request, never the key; still only its start goes into the error.
		snippet := string(data)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return Reply{}, fmt.Errorf("assistant model: HTTP %d: %s", resp.StatusCode, snippet)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content   *string    `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Reply{}, fmt.Errorf("assistant model: unreadable answer: %w", err)
	}
	if len(out.Choices) == 0 {
		return Reply{}, errors.New("assistant model: empty answer")
	}
	m := out.Choices[0].Message
	r := Reply{ToolCalls: m.ToolCalls, PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens}
	if m.Content != nil {
		r.Content = *m.Content
	}
	return r, nil
}

var (
	thinkBlock    = regexp.MustCompile(`(?s)<think>.*?</think>`)
	textToolCall  = regexp.MustCompile(`(?s)<tool_call>\s*(\{.*?\})\s*</tool_call>`)
	unclosedThink = regexp.MustCompile(`(?s)^.*</think>`)
	// The XML form of qwen3_coder (Qwen3.6 on a vLLM without --tool-call-parser, as ZWR-KI-VLLM-LARGE sent it until 24.11.2026).
	xmlToolCall  = regexp.MustCompile(`(?s)<tool_call>\s*<function=([^>\s]+)>(.*?)</function>\s*</tool_call>`)
	xmlParameter = regexp.MustCompile(`(?s)<parameter=([^>\s]+)>\n?(.*?)\n?</parameter>`)
)

// normalizeReply cleans what a Qwen model behind vLLM or Ollama may send instead of the OpenAI form: its reasoning in <think> (dropped, it is not for the person), and a tool call written as text when the backend's parser did not take it, as JSON (<tool_call>{"name":…,"arguments":{…}}</tool_call>) or in the XML of qwen3_coder (<tool_call><function=…><parameter=…>…</parameter></function></tool_call>) (read as a real call, so a misconfigured backend degrades instead of answering with markup).
func normalizeReply(r Reply) Reply {
	c := thinkBlock.ReplaceAllString(r.Content, "")
	c = unclosedThink.ReplaceAllString(c, "")
	if len(r.ToolCalls) == 0 {
		for i, m := range textToolCall.FindAllStringSubmatch(c, -1) {
			var tc struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if json.Unmarshal([]byte(m[1]), &tc) != nil || tc.Name == "" {
				continue
			}
			call := ToolCall{ID: fmt.Sprintf("text-%d", i), Type: "function"}
			call.Function.Name = tc.Name
			call.Function.Arguments = string(tc.Arguments)
			if len(tc.Arguments) == 0 {
				call.Function.Arguments = "{}"
			}
			r.ToolCalls = append(r.ToolCalls, call)
		}
		for i, m := range xmlToolCall.FindAllStringSubmatch(c, -1) {
			args := map[string]any{}
			for _, p := range xmlParameter.FindAllStringSubmatch(m[2], -1) {
				// A value that is JSON (a number, an object, a list, true) is taken as such, anything else as text.
				var v any
				if json.Unmarshal([]byte(strings.TrimSpace(p[2])), &v) == nil {
					args[p[1]] = v
				} else {
					args[p[1]] = p[2]
				}
			}
			raw, _ := json.Marshal(args)
			call := ToolCall{ID: fmt.Sprintf("xml-%d", i), Type: "function"}
			call.Function.Name = m[1]
			call.Function.Arguments = string(raw)
			r.ToolCalls = append(r.ToolCalls, call)
		}
		if len(r.ToolCalls) > 0 {
			c = textToolCall.ReplaceAllString(c, "")
			c = xmlToolCall.ReplaceAllString(c, "")
		}
	}
	r.Content = strings.TrimSpace(c)
	return r
}
