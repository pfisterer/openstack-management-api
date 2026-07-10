package common

import (
	"sort"
	"strings"
)

// ParticipantEmails returns the distinct, sorted set of user email addresses that
// appear as participants across the given projects — i.e. the email carried by any
// "user:<email>" requester token or authorized-user token. Group and pattern
// tokens are ignored (a glob membership has no concrete email).
//
// This is how role-switch impersonation surfaces pattern-covered members such as
// students: they have no enumerable rows in the role provider, but once they own
// (or are authorized on) a project their email becomes assumable here.
// Deduplication is case-insensitive; the first-seen spelling is preserved.
func ParticipantEmails(projects []Project) []string {
	seen := map[string]string{} // lower-cased email -> first-seen spelling
	for _, p := range projects {
		for _, t := range p.RequesterTokens {
			addUserEmail(seen, t)
		}
		for _, u := range p.AuthorizedUsers {
			addUserEmail(seen, u.Token)
		}
	}
	out := make([]string, 0, len(seen))
	for _, orig := range seen {
		out = append(out, orig)
	}
	sort.Strings(out)
	return out
}

// addUserEmail records the email of a "user:<email>" token into seen, keyed by its
// lower-cased form. Non-user tokens (group:/pattern:) and blank emails are skipped.
func addUserEmail(seen map[string]string, token string) {
	const prefix = "user:"
	if !strings.HasPrefix(token, prefix) {
		return
	}
	email := strings.TrimSpace(token[len(prefix):])
	if email == "" {
		return
	}
	if key := strings.ToLower(email); key != "" {
		if _, ok := seen[key]; !ok {
			seen[key] = email
		}
	}
}
