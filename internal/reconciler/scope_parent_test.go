package reconciler

import (
	"errors"
	"testing"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/projects"
	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"go.uber.org/zap"
)

// fakeScopeParentClient implements scopeParentClient for resolveScopeParent tests.
type fakeScopeParentClient struct {
	findResults []*projects.Project // consumed per FindProjectByName call
	findErr     error
	createdID   string
	createErr   error

	findCalls   int
	createCalls int
	createdOpts osclient.ProjectCreateOpts
}

func (f *fakeScopeParentClient) FindProjectByName(name string) (*projects.Project, error) {
	f.findCalls++
	if f.findErr != nil {
		return nil, f.findErr
	}
	if len(f.findResults) == 0 {
		return nil, nil
	}
	result := f.findResults[0]
	f.findResults = f.findResults[1:]
	return result, nil
}

func (f *fakeScopeParentClient) CreateProject(opts osclient.ProjectCreateOpts) (*projects.Project, error) {
	f.createCalls++
	f.createdOpts = opts
	if f.createErr != nil {
		return nil, f.createErr
	}
	return &projects.Project{ID: f.createdID, Name: opts.Name}, nil
}

func testLog() *zap.SugaredLogger { return zap.NewNop().Sugar() }

func TestResolveScopeParent_ExplicitIDWins(t *testing.T) {
	client := &fakeScopeParentClient{}
	cfg := Config{ScopeParentID: "id-123", ScopeParentName: "umbrella"}

	id, err := resolveScopeParent(client, cfg, testLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "id-123" {
		t.Fatalf("expected id-123, got %q", id)
	}
	if client.findCalls != 0 || client.createCalls != 0 {
		t.Fatalf("expected no OpenStack calls, got find=%d create=%d", client.findCalls, client.createCalls)
	}
}

func TestResolveScopeParent_NoScopeConfigured(t *testing.T) {
	client := &fakeScopeParentClient{}

	id, err := resolveScopeParent(client, Config{}, testLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Fatalf("expected empty id, got %q", id)
	}
	if client.findCalls != 0 || client.createCalls != 0 {
		t.Fatalf("expected no OpenStack calls, got find=%d create=%d", client.findCalls, client.createCalls)
	}
}

func TestResolveScopeParent_NameFound(t *testing.T) {
	client := &fakeScopeParentClient{
		findResults: []*projects.Project{{ID: "existing-id", Name: "umbrella"}},
	}
	cfg := Config{ScopeParentName: "umbrella"}

	id, err := resolveScopeParent(client, cfg, testLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "existing-id" {
		t.Fatalf("expected existing-id, got %q", id)
	}
	if client.createCalls != 0 {
		t.Fatalf("expected no create, got %d", client.createCalls)
	}
}

func TestResolveScopeParent_NameMissingCreates(t *testing.T) {
	client := &fakeScopeParentClient{createdID: "new-id"}
	cfg := Config{ScopeParentName: "umbrella"}

	id, err := resolveScopeParent(client, cfg, testLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "new-id" {
		t.Fatalf("expected new-id, got %q", id)
	}
	if client.createCalls != 1 {
		t.Fatalf("expected one create, got %d", client.createCalls)
	}
	if client.createdOpts.Name != "umbrella" {
		t.Fatalf("expected create with name umbrella, got %q", client.createdOpts.Name)
	}
	if len(client.createdOpts.Tags) != 0 {
		t.Fatalf("scope parent must not carry managed tags, got %v", client.createdOpts.Tags)
	}
}

func TestResolveScopeParent_DryRunDoesNotCreate(t *testing.T) {
	client := &fakeScopeParentClient{}
	cfg := Config{ScopeParentName: "umbrella", DryRun: true}

	id, err := resolveScopeParent(client, cfg, testLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Fatalf("expected unscoped result in dry-run, got %q", id)
	}
	if client.createCalls != 0 {
		t.Fatalf("dry-run must not create, got %d creates", client.createCalls)
	}
}

func TestResolveScopeParent_CreateConflictFallsBackToLookup(t *testing.T) {
	// First lookup: not found; create fails (lost race); second lookup finds the winner.
	client := &fakeScopeParentClient{
		findResults: []*projects.Project{nil, {ID: "race-winner", Name: "umbrella"}},
		createErr:   errors.New("409 conflict"),
	}
	cfg := Config{ScopeParentName: "umbrella"}

	id, err := resolveScopeParent(client, cfg, testLog())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "race-winner" {
		t.Fatalf("expected race-winner, got %q", id)
	}
	if client.findCalls != 2 {
		t.Fatalf("expected two lookups, got %d", client.findCalls)
	}
}

func TestResolveScopeParent_LookupErrorPropagates(t *testing.T) {
	client := &fakeScopeParentClient{findErr: errors.New("keystone down")}
	cfg := Config{ScopeParentName: "umbrella"}

	if _, err := resolveScopeParent(client, cfg, testLog()); err == nil {
		t.Fatal("expected error, got nil")
	}
	if client.createCalls != 0 {
		t.Fatalf("expected no create after lookup error, got %d", client.createCalls)
	}
}
