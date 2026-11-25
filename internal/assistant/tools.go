package assistant

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Toolbox is what the agent may call: the tools it is shown and the call itself. The real one is the service's own MCP server, built for the calling person (MCPToolbox); the tests bring their own.
type Toolbox interface {
	Tools(ctx context.Context) ([]ToolDef, error)
	// Call runs a tool with the model's arguments; the text is what goes back to the model, isError says the tool refused or failed (a normal outcome the model may react to), err is a failure of the toolbox itself.
	Call(ctx context.Context, name string, arguments json.RawMessage) (text string, isError bool, err error)
	Close() error
}

// ReadTools are the tools of stage 1 (read only): the MCP tools that change nothing. A tool not on this list is neither shown to the model nor called, whatever the model asks for; the MCP server behind it is moreover built for a read-only caller, so no tool that writes even exists there (mcpserve.AddTool with mutates). Two locks, each enough on its own. plan_project_split and estimate_resources compute and write nothing.
var ReadTools = []string{
	"list_my_projects",
	"list_my_budgets",
	"get_project",
	"search_projects",
	"list_requestable_budgets",
	"list_requests_to_decide",
	"list_budget_contents",
	"plan_project_split",
	"estimate_options",
	"estimate_resources",
}

// MCPToolbox talks to an MCP server in the same process over an in-memory connection: the same tools, schemas, rights checks and error texts as /mcp, without a network hop or a token of its own. The server is built per request for the calling person, so the tools act as that person and only as them.
type MCPToolbox struct {
	session *mcp.ClientSession
	server  *mcp.ServerSession
	allow   []string
}

// NewMCPToolbox connects to s and offers only the tools in allow.
func NewMCPToolbox(ctx context.Context, s *mcp.Server, allow []string) (*MCPToolbox, error) {
	clientSide, serverSide := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, serverSide, nil)
	if err != nil {
		return nil, fmt.Errorf("assistant tools: connect server: %w", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "portal-assistant", Version: "1"}, nil).Connect(ctx, clientSide, nil)
	if err != nil {
		_ = ss.Close()
		return nil, fmt.Errorf("assistant tools: connect client: %w", err)
	}
	return &MCPToolbox{session: cs, server: ss, allow: allow}, nil
}

func (b *MCPToolbox) Tools(ctx context.Context) ([]ToolDef, error) {
	res, err := b.session.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil {
		return nil, fmt.Errorf("assistant tools: list: %w", err)
	}
	var out []ToolDef
	for _, t := range res.Tools {
		if !slices.Contains(b.allow, t.Name) {
			continue
		}
		out = append(out, ToolDef{Name: t.Name, Description: t.Description, Parameters: t.InputSchema})
	}
	slices.SortFunc(out, func(a, b ToolDef) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func (b *MCPToolbox) Call(ctx context.Context, name string, arguments json.RawMessage) (string, bool, error) {
	if !slices.Contains(b.allow, name) {
		return "", true, fmt.Errorf("tool %q is not offered", name)
	}
	var args map[string]any
	if len(arguments) > 0 && string(arguments) != "null" {
		if err := json.Unmarshal(arguments, &args); err != nil {
			return "The arguments are not a JSON object: " + err.Error(), true, nil
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	res, err := b.session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// A protocol error, e.g. arguments that do not fit the schema: the model can correct it.
		return err.Error(), true, nil
	}
	if res.StructuredContent != nil && !res.IsError {
		raw, err := json.Marshal(res.StructuredContent)
		if err == nil {
			return string(raw), false, nil
		}
	}
	var parts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, t.Text)
		}
	}
	return strings.Join(parts, "\n"), res.IsError, nil
}

func (b *MCPToolbox) Close() error {
	err := b.session.Close()
	_ = b.server.Close()
	return err
}
