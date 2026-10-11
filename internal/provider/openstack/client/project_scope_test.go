package osclient

import (
	"testing"

	"github.com/gophercloud/gophercloud"
	"go.uber.org/zap"
)

// The project-scope split exists only for domain-scoped password auth. For
// every other client the accessors must keep returning the primary service
// clients, and EnsureProjectScope must be a silent no-op — otherwise the
// app-credential environments would suddenly try to re-authenticate.
func TestProjectScopeSplitIsInertWithoutDomainScope(t *testing.T) {
	compute := &gophercloud.ServiceClient{}
	network := &gophercloud.ServiceClient{}
	block := &gophercloud.ServiceClient{}
	c := &OpenStackClient{Compute: compute, Network: network, Block: block, log: zap.NewNop().Sugar()}

	if err := c.EnsureProjectScope("11111111111111111111111111111111"); err != nil {
		t.Fatalf("EnsureProjectScope without projectScopeAuth must be a no-op, got %v", err)
	}
	if c.computeSvc() != compute || c.networkSvc() != network || c.blockSvc() != block {
		t.Fatal("accessors must fall back to the primary service clients")
	}
	if c.projectScoped.Load() != nil {
		t.Fatal("no project-scoped services may be built without the auth recipe")
	}
}

// An empty project id must not attempt authentication either — the reconciler
// passes "" when it runs unscoped (no scope parent configured).
func TestEnsureProjectScopeIgnoresEmptyProject(t *testing.T) {
	c := &OpenStackClient{projectScopeAuth: &PasswordAuthOpts{Username: "svc"}, log: zap.NewNop().Sugar()}
	if err := c.EnsureProjectScope(""); err != nil {
		t.Fatalf("empty project id must be a no-op, got %v", err)
	}
	if c.projectScoped.Load() != nil {
		t.Fatal("nothing may be built for an empty project id")
	}
}
