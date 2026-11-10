package webserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// Every handler that writes a node from a body answers 400 to a body that does not bind and 401 without a caller, before the service is asked; the actions without a body answer 401 without a caller.
func TestNodeWriteHandlers_BadBodyAndNoCaller(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := APIConfig{} // no service: none of these paths may reach it
	withBody := map[string]gin.HandlerFunc{
		"createNode":        createNode(cfg),
		"updateNode":        updateNode(cfg),
		"requestNodeChange": requestNodeChange(cfg),
		"reparentNode":      reparentNode(cfg),
		"transferNodeOwner": transferNodeOwner(cfg),
		"setNodeAttributes": setNodeAttributes(cfg),
		"setNodeAllocation": setNodeAllocation(cfg),
		"promoteNode":       promoteNode(cfg),
		"approveNode":       approveNode(cfg),
	}
	valid := map[string]string{
		"createNode":        `{"parent_id":"root","kind":"project","reason":"r","limit":{}}`,
		"updateNode":        `{}`,
		"requestNodeChange": `{}`,
		"reparentNode":      `{"new_parent_id":"b"}`,
		"transferNodeOwner": `{"new_owner":"x@x"}`,
		"setNodeAttributes": `{}`,
		"setNodeAllocation": `{"budget_id":"b"}`,
		"promoteNode":       `{"new_parent_id":"b","owner":"x@x","reason":"r"}`,
		"approveNode":       `{"expected_revision":1}`,
	}
	run := func(h gin.HandlerFunc, body string) int {
		rr := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rr)
		c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h(c)
		return rr.Code
	}
	for name, h := range withBody {
		if got := run(h, "{"); got != http.StatusBadRequest {
			t.Errorf("%s: broken JSON answered %d, want 400", name, got)
		}
		if got := run(h, valid[name]); got != http.StatusUnauthorized {
			t.Errorf("%s: no caller answered %d, want 401", name, got)
		}
	}
	for name, h := range map[string]gin.HandlerFunc{
		"requestNodeDeletion":     requestNodeDeletion(cfg),
		"removeNodeExternalGroup": removeNodeExternalGroup(cfg),
		"rejectNode":              rejectNode(cfg),
		"releaseNode":             releaseNode(cfg),
	} {
		if got := run(h, ""); got != http.StatusUnauthorized {
			t.Errorf("%s: no caller answered %d, want 401", name, got)
		}
	}
}
