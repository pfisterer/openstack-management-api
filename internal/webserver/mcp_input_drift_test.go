package webserver

import (
	"reflect"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// The MCP tools take hand-written input structs rather than the tree request
// types themselves. That is a deliberate trade — feeding tree.CreateNodeRequest
// straight in would hand a model `kind`, `admin_scope` and `auto_approve` on a
// tool that must not set them, and would push LLM-facing prose into the domain
// types — but it buys a way to drift: a field added to a request type simply
// never reaches the tools, and nothing says so.
//
// This test is what says so. Every field of the domain type must be answered
// for: offered under the same name, offered under a different one, or left out
// with a reason written down. A new field matches none of the three and turns
// this red, which is the point — not to force it into the tool, but to force
// somebody to decide.
type toolInputContract struct {
	tool   string
	domain any
	input  any
	// renamed maps a domain field to the tool's name for it, where the tool can
	// be clearer than the storage layer.
	renamed map[string]string
	// omitted lists what the tool deliberately does not offer, and why.
	omitted map[string]string
}

var toolInputContracts = []toolInputContract{
	{
		tool: "create_project", domain: tree.CreateNodeRequest{}, input: mcpRequestInput{},
		renamed: map[string]string{"parent_id": "budget_id"},
		omitted: map[string]string{
			"kind":                      "the tool sets it; offering it would let a model create a budget through the project tool",
			"authorized_users":          "participants are managed in the UI, where the person can see who they are adding",
			"admin_scope":               "budget-only",
			"eligible_requesters":       "budget-only",
			"auto_approve":              "budget-only",
			"allow_sub_budget_requests": "budget-only",
		},
	},
	{
		tool: "create_budget", domain: tree.CreateNodeRequest{}, input: mcpCreateBudgetInput{},
		renamed: map[string]string{"auto_approve": "auto_approve_limit"},
		omitted: map[string]string{
			"kind":                      "the tool sets it",
			"termination_date":          "budgets are not given an end date through this tool",
			"authorized_users":          "leaf-only",
			"allow_sub_budget_requests": "defaults to allowed; a tool that changes it can be added when somebody wants it",
		},
	},
	{
		tool: "request_project_change", domain: tree.ChangeNodeRequest{}, input: mcpChangeInput{},
		omitted: map[string]string{
			"termination_date": "changing the end date is not offered yet",
			"authorized_users": "participants are managed in the UI",
		},
	},
	{
		tool: "approve_request", domain: tree.ApproveNodeRequest{}, input: mcpApproveInput{},
	},
	{
		tool: "reject_request", domain: tree.RejectNodeRequest{}, input: mcpRejectInput{},
	},
	{
		tool: "move_to_budget", domain: tree.ReparentNodeRequest{}, input: mcpMoveInput{},
		renamed: map[string]string{"new_parent_id": "new_budget_id"},
	},
	{
		tool: "transfer_ownership", domain: tree.TransferOwnerRequest{}, input: mcpTransferInput{},
	},
	{
		tool: "adopt_imported_project", domain: tree.PromoteNodeRequest{}, input: mcpAdoptInput{},
		renamed: map[string]string{"new_parent_id": "new_budget_id"},
		omitted: map[string]string{
			"termination_date": "not offered yet",
			"authorized_users": "participants are managed in the UI",
		},
	},
	{
		tool: "rename_project", domain: tree.UpdateNodeRequest{}, input: mcpRenameInput{},
		omitted: map[string]string{
			"admin_scope":               "changing who may approve is not done by an agent",
			"eligible_requesters":       "changing who may request is not done by an agent",
			"auto_approve":              "granting automatic approval is not done by an agent",
			"clear_auto_approve":        "see auto_approve",
			"allow_sub_budget_requests": "policy, not a value",
			"limit":                     "resources change through request_project_change, which records a reason",
			"termination_date":          "not offered yet",
			"clear_termination_date":    "see termination_date",
		},
	},
}

// jsonFields returns the json names of a struct's exported fields, skipping the
// ones json itself skips.
func jsonFields(v any) map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous {
			// Embedded structs contribute their own fields, which is how
			// mcpSearchInput gets limit/offset.
			for name := range jsonFields(reflect.New(f.Type).Elem().Interface()) {
				out[name] = true
			}
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out[name] = true
	}
	return out
}

func TestMCPToolInputsCoverTheirRequestTypes(t *testing.T) {
	for _, c := range toolInputContracts {
		t.Run(c.tool, func(t *testing.T) {
			domain := jsonFields(c.domain)
			input := jsonFields(c.input)

			for field := range domain {
				switch {
				case c.omitted[field] != "":
					// Deliberately not offered, reason recorded above.
				case c.renamed[field] != "":
					if !input[c.renamed[field]] {
						t.Errorf("%q is mapped to %q, but the tool input has no such field",
							field, c.renamed[field])
					}
				case !input[field]:
					t.Errorf("%s.%s reaches no tool input.\n"+
						"Add it to %s, map it under renamed, or write down under omitted why an agent must not set it.",
						reflect.TypeOf(c.domain).Name(), field, reflect.TypeOf(c.input).Name())
				}
			}

			// A stale entry is its own kind of drift: it claims a decision about
			// a field that no longer exists.
			for field := range c.omitted {
				if !domain[field] {
					t.Errorf("omitted lists %q, which %s no longer has", field, reflect.TypeOf(c.domain).Name())
				}
			}
			for field := range c.renamed {
				if !domain[field] {
					t.Errorf("renamed lists %q, which %s no longer has", field, reflect.TypeOf(c.domain).Name())
				}
			}
		})
	}
}
