package common

import (
	"reflect"
	"testing"
)

func TestParticipantEmails(t *testing.T) {
	projects := []Project{
		{
			RequesterTokens: TokenList{"user:Alice@dhbw.de", "group:studierende-dhbw-ma"},
			AuthorizedUsers: []AuthorizedUser{{Token: "user:bob@dhbw.de", OpenstackRole: "member"}},
		},
		{
			// Duplicate alice (different case) + a pattern token that must be ignored.
			RequesterTokens: TokenList{"user:alice@dhbw.de", "pattern:*@student.dhbw-mannheim.de"},
			AuthorizedUsers: []AuthorizedUser{{Token: "group:leiter-zwr"}},
		},
		{
			RequesterTokens: TokenList{"user:carol@dhbw.de"},
		},
	}

	got := ParticipantEmails(projects)
	// Sorted, case-insensitively deduped, first-seen spelling ("Alice") preserved,
	// group + pattern tokens dropped.
	want := []string{"Alice@dhbw.de", "bob@dhbw.de", "carol@dhbw.de"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParticipantEmails = %v, want %v", got, want)
	}
}

func TestParticipantEmailsEmpty(t *testing.T) {
	if got := ParticipantEmails(nil); len(got) != 0 {
		t.Fatalf("expected no participants, got %v", got)
	}
	// Only group/pattern tokens → nobody assumable.
	only := []Project{{RequesterTokens: TokenList{"group:x", "pattern:*@y.z", "user:"}}}
	if got := ParticipantEmails(only); len(got) != 0 {
		t.Fatalf("expected no participants from group/pattern/blank tokens, got %v", got)
	}
}
