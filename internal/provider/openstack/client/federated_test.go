package osclient

import "testing"

// The pre-created account is only found by a later SSO login if unique_id matches
// byte for byte, so the encoding is pinned here rather than trusted.
func TestFederatedUniqueID(t *testing.T) {
	cases := map[string]string{
		"dennis.pfisterer@dhbw.de": "dennis.pfisterer%40dhbw.de",
		// Keycloak asserts usernames lowercased; Keystone itself normalizes nothing.
		"Raymond.Bimazubute@dhbw-stuttgart.de": "raymond.bimazubute%40dhbw-stuttgart.de",
		"s241221@student.dhbw-mannheim.de":     "s241221%40student.dhbw-mannheim.de",
		// Python's quote() leaves "/" and "~" alone and encodes a space as %20 —
		// Go's url.QueryEscape would produce "%2F" and "+" here.
		"a b/c~d": "a%20b/c~d",
	}
	for in, want := range cases {
		if got := FederatedUniqueID(in); got != want {
			t.Errorf("FederatedUniqueID(%q) = %q, want %q", in, got, want)
		}
	}
}

// The derived ID is what makes an exact lookup possible; a change here silently
// turns every pre-seeded account into a duplicate.
func TestFederatedUserID(t *testing.T) {
	// Cross-checked against the Python original:
	//   hashlib.sha256(("default" + "user" +
	//       quote("s241221@student.dhbw-mannheim.de")).encode()).hexdigest()
	const want = "6dbb3341e06b899310c5d0a2839a371d468fbf9842439c4ad8259e2c11aaa983"
	got := FederatedUserID("default", FederatedUniqueID("s241221@student.dhbw-mannheim.de"))
	if len(got) != 64 {
		t.Fatalf("derived id %q is not a sha256 hex digest", got)
	}
	if got != want {
		t.Errorf("FederatedUserID = %q, want %q", got, want)
	}
}
