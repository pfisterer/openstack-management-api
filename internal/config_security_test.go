package app

import (
	"strings"
	"testing"
)

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

// The configuration log says whether a secret is set and nothing more: it used to show the first four characters and the length of the database password.
func TestRedactedConfigShowsNoPartOfASecret(t *testing.T) {
	var c AppConfiguration
	c.Openstack.Password = "hunter2-very-secret"
	c.Openstack.ApplicationCredentialSecret = "appcred-secret-value"
	c.RoleProvider.APIToken = "rp-token-value"
	c.AIaaS.Token = "aiaas-token-value"
	c.Storage.ConnectionString = "host=db user=osmgt password='pa ss word' dbname=osmgt sslmode=require"
	r := redactedConfig(c)
	if r.Openstack.Password != "set" || r.Openstack.ApplicationCredentialSecret != "set" || r.RoleProvider.APIToken != "set" || r.AIaaS.Token != "set" {
		t.Fatalf("secrets not reduced to set: %+v", r)
	}
	if got, want := r.Storage.ConnectionString, "host=db user=osmgt password=set dbname=osmgt sslmode=require"; got != want {
		t.Fatalf("dsn = %q, want %q", got, want)
	}
	if redactedConfig(AppConfiguration{}).Openstack.Password != "empty" {
		t.Fatal("an unset secret must read as empty")
	}
	for _, dsn := range []string{
		"postgres://osmgt:pa55word@db:5432/osmgt?sslmode=require",
		"postgres://osmgt@db/osmgt?password=pa55word",
		"host=db password=pa55word",
	} {
		got := redactConnString(dsn)
		if strings.Contains(got, "pa55") || !strings.Contains(got, "db") {
			t.Errorf("redactConnString(%q) = %q", dsn, got)
		}
	}
}
