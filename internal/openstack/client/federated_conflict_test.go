package osclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud"
	"go.uber.org/zap"
)

// keystoneStub is the smallest Keystone that findOrCreateFederatedUser needs:
// a user list, a user GET and a user create/delete. It records the deletes,
// which is the whole point of the test below.
type keystoneStub struct {
	// createdID is the id the stub hands out on POST /users — the knob that
	// decides whether the caller sees a match or a conflict.
	createdID string
	deleted   []string
	server    *httptest.Server
}

func newKeystoneStub(t *testing.T, createdID string) *keystoneStub {
	t.Helper()
	stub := &keystoneStub{createdID: createdID}
	mux := http.NewServeMux()

	mux.HandleFunc("/users", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			// No account carries this name — drives the "neither exists" branch.
			_ = json.NewEncoder(w).Encode(map[string]any{"users": []any{}})
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated) // Keystone answers 201; gophercloud insists on it
			_ = json.NewEncoder(w).Encode(map[string]any{
				"user": map[string]any{"id": stub.createdID, "name": "s1@example.edu", "enabled": true},
			})
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/users/", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimPrefix(r.URL.Path, "/users/")
		switch r.Method {
		case http.MethodGet:
			// The derived id is never already present in these tests.
			w.WriteHeader(http.StatusNotFound)
		case http.MethodDelete:
			stub.deleted = append(stub.deleted, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	stub.server = httptest.NewServer(mux)
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *keystoneStub) client() *OpenStackClient {
	log := zap.NewNop()
	return &OpenStackClient{
		Identity:              &gophercloud.ServiceClient{Endpoint: s.server.URL + "/", ProviderClient: &gophercloud.ProviderClient{}},
		logger:                log,
		log:                   log.Sugar(),
		federatedProvisioning: true,
		federatedIdPID:        "keycloak",
		federatedProtocolID:   "openid",
		federatedDomainID:     "default",
	}
}

// A rejected account must not survive the call that rejected it.
//
// This is the regression that mattered: the conflict branch used to leave the
// account in Keystone. It carried the managed description and held no role, so
// the orphan sweep at the end of the same reconcile pass deleted it — and five
// minutes later the next pass created it again under a fresh uuid. The loop ran
// unattended against a live Keystone.
func TestFindOrCreateFederatedUser_RemovesTheAccountItRejects(t *testing.T) {
	const email = "s1@example.edu"
	stub := newKeystoneStub(t, "aaaabbbbccccddddeeeeffff00001111") // a uuid, not the derived sha256
	client := stub.client()

	user, err := client.findOrCreateFederatedUser(email, nil)

	if user != nil {
		t.Errorf("user = %+v, want nil on conflict", user)
	}
	var conflict *PreseedConflict
	if !asPreseedConflict(err, &conflict) {
		t.Fatalf("err = %v, want a *PreseedConflict", err)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != stub.createdID {
		t.Fatalf("deleted = %v, want exactly the account just created (%s)", stub.deleted, stub.createdID)
	}
	if strings.Contains(conflict.Reason, "delete the account and pre-seed it manually") {
		t.Error("the reason still tells the operator to delete an account that is already gone")
	}
}

// The happy path must not delete anything.
func TestFindOrCreateFederatedUser_KeepsAMatchingAccount(t *testing.T) {
	const email = "s1@example.edu"
	expected := FederatedUserID("default", FederatedUniqueID(email))
	stub := newKeystoneStub(t, expected)
	client := stub.client()

	user, err := client.findOrCreateFederatedUser(email, nil)
	if err != nil {
		t.Fatalf("findOrCreateFederatedUser: %v", err)
	}
	if user == nil || user.ID != expected {
		t.Fatalf("user = %+v, want the account with the derived id %s", user, expected)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing removed on the happy path", stub.deleted)
	}
}

// asPreseedConflict keeps the test readable; errors.As needs a typed target.
func asPreseedConflict(err error, target **PreseedConflict) bool {
	if err == nil {
		return false
	}
	c, ok := err.(*PreseedConflict)
	if ok {
		*target = c
	}
	return ok
}
