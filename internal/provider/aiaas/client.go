// Package aiaas carries projects of the tree out at the AI service (aiaas-management-api): workspaces with CPU and GPU, and LLM tokens. Like the OpenStack reconciler drives OpenStack's API, it drives the AI service's /v1/tree API; the AI service enforces and books, and reports what was used.
package aiaas

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/httpx"
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
	// Profiles are the server types the project is granted (availabilities ws-profile-<slug>: cpu, cpu-gpukernel, gpu); always sent, empty = none.
	Profiles *[]string `json:"profiles"`
	// StorageGB is the project's workspace storage (homes of its members and their shared folder); 0 = none.
	StorageGB int `json:"storage_gb"`
	// DeleteAt is the day the project's Lab/AI part is deleted (its end plus the grace period), for the warnings in the hub and the self-service UI; nil = not planned.
	DeleteAt *time.Time `json:"delete_at,omitempty"`
	// Workspaces tells whether the project holds anything of the area workspaces: only then it is a workspace project (start form, servers); one with LLM tokens only is not.
	Workspaces bool `json:"workspaces"`
	// Release are the most the project may choose as release times: the strictest release maximums along its chain of budgets, root included (ReleaseFor). Always sent, so they belong to the tree there and nobody sets them at the AI service; a field no budget sets is left out and the AI service keeps its own default for it.
	Release *Release `json:"release"`
	// WorkspaceScratchGB is the local storage (GB) a workspace or GPU kernel of the project may write outside its home: the smallest value along its chain of budgets, root included (ScratchFor); 0 = no budget sets one, the AI service applies its default.
	WorkspaceScratchGB int `json:"workspace_scratch_gb"`
	// BatchPriority is how the project's batch jobs run: "guaranteed" or "preemptible", the strictest value along its chain of budgets, root included (BatchPriorityFor); empty = no budget sets one, the AI service treats it as preemptible.
	BatchPriority string `json:"batch_priority,omitempty" example:"preemptible" enums:"guaranteed,preemptible"`
	// BatchJobs are the project's batch jobs: the strictest values along its chain of budgets, root included (BatchJobsFor); {0, 0} where they have none. Always sent: the tree decides the job limits of its projects at the AI service.
	BatchJobs *BatchJobs `json:"batch_jobs"`
}

// BatchJobs are the AI service's batch jobs from the tree (its workspace.TreeJobs): jobs per person (0 = none) and the longest run time in hours (0 when none).
type BatchJobs struct {
	JobsPerPerson int `json:"jobs_per_person" example:"2"`
	MaxHours      int `json:"max_hours" example:"4"`
}

// ReleaseTimes and Release are the AI service's release times (its workspace.ReleaseTimes and Release): per kind of use, minutes without activity with and without an open tab, and an upper bound in hours (0 = none).
type ReleaseTimes struct {
	IdleOpenMinutes   *int `json:"idle_open_minutes,omitempty"`
	IdleClosedMinutes *int `json:"idle_closed_minutes,omitempty"`
	MaxHours          *int `json:"max_hours,omitempty"`
}

type Release struct {
	CPU ReleaseTimes `json:"cpu"`
	GPU ReleaseTimes `json:"gpu"`
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

// NewClient is a client with the time limit timeout per call, on golib's retrying transport: a name that does not resolve is tried again for any call, a connection that broke before the answer for a GET (three attempts, 1 s and 2 s apart, within timeout).
func NewClient(baseURL, token string, timeout time.Duration) *Client {
	return &Client{base: strings.TrimRight(baseURL, "/"), token: token, http: httpx.RetryingClient(timeout)}
}

// ErrNotFound is a project the AI service does not have.
var ErrNotFound = fmt.Errorf("aiaas: project not found")

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	status, err := httpx.Do(ctx, c.http, httpx.Request{
		Method: method,
		URL:    c.base + path,
		Header: http.Header{"Authorization": {"Bearer " + c.token}},
		Body:   in,
	}, out)
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		if se.Status == http.StatusNotFound {
			return ErrNotFound
		}
		return fmt.Errorf("aiaas: %s %s: HTTP %d: %s", method, path, se.Status, strings.TrimSpace(se.Body))
	}
	if err != nil && status == 0 {
		// No answer: the call itself failed.
		return fmt.Errorf("aiaas: %s %s: %w", method, path, err)
	}
	return err // an answer that does not decode
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
