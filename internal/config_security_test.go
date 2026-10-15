package app

import "testing"

// ROLE_PROVIDER has no default any more, and the mock is refused in production: running on the demo identities would hand out their group memberships.
func TestResolveRoleProviderType(t *testing.T) {
	cases := []struct {
		raw     string
		devMode bool
		want    string
		wantErr bool
	}{
		{"http", false, "http", false},
		{" HTTP ", false, "http", false},
		{"mock", true, "mock", false},
		{"mock", false, "", true},
		{"", true, "", true},
		{"", false, "", true},
		{"htpp", true, "", true},
	}
	for _, c := range cases {
		got, err := resolveRoleProviderType(c.raw, c.devMode)
		if (err != nil) != c.wantErr || got != c.want {
			t.Errorf("resolveRoleProviderType(%q, dev=%v) = %q, %v", c.raw, c.devMode, got, err)
		}
	}
}

// The server has timeouts: router.Run had none, so a client trickling its request held a connection for ever.
func TestHTTPServerHasTimeouts(t *testing.T) {
	s := newHTTPServer(":0", nil)
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.WriteTimeout <= 0 || s.IdleTimeout <= 0 {
		t.Fatalf("missing timeouts: %+v", s)
	}
}
