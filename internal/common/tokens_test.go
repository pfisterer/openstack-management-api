package common

import (
	"slices"
	"testing"
)

func TestCanonicalTokens(t *testing.T) {
	got := CanonicalTokens(TokenList{" user:A.B@Example.org", "user:a.b@example.org", "group:Leiter-ZWR", "", "group:Leiter-ZWR"})
	want := TokenList{"user:a.b@example.org", "group:leiter-zwr"}
	if !slices.Equal(got, want) {
		t.Errorf("CanonicalTokens = %v, want %v", got, want)
	}
	if CanonicalTokens(nil) != nil {
		t.Error("an absent list must stay absent")
	}
}
