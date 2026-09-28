package reconciler

import "testing"

func TestKeystoneGroupNames(t *testing.T) {
	cases := []struct{ token, name string }{
		// Plain groups keep their historical name.
		{"group:dept_cs_faculty", "managed-dept_cs_faculty"},
		{"group:wwi23seb#dozent", "managed-wwi23seb--dozent"},
		{"group:wwi23seb#member", "managed-wwi23seb"},
	}
	for _, c := range cases {
		if got := keystoneGroupName("managed-", c.token); got != c.name {
			t.Errorf("keystoneGroupName(%q) = %q, want %q", c.token, got, c.name)
		}
	}

	back := []struct{ name, token string }{
		{"managed-wwi23seb--dozent", "group:wwi23seb#dozent"},
		{"managed-dept_cs_faculty", "group:dept_cs_faculty"},
		// Foreign groups are left as they are.
		{"lab-admins", "group:lab-admins"},
		// A trailing "--" with no relation is part of the name.
		{"managed-odd--", "group:odd--"},
	}
	for _, c := range back {
		if got := groupTokenForKeystoneName("managed-", c.name); got != c.token {
			t.Errorf("groupTokenForKeystoneName(%q) = %q, want %q", c.name, got, c.token)
		}
	}
}
