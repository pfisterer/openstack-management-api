package webserver_test

// Acting on behalf of another person (X-Act-As). The allowlist is rootAdminTokens, exactly as app.go wires it.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
)

type actAsState struct {
	MayActAs        bool     `json:"may_act_as"`
	Actor           string   `json:"actor"`
	ActingAs        string   `json:"acting_as"`
	EffectiveTokens []string `json:"effective_tokens"`
}

func TestActAs_OnlyAllowlistedUsers(t *testing.T) {
	h := setupRouterSignedInAs(t, userFaculty)

	// A non-root user may ask, and is told no.
	rr := do(t, h, http.MethodGet, "/v1/act-as", "", nil)
	assertStatus(t, rr, http.StatusOK)
	var st actAsState
	mustDecode(t, rr, &st)
	if st.MayActAs || st.Actor != userFaculty || st.ActingAs != "" {
		t.Errorf("faculty: want no right and no switch, got %+v", st)
	}

	// Sending the header anyway is refused, not ignored.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/mine", userStudent, nil), http.StatusForbidden)
	// Naming oneself is no switch.
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/mine", userFaculty, nil), http.StatusOK)
}

func TestActAs_RejectsMalformedHeader(t *testing.T) {
	h := setupRouter(t)
	assertStatus(t, do(t, h, http.MethodGet, "/v1/nodes/mine", "group:root_uni", nil), http.StatusBadRequest)
}

func TestActAs_FaithfulAndRecorded(t *testing.T) {
	h := setupRouter(t)

	rr := do(t, h, http.MethodGet, "/v1/act-as", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	var st actAsState
	mustDecode(t, rr, &st)
	if !st.MayActAs || st.Actor != userRoot || st.ActingAs != userStudent {
		t.Errorf("root acting for the student: got %+v", st)
	}
	for _, tok := range st.EffectiveTokens {
		if tok == "group:root_uni" || tok == "user:"+userRoot {
			t.Errorf("acting for the student must drop root's tokens, got %v", st.EffectiveTokens)
		}
	}

	// Email-scoped views follow the person: "mine" shows the student's leaves (p_002 in the mock seed).
	rr = do(t, h, http.MethodGet, "/v1/nodes/mine", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	mine := decodePage(t, rr)
	if len(mine) != 1 || mine[0].ID != "p_002" {
		t.Errorf("the student owns exactly [p_002], got %v", nodeIDs(mine))
	}

	// Root's own rights are gone: the student cannot decide the pending leaf.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", userStudent, approval(t, h, "p_002", nil)), http.StatusForbidden)

	// A request auto-approves as it would for the student, belongs to them, and the history names the admin who made it.
	rr = do(t, h, http.MethodPost, "/v1/nodes", userStudent, tree.CreateNodeRequest{
		ParentID: "b_cs_students", Kind: model.KindProject, Name: "on behalf",
		Reason: "on behalf", Limit: cores(1),
	})
	assertStatus(t, rr, http.StatusCreated)
	var n model.Node
	mustDecode(t, rr, &n)
	if n.Status != model.StatusApproved {
		t.Errorf("the request should auto-approve, got %q", n.Status)
	}
	if n.Owner != "user:"+userStudent {
		t.Errorf("owner should be the student, got %q", n.Owner)
	}
	if len(n.History) == 0 || n.History[0].Actor != userStudent || n.History[0].ActedBy != userRoot {
		t.Errorf("history should attribute to the student and name root as acting, got %+v", n.History)
	}

	// Without the header root is root again.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/nodes/p_002/approve", "", approval(t, h, "p_002", nil)), http.StatusOK)
}

// Acting on somebody's behalf lists that person's API tokens, not the admin's — but never issues one: a token made that way would be a credential of the person in the admin's hands.
func TestActAs_APITokensAreThePersons(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodPost, "/v1/tokens", userStudent, map[string]any{"description": "for the ticket"})
	assertStatus(t, rr, http.StatusForbidden)
	if !strings.Contains(rr.Body.String(), "acting on behalf") {
		t.Errorf("the refusal should say why, got %s", rr.Body.String())
	}

	var list struct {
		Tokens []struct {
			User string `json:"user"`
		} `json:"tokens"`
	}
	rr = do(t, h, http.MethodGet, "/v1/tokens", userStudent, nil)
	assertStatus(t, rr, http.StatusOK)
	mustDecode(t, rr, &list)
	if len(list.Tokens) != 0 {
		t.Errorf("no token should have been issued for the student, got %+v", list.Tokens)
	}

	// The admin's own tokens are still the admin's to create.
	assertStatus(t, do(t, h, http.MethodPost, "/v1/tokens", "", map[string]any{"description": "mine"}), http.StatusCreated)
	rr = do(t, h, http.MethodGet, "/v1/tokens", "", nil)
	assertStatus(t, rr, http.StatusOK)
	mustDecode(t, rr, &list)
	if len(list.Tokens) != 1 || list.Tokens[0].User != userRoot {
		t.Errorf("the admin should own exactly their token, got %+v", list.Tokens)
	}
}
