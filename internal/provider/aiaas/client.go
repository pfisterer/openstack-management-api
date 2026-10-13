// Package aiaas carries projects of the tree out at the AI service (aiaas-management-api): workspaces with CPU and GPU, and LLM tokens. Like the OpenStack reconciler drives OpenStack's API, it drives the AI service's /v1/tree API; the AI service enforces and books, and reports what was used.
package aiaas

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Provider is the AI service's name in the tree: a leaf's binding under it holds the project's slug there and what was used.
const Provider = "aiaas"

// Member, Budget, Project and Status are the AI service's /v1/tree types.
type Member struct {
	Token string `json:"token"`
	Role  string `json:"role"`
}

type Budget struct {
	Unit     string  `json:"unit"`
	GPUClass string  `json:"gpu_class,omitempty"`
	Window   string  `json:"window"`
	Amount   float64 `json:"amount"`
}

type Project struct {
	Name        string         `json:"name"`
	Members     []Member       `json:"members"`
	EndsAt      *time.Time     `json:"ends_at,omitempty"`
	Budgets     []Budget       `json:"budgets"`
	GPUsByClass map[string]int `json:"gpus_by_class"`
	LLMMTokens  int            `json:"llm_mtokens"`
	// LLMRPM and LLMTPM are the requests and tokens per minute for the whole project; 0 = not set, the AI service's default.
	LLMRPM int `json:"llm_rpm"`
	LLMTPM int `json:"llm_tpm"`
	// LLMModels are the extra model groups the project was granted (availabilities llm-model-<group>); the base models every project has are the AI service's.
	LLMModels []string `json:"llm_models"`
	// StorageGB is the project's workspace storage (homes of its members and their shared folder); 0 = none.
	StorageGB int `json:"storage_gb"`
}

type Use struct {
	Budget
	Used float64 `json:"used"`
}

type Status struct {
	Node string `json:"node"`
	Slug string `json:"slug"`
	Used []Use  `json:"used"`
}

// Client talks to the AI service with the tree's token.
type Client struct {
	base  string
	token string
	http  *http.Client
}

func NewClient(baseURL, token string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, http: &http.Client{Timeout: timeout}}
}

// ErrNotFound is a project the AI service does not have.
var ErrNotFound = fmt.Errorf("aiaas: project not found")

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("aiaas: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return ErrNotFound
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("aiaas: %s %s: HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// List is every project the tree has at the AI service.
func (c *Client) List(ctx context.Context) ([]Status, error) {
	var out []Status
	return out, c.do(ctx, http.MethodGet, "/v1/tree/projects", nil, &out)
}

// Put creates or updates the node's project.
func (c *Client) Put(ctx context.Context, node string, p Project) (Status, error) {
	var out Status
	return out, c.do(ctx, http.MethodPut, "/v1/tree/projects/"+url.PathEscape(node), p, &out)
}

// Delete removes the node's project; ErrNotFound when there is none.
func (c *Client) Delete(ctx context.Context, node string) error {
	return c.do(ctx, http.MethodDelete, "/v1/tree/projects/"+url.PathEscape(node), nil, nil)
}
