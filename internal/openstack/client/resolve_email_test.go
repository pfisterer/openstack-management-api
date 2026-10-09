package osclient

import (
	"testing"

	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
)

func TestResolveUserEmail(t *testing.T) {
	user := func(name, email string) *users.User {
		u := &users.User{Name: name, Extra: map[string]any{}}
		if email != "" {
			u.Extra["email"] = email
		}
		return u
	}
	for _, c := range []struct {
		name, email, want string
	}{
		{"s1@example.edu", "s1@example.edu", "s1@example.edu"},
		{"s1@example.edu", "Bennet Frey (s1@example.edu)", "s1@example.edu"},
		{"bfrey", "Bennet Frey (s1@example.edu)", "s1@example.edu"},
		{"bfrey", "Bennet Frey <s1@example.edu>", "s1@example.edu"},
		{"svc-backup", "", "svc-backup"},
		{"admin", "ops@example.edu", "ops@example.edu"},
	} {
		if got := resolveUserEmail(user(c.name, c.email)); got != c.want {
			t.Errorf("name %q, email %q: got %q, want %q", c.name, c.email, got, c.want)
		}
	}
}
