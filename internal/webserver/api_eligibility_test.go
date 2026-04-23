package webserver_test

// Eligibility rules in DefaultMockResourceState:
//
//	group:root_uni      → eligible: [group:root_uni, user:root.admin@uni.example, group:dept_cs_admin, group:dept_bio]
//	group:dept_cs_admin → eligible: [group:dept_cs_faculty, user:faculty@cs.example, user:admin@cs.example]
//	group:dept_cs_faculty → eligible: [group:cs-student, user:student@cs.example]
//	group:dept_bio      → eligible: [group:dept_bio, user:faculty@bio.example]
//
// SetEligibilityRule requires the caller to hold ownerToken in their effective token set.

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// ── GET /v1/eligibility ───────────────────────────────────────────────────────

func TestListEligibility_ReturnsRulesOwnedByCallerTokens(t *testing.T) {
	h := setupRouter(t)
	// faculty@cs.example holds group:dept_cs_faculty → owns the dept_cs_faculty rule
	rr := do(t, h, http.MethodGet, "/v1/eligibility", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var rules []common.TokenEligibilityRule
	mustDecode(t, rr, &rules)

	found := false
	for _, r := range rules {
		if r.OwnerToken == "group:dept_cs_faculty" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("faculty should see the rule for group:dept_cs_faculty; got %v", ruleOwners(rules))
	}
}

func TestListEligibility_DoesNotReturnOtherUsersRules(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/eligibility", userFaculty, nil)
	assertStatus(t, rr, http.StatusOK)

	var rules []common.TokenEligibilityRule
	mustDecode(t, rr, &rules)

	for _, r := range rules {
		if r.OwnerToken == "group:dept_bio" {
			t.Errorf("faculty should not see the dept_bio rule (owned by bio faculty)")
		}
		if r.OwnerToken == "group:root_uni" {
			t.Errorf("faculty should not see the root_uni rule (owned by root admin)")
		}
	}
}

func TestListEligibility_RootAdminSeesRootRule(t *testing.T) {
	h := setupRouter(t)
	rr := do(t, h, http.MethodGet, "/v1/eligibility", userRoot, nil)
	assertStatus(t, rr, http.StatusOK)

	var rules []common.TokenEligibilityRule
	mustDecode(t, rr, &rules)

	found := false
	for _, r := range rules {
		if r.OwnerToken == "group:root_uni" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("root admin should see rule for group:root_uni; got %v", ruleOwners(rules))
	}
}

// ── PUT /v1/eligibility/:token ────────────────────────────────────────────────

func TestSetEligibilityRule_OwnerCanCreateRule(t *testing.T) {
	h := setupRouter(t)
	// faculty owns group:dept_cs_faculty; they may set/replace the rule for it
	body := map[string]any{
		"eligible_requesters": []string{"group:cs-student", "user:newuser@cs.example"},
	}
	rr := do(t, h, http.MethodPut, "/v1/eligibility/group:dept_cs_faculty", userFaculty, body)
	assertStatus(t, rr, http.StatusOK)

	var rule common.TokenEligibilityRule
	mustDecode(t, rr, &rule)
	if rule.OwnerToken != "group:dept_cs_faculty" {
		t.Errorf("expected owner_token=group:dept_cs_faculty, got %q", rule.OwnerToken)
	}
	// Verify the new requester list is stored
	found := false
	for _, r := range rule.EligibleRequesters {
		if r == "user:newuser@cs.example" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("updated rule should contain user:newuser@cs.example; got %v", rule.EligibleRequesters)
	}
}

func TestSetEligibilityRule_NonOwnerGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	// bio faculty does not hold group:dept_cs_faculty → cannot set its rule
	body := map[string]any{
		"eligible_requesters": []string{"user:attacker@evil.example"},
	}
	rr := do(t, h, http.MethodPut, "/v1/eligibility/group:dept_cs_faculty", userBio, body)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestSetEligibilityRule_OwnerCanSetOwnUserToken(t *testing.T) {
	h := setupRouter(t)
	// root admin holds user:root.admin@uni.example → can set rule for that token
	body := map[string]any{
		"eligible_requesters": []string{"group:root_uni"},
	}
	rr := do(t, h, http.MethodPut, "/v1/eligibility/user:root.admin@uni.example", userRoot, body)
	assertStatus(t, rr, http.StatusOK)
}

// ── DELETE /v1/eligibility/:token ─────────────────────────────────────────────

func TestDeleteEligibilityRule_OwnerCanDelete(t *testing.T) {
	h := setupRouter(t)
	// faculty deletes their own rule
	rr := do(t, h, http.MethodDelete, "/v1/eligibility/group:dept_cs_faculty", userFaculty, nil)
	assertStatus(t, rr, http.StatusNoContent)

	// Verify it is gone
	listRR := do(t, h, http.MethodGet, "/v1/eligibility", userFaculty, nil)
	var rules []common.TokenEligibilityRule
	mustDecode(t, listRR, &rules)
	for _, r := range rules {
		if r.OwnerToken == "group:dept_cs_faculty" {
			t.Error("rule for group:dept_cs_faculty should have been deleted")
		}
	}
}

func TestDeleteEligibilityRule_NonOwnerGetsForbidden(t *testing.T) {
	h := setupRouter(t)
	// bio faculty cannot delete a rule they don't own
	rr := do(t, h, http.MethodDelete, "/v1/eligibility/group:dept_cs_faculty", userBio, nil)
	assertStatus(t, rr, http.StatusForbidden)
}

func TestDeleteEligibilityRule_DeletedRuleBlocksEligibleDelegation(t *testing.T) {
	h := setupRouter(t)
	// faculty deletes the dept_cs_faculty eligibility rule.
	// After deletion, students should no longer see dept_cs_faculty as eligible.
	do(t, h, http.MethodDelete, "/v1/eligibility/group:dept_cs_faculty", userFaculty, nil)

	eligRR := do(t, h, http.MethodGet, "/v1/delegations/eligible-for-me", userStudent, nil)
	assertStatus(t, eligRR, http.StatusOK)

	var delegations []common.Delegation
	mustDecode(t, eligRR, &delegations)
	for _, d := range delegations {
		if d.ID == "group:dept_cs_faculty" {
			t.Error("dept_cs_faculty should no longer be eligible for students after rule deletion")
		}
	}
}

// ruleOwners extracts OwnerToken values from a rule slice for error messages.
func ruleOwners(rules []common.TokenEligibilityRule) []string {
	out := make([]string, len(rules))
	for i, r := range rules {
		out[i] = r.OwnerToken
	}
	return out
}
