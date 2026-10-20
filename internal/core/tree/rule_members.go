package tree

import (
	"context"
	"fmt"
	"net/mail"
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
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
//
// A group grants everyone in it access to the project, so whoever adds one must belong to it: callerTokens has to hold the group token. Otherwise any requester could put a whole faculty, or the root admins' group, into a project of theirs — and with a budget that auto-approves, nobody would ever look. manager exempts the managers of the budgets above the node (root admins included), who decide over access there anyway. Groups the node already lists (keep) stay allowed, so an owner can edit a list a manager extended.
//
// openstack says whether the project involves OpenStack (involvesOpenStack), which decides the ceiling (maxAuthorizedUsersFor).
func (s *Service) normalizeAuthorizedUsers(ctx context.Context, users []common.AuthorizedUser, openstack bool, callerTokens common.TokenList, manager bool, keep ...[]common.AuthorizedUser) ([]common.AuthorizedUser, error) {
	if err := s.checkAuthorizedUserCount(len(users), openstack); err != nil {
		return nil, err
	}
	caller := common.NewTokenSet(common.CanonicalTokens(callerTokens))
	kept := map[string]bool{}
	for _, list := range keep {
		for _, u := range list {
			kept[u.Token] = true
		}
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
			// A plain address only: ParseAddress also accepts `Name <a@b>` and `a@b (comment)`, which would be stored and later resolved as a different person than the one displayed.
			if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
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
					return nil, fmt.Errorf("cannot verify group %q right now: %w: %w", token, common.ErrInternal, err)
				}
				if !exists {
					return nil, fmt.Errorf("invalid authorized_users: entry %d references unknown group %q", i, token)
				}
				if !manager && !kept[token] && !caller.Contains(token) {
					return nil, fmt.Errorf("%w: authorized_users entry %d: only members of group %q may add it", common.ErrForbidden, i, token)
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

// maxAuthorizedUsersFor is the ceiling on a project's participants. Only a project that involves OpenStack makes each of them cost work on every reconciler run (a Keystone account, group and role assignment); the members of a workspace or LLM project are only handed to the AI service, so those projects get a ceiling of their own (SetMaxAuthorizedUsersOther).
func (s *Service) maxAuthorizedUsersFor(openstack bool) int {
	if openstack {
		return s.maxAuthorizedUsers
	}
	return s.maxAuthorizedUsersOther
}

// checkAuthorizedUserCount refuses n participants above the ceiling that applies, naming it.
func (s *Service) checkAuthorizedUserCount(n int, openstack bool) error {
	limit := s.maxAuthorizedUsersFor(openstack)
	if n <= limit {
		return nil
	}
	if openstack {
		return fmt.Errorf("at most %d authorized users in a project with OpenStack resources (got %d) — use a group token to grant access to a whole course or department", limit, n)
	}
	return fmt.Errorf("at most %d authorized users in a project (got %d) — use a group token to grant access to a whole course or department", limit, n)
}

// involvesOpenStack reports whether project n is OpenStack's business — or becomes it with one of limits (a requested change, a new allocation): it has an OpenStack binding (an OpenStack project exists or existed for it, imports included), or its limit, an allocation, a waiting change or one of limits holds an OpenStack resource. The same view the reconciler takes (concerns): a resource without a provider belongs to OpenStack too. A held availability counts, a quantity of 0 does not.
func (s *Service) involvesOpenStack(n model.Node, limits ...common.ProjectQuota) bool {
	if _, bound := n.Bindings[common.ProviderOpenStack]; bound || n.Status == model.StatusImported {
		return true
	}
	all := append([]common.ProjectQuota{n.EffectiveLimit()}, limits...)
	if n.Pending != nil && n.Pending.Limit != nil {
		all = append(all, *n.Pending.Limit)
	}
	for _, r := range s.resources() {
		if !r.BelongsTo(common.ProviderOpenStack) {
			continue
		}
		for _, l := range all {
			if v := l[r.ID]; v > 0 || v == common.UnlimitedQuota {
				return true
			}
		}
	}
	return false
}
