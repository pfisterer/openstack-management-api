package webserver

import (
	"slices"
	"testing"

	"github.com/pfisterer/cloud-self-service-golib/mcpserve/mcptest"
	"github.com/pfisterer/openstack-management-api/internal/assistant"
	"go.uber.org/zap"
)

// Every tool of the assistant's stage 1 has to be one the MCP server offers a read-only caller, i.e. one registered with mutates=false. A tool on assistant.ReadTools that writes would not exist there (and silently go missing); this says so instead, and a renamed tool turns it red too.
func TestAssistantReadToolsAreReadOnlyTools(t *testing.T) {
	readOnly := mcptest.ToolNames(t, mcptest.InMemory(t, newMCPServer(APIConfig{}, mcpCaller{readOnly: true}, zap.NewNop().Sugar())))
	for _, name := range assistant.ReadTools {
		if !slices.Contains(readOnly, name) {
			t.Errorf("assistant.ReadTools names %q, which a read-only caller is not offered (a write tool, or renamed)", name)
		}
	}
}
