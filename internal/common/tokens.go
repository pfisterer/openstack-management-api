package common

import (
	"strings"
)

// UserPrefix and GroupPrefix mark the two kinds of authorization token.
const (
	UserPrefix  = "user:"
	GroupPrefix = "group:"
)

// TokenList is an alias for a list of tokens (string).
type TokenList []string

// CanonicalToken is the one spelling a token is stored and compared in:
// trimmed and lowercased, the whole of it. No token may tell two things apart by
// case — an e-mail address compares case-insensitively and identity providers
// release it in whatever case the directory holds, and a "group:Leiter-ZWR"
// next to "group:leiter-zwr" is a typo that would grant nothing. The role
// provider keeps its ids in the same spelling.
func CanonicalToken(token string) string {
	return strings.ToLower(strings.TrimSpace(token))
}

// CanonicalTokens returns the list in canonical spelling, without the
// duplicates that folding case can reveal. nil stays nil, so an absent list
// in a request stays absent.
func CanonicalTokens(tokens TokenList) TokenList {
	if tokens == nil {
		return nil
	}
	out := make(TokenList, 0, len(tokens))
	seen := make(map[string]bool, len(tokens))
	for _, t := range tokens {
		c := CanonicalToken(t)
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

// TokenSet is a set of tokens for O(1) membership tests.
type TokenSet map[string]struct{}

// NewTokenSet builds a TokenSet from a TokenList.
func NewTokenSet(tokens TokenList) TokenSet {
	s := make(TokenSet, len(tokens))
	for _, t := range tokens {
		s[t] = struct{}{}
	}
	return s
}

// Contains reports whether token is in the set.
func (s TokenSet) Contains(token string) bool {
	_, ok := s[token]
	return ok
}

// ContainsAny reports whether any token in the list is in the set.
func (s TokenSet) ContainsAny(tokens TokenList) bool {
	for _, t := range tokens {
		if _, ok := s[t]; ok {
			return true
		}
	}
	return false
}
