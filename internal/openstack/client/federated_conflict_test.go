package osclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
	"go.uber.org/zap"
)

// keystoneStub is the smallest Keystone that findOrCreateFederatedUser needs:
// a user list, a user GET and a user create/delete. It records the deletes,
// which is the whole point of the test below.
type keystoneStub struct {
	// createdID is the id the stub hands out on POST /users — the knob that
	// decides whether the caller sees a match or a conflict.
	createdID string
	// present answers GET /users/{id}: the account a login binds to exists only
	// when a test puts it here.
	present map[string]string // id -> name
	// federated marks ids whose GET carries a federation link — Keystone returns
	// those on a single-user GET only, never in a list.
	federated map[string]bool
	deleted   []string
	server    *httptest.Server
}

func newKeystoneStub(t *testing.T, createdID string) *keystoneStub {
	t.Helper()
	stub := &keystoneStub{createdID: createdID, present: map[string]string{}, federated: map[string]bool{}}
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
			name, ok := stub.present[id]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			user := map[string]any{"id": id, "name": name, "enabled": true, "description": ManagedUserDescription}
			if stub.federated[id] {
				user["federated"] = []any{map[string]any{
					"idp_id":    "keycloak",
					"protocols": []any{map[string]any{"protocol_id": "openid", "unique_id": FederatedUniqueID("s1@example.edu")}},
				}}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"user": user})
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

// Keystone mints its own ID on POST /v3/users and ignores one supplied in the
// request — verified against the cloud this runs on. So a pre-created account
// NEVER carries the ID a login resolves to, and an earlier version of this code
// took that difference as proof of a broken assumption and deleted the account
// again. That made pre-creation impossible: no cloud can pass that test.
//
// The stand-in is what holds the role until the first login, so it has to
// survive the call that created it.
func TestFindOrCreateFederatedUser_KeepsThePreCreatedStandIn(t *testing.T) {
	const email = "s1@example.edu"
	stub := newKeystoneStub(t, "aaaabbbbccccddddeeeeffff00001111") // a uuid, as Keystone hands out
	client := stub.client()

	user, err := client.findOrCreateFederatedUser(email, nil)
	if err != nil {
		t.Fatalf("findOrCreateFederatedUser: %v", err)
	}
	if user == nil || user.ID != stub.createdID {
		t.Fatalf("user = %+v, want the account just created (%s)", user, stub.createdID)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("deleted = %v, want the stand-in kept", stub.deleted)
	}
}

// Once the person has logged in, the account Keystone made for that login is
// the only one worth holding a role — and our stand-in is a duplicate of the
// same person, so it goes.
func TestFindOrCreateFederatedUser_LoginAccountWinsAndSupersedesTheStandIn(t *testing.T) {
	const email = "s1@example.edu"
	derived := FederatedUserID("default", FederatedUniqueID(email))
	stub := newKeystoneStub(t, "unused")
	stub.present[derived] = email
	client := stub.client()

	standIn := &users.User{ID: "aaaabbbbccccddddeeeeffff00001111", Name: email, Description: ManagedUserDescription}
	user, err := client.findOrCreateFederatedUser(email, standIn)
	if err != nil {
		t.Fatalf("findOrCreateFederatedUser: %v", err)
	}
	if user == nil || user.ID != derived {
		t.Fatalf("user = %+v, want the account the login binds to (%s)", user, derived)
	}
	if len(stub.deleted) != 1 || stub.deleted[0] != standIn.ID {
		t.Fatalf("deleted = %v, want exactly the superseded stand-in (%s)", stub.deleted, standIn.ID)
	}
}

// An account we did not create is never deleted and never silently reused: a
// role assigned to it would be invisible to the login.
func TestFindOrCreateFederatedUser_LeavesAForeignAccountAlone(t *testing.T) {
	const email = "s1@example.edu"
	stub := newKeystoneStub(t, "unused")
	client := stub.client()

	foreign := &users.User{ID: "0000111122223333444455556666aaaa", Name: email, Description: "somebody else's account"}
	user, err := client.findOrCreateFederatedUser(email, foreign)

	if user != nil {
		t.Errorf("user = %+v, want nil rather than a guess", user)
	}
	var conflict *PreseedConflict
	if !asPreseedConflict(err, &conflict) {
		t.Fatalf("err = %v, want a *PreseedConflict", err)
	}
	if len(stub.deleted) != 0 {
		t.Fatalf("deleted = %v, want a foreign account left untouched", stub.deleted)
	}
}

// The regression this cost a staging deployment: Keystone's user LIST omits
// federated attributes, so the account we pre-created looks link-less on every
// later sync. Judging it on that payload turned "our own stand-in" into "an
// account we must not touch", and the role was never assigned to anything after
// the run that created it.
func TestFindOrCreateFederatedUser_ReReadsTheAccountBeforeJudgingIt(t *testing.T) {
	const email = "s1@example.edu"
	stub := newKeystoneStub(t, "unused")
	const standInID = "aaaabbbbccccddddeeeeffff00001111"
	stub.present[standInID] = email  // a GET finds it…
	stub.federated[standInID] = true // …carrying the link a list would not show
	client := stub.client()

	// Exactly what FindUserByName hands over: no federated attributes at all.
	fromList := &users.User{ID: standInID, Name: email, Description: ManagedUserDescription}
	user, err := client.findOrCreateFederatedUser(email, fromList)
	if err != nil {
		t.Fatalf("findOrCreateFederatedUser: %v", err)
	}
	if user == nil || user.ID != standInID {
		t.Fatalf("user = %+v, want the existing stand-in (%s) reused", user, standInID)
	}
	if len(stub.deleted) != 0 {
		t.Errorf("deleted = %v, want nothing removed", stub.deleted)
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
