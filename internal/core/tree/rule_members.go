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
		return "", invalid("Die verantwortliche Person muss als E-Mail-Adresse oder als user:-Token angegeben werden.")
	}
	return "user:" + strings.ToLower(v), nil
}

// requireGroupMembership switches the rule that only a group's members (or the managers above) may add the group. Off since 10.11.2026, decided: the role provider knows no lecturers of the Dualis courses (dualis-* groups carry only #studierende), so no lecturer could add the students of their course; until it does, anybody may add any existing group. Switch it on again (and drop this note) once Dualis delivers the lecturers, see __TODOS.md in dhbw-deployment (g1).
var requireGroupMembership = false

// normalizeAuthorizedUsers validates and normalizes authorization entries.
//
// Every entry here has consequences in OpenStack: the reconciler creates a
// Keystone group for each group token, resolves its members through the role
// provider and CREATES an account for each of them. An unchecked token therefore
// let any requester materialize identities for people who never used the system,
// and a typo left an empty group behind on every run. So a token must name
// something that exists, and the list has a ceiling.
//
// A group grants everyone in it access to the project, so whoever adds one must belong to it (while requireGroupMembership is on, which it is not at present): callerTokens has to hold the group token. Otherwise any requester could put a whole faculty, or the root admins' group, into a project of theirs — and with a budget that auto-approves, nobody would ever look. manager exempts the managers of the budgets above the node (root admins included), who decide over access there anyway. Groups the node already lists (keep) stay allowed, so an owner can edit a list a manager extended.
//
// ceiling is how many members the project may list (memberCeilingOf).
func (s *Service) normalizeAuthorizedUsers(ctx context.Context, users []common.AuthorizedUser, ceiling memberCeiling, callerTokens common.TokenList, manager bool, keep ...[]common.AuthorizedUser) ([]common.AuthorizedUser, error) {
	if err := checkMemberCount(len(users), ceiling); err != nil {
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
			return nil, invalid("Mitglieder: Eintrag %d hat kein Token.", i)
		}

		role := strings.ToLower(strings.TrimSpace(user.Role))
		if !slices.Contains(common.Roles, role) {
			return nil, invalid("Mitglieder: Eintrag %d hat die Rolle %q, erlaubt sind %v.", i, user.Role, common.Roles)
		}

		switch {
		case strings.HasPrefix(token, common.UserPrefix):
			email := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(token, common.UserPrefix)))
			// A plain address only: ParseAddress also accepts `Name <a@b>` and `a@b (comment)`, which would be stored and later resolved as a different person than the one displayed.
			if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email {
				return nil, invalid("Mitglieder: Eintrag %d ist keine gültige E-Mail-Adresse.", i)
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
					return nil, invalid("Mitglieder: Eintrag %d nennt die unbekannte Gruppe %q.", i, token)
				}
				if requireGroupMembership && !manager && !kept[token] && !caller.Contains(token) {
					return nil, refuse(common.ErrForbidden, "Mitglieder: Eintrag %d: Die Gruppe %q dürfen nur ihre eigenen Mitglieder hinzufügen.", i, token)
				}
				seenGroups[token] = true
			}

		default:
			return nil, invalid("Mitglieder: Eintrag %d muss ein %s- oder %s-Token sein.", i, common.UserPrefix, common.GroupPrefix)
		}

		out = append(out, common.AuthorizedUser{Token: token, Role: role})
	}
	return out, nil
}

// groupTokenExists asks the role provider whether a group token is real: the
// group must exist and, for "group:x#dozent", the relation must be one the
// provider knows. The search matches substrings, so the exact token still has
// to be picked out of the results.
func (s *Service) groupTokenExists(ctx context.Context, token string) (bool, error) {
	if s.roles == nil {
		return false, fmt.Errorf("%w: no role provider configured", common.ErrInternal)
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

// ProviderRules is what a provider tells the tree about itself beyond its resources and caps (decided 10.11.2026, docs/plan-caps-katalog.md): how many members a project of its may list. The tree does not know why; for OpenStack each one costs Keystone work on every reconciler run, for the AI service only the size of what it is handed.
type ProviderRules interface {
	Provider() string
	// MaxMembers is how many entries of authorized_users a project of the provider may list (a group counts as one).
	MaxMembers() int
}

// SetProviderRules registers the rules of the providers; one per provider, a later one replaces an earlier one.
func (s *Service) SetProviderRules(rules ...ProviderRules) {
	if s.rules == nil {
		s.rules = map[string]ProviderRules{}
	}
	for _, r := range rules {
		s.rules[r.Provider()] = r
	}
}

// memberCeiling is the most members a project may list and the provider that sets it ("" for the tree's own ceiling).
type memberCeiling struct {
	max      int
	provider string
}

// memberCeilingOf is the ceiling on the members of project n — as it is, or as it becomes with one of limits (a requested change, a new allocation): the smallest MaxMembers of the providers it concerns (concernedProviders), else the tree's own (NewService).
func (s *Service) memberCeilingOf(n model.Node, limits ...common.ProjectQuota) memberCeiling {
	out := memberCeiling{max: s.maxMembers}
	first := true
	for _, p := range s.concernedProviders(n, limits...) {
		r, ok := s.rules[p]
		if !ok {
			continue
		}
		if first || r.MaxMembers() < out.max {
			out, first = memberCeiling{max: r.MaxMembers(), provider: p}, false
		}
	}
	return out
}

// concernedProviders are the providers with rules (SetProviderRules) whose business project n is, or becomes with one of limits: it has their binding, or its limit, an allocation, a waiting change or one of limits holds one of their resources (a resource without a provider belongs to every provider). The view the providers take of their leaves (concerns). Sorted.
func (s *Service) concernedProviders(n model.Node, limits ...common.ProjectQuota) []string {
	all := append([]common.ProjectQuota{n.EffectiveLimit()}, limits...)
	if n.Pending != nil && n.Pending.Limit != nil {
		all = append(all, *n.Pending.Limit)
	}
	var out []string
	for p := range s.rules {
		if _, bound := n.Bindings[p]; bound || s.holdsOf(p, all) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// holdsOf reports whether one of limits holds a resource of provider p.
func (s *Service) holdsOf(p string, limits []common.ProjectQuota) bool {
	for _, r := range s.resources() {
		if !r.BelongsTo(p) {
			continue
		}
		for _, l := range limits {
			if l.Holds(r.ID) {
				return true
			}
		}
	}
	return false
}

// checkMemberCount refuses n members above c, naming it.
func checkMemberCount(n int, c memberCeiling) error {
	if n <= c.max {
		return nil
	}
	if c.provider != "" {
		return invalid("Ein Projekt mit Ressourcen von %[2]s hat höchstens %[1]d Mitglieder, angegeben sind %[3]d. Für einen ganzen Kurs oder eine Abteilung eignet sich ein Gruppen-Token.", c.max, c.provider, n)
	}
	return invalid("Ein Projekt hat höchstens %d Mitglieder, angegeben sind %d. Für einen ganzen Kurs oder eine Abteilung eignet sich ein Gruppen-Token.", c.max, n)
}

// checkMemberCeilingFalls refuses a change that lowers the ceiling on the members of a project (from was, to next) below the members it already lists.
func checkMemberCeilingFalls(members int, was, next memberCeiling) error {
	if next.max >= was.max {
		return nil
	}
	return checkMemberCount(members, next)
}
