package tree

import (
	"context"
	"fmt"
	"net/mail"
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/core/roles"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// tokenListsEqual compares two token lists as sets.
func tokenListsEqual(a, b common.TokenList) bool {
	if len(a) != len(b) {
		return false
	}
	setA := common.NewTokenSet(a)
	for _, t := range b {
		if !setA.Contains(t) {
			return false
		}
	}
	return true
}

// normalizeOwnerToken accepts "email" or "user:email" and returns "user:email".
func normalizeOwnerToken(raw string) (string, error) {
	v := strings.TrimSpace(raw)
	v = strings.TrimPrefix(v, "user:")
	if v == "" || strings.Contains(v, ":") {
		return "", fmt.Errorf("owner must be a plain email or user: token")
	}
	return "user:" + strings.ToLower(v), nil
}

// normalizeAuthorizedUsers validates and normalizes authorization entries.
//
// Every entry here has consequences in OpenStack: the reconciler creates a
// Keystone group for each group token, resolves its members through the role
// provider and CREATES an account for each of them. An unchecked token therefore
// let any requester materialize identities for people who never used the system,
// and a typo left an empty group behind on every run. So a token must name
// something that exists, and the list has a ceiling.
func (s *Service) normalizeAuthorizedUsers(ctx context.Context, users []common.AuthorizedUser) ([]common.AuthorizedUser, error) {
	if len(users) > s.maxAuthorizedUsers {
		return nil, fmt.Errorf("at most %d authorized users — use a group token to grant access to a whole course or department", s.maxAuthorizedUsers)
	}

	out := make([]common.AuthorizedUser, 0, len(users))
	seenGroups := make(map[string]bool, len(users))

	for i, user := range users {
		token := common.CanonicalToken(user.Token)
		if token == "" {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has empty token", i)
		}

		role := strings.ToLower(strings.TrimSpace(user.OpenstackRole))
		if !slices.Contains(common.OpenstackRoles, role) {
			return nil, fmt.Errorf("invalid authorized_users: entry %d has role %q, expected one of %v", i, user.OpenstackRole, common.OpenstackRoles)
		}

		switch {
		case strings.HasPrefix(token, common.UserPrefix):
			email := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(token, common.UserPrefix)))
			if _, err := mail.ParseAddress(email); err != nil {
				return nil, fmt.Errorf("invalid authorized_users: entry %d is not a valid email address", i)
			}
			token = common.UserPrefix + email

		case strings.HasPrefix(token, common.GroupPrefix):
			// "group:x#member" is spelled "group:x", so the same grant cannot
			// appear under two names.
			token = roles.JoinGroupToken(roles.SplitGroupToken(token))
			if !seenGroups[token] {
				exists, err := s.groupTokenExists(ctx, token)
				if err != nil {
					// Fail closed. The group directory is also what resolves the
					// caller's own tokens, so if it is unreachable nothing else
					// works either — better an honest error than a node whose
					// members were never checked.
					return nil, fmt.Errorf("cannot verify group %q right now: %w", token, err)
				}
				if !exists {
					return nil, fmt.Errorf("invalid authorized_users: entry %d references unknown group %q", i, token)
				}
				seenGroups[token] = true
			}

		default:
			return nil, fmt.Errorf("invalid authorized_users: entry %d must be a %s or %s token", i, common.UserPrefix, common.GroupPrefix)
		}

		out = append(out, common.AuthorizedUser{Token: token, OpenstackRole: role})
	}
	return out, nil
}

// groupTokenExists asks the role provider whether a group token is real: the
// group must exist and, for "group:x#dozent", the relation must be one the
// provider knows. The search matches substrings, so the exact token still has
// to be picked out of the results.
func (s *Service) groupTokenExists(ctx context.Context, token string) (bool, error) {
	if s.roles == nil {
		return false, fmt.Errorf("no role provider configured")
	}
	token, relation := roles.SplitGroupToken(token)
	if relation != roles.RelationMember {
		relations, err := s.roles.ListRelations(ctx)
		if err != nil {
			return false, err
		}
		if !slices.Contains(relations, relation) {
			return false, nil
		}
	}
	groups, err := s.roles.SearchGroups(ctx, token, common.DefaultPageLimit)
	if err != nil {
		return false, err
	}
	for _, g := range groups {
		if strings.EqualFold(g.Token, token) {
			return true, nil
		}
	}
	return false, nil
}
