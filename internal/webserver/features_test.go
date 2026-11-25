package webserver_test

import (
	"net/http"
	"testing"

	"github.com/pfisterer/openstack-management-api/internal/features"
	"github.com/pfisterer/openstack-management-api/internal/webserver"
)

// /v1/config says per caller which feature gates are open: a gate for a group is open to its members only, an unconfigured known gate to everyone.
func TestConfigFeatures(t *testing.T) {
	gates, err := features.Parse(`{"estimator": ["group:root_uni"], "closed": []}`)
	if err != nil {
		t.Fatal(err)
	}
	h := newTestTree(t, mockNodes(), testAccounting).router(func(c *webserver.SetupConfig) { c.API.Features = gates })
	get := func(user string) map[string]bool {
		rr := do(t, h, http.MethodGet, "/v1/config", user, nil)
		assertStatus(t, rr, http.StatusOK)
		var body struct {
			Features map[string]bool `json:"features"`
		}
		mustDecode(t, rr, &body)
		return body.Features
	}
	root := get("")
	if !root["estimator"] || !root["assistant"] || root["closed"] {
		t.Errorf("root admin (in group:root_uni): %v", root)
	}
	student := get(userStudent)
	if student["estimator"] || !student["assistant"] || student["closed"] {
		t.Errorf("student: %v", student)
	}
}
