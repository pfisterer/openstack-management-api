// Package identity provides the model-agnostic identity features: role-switch
// (group override), full identity impersonation, and the principal search behind
// every token field. It was extracted from the former applogic service — nothing
// in here depends on the resource tree model.
package identity

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// Store is the minimal persistence interface the identity service needs.
type Store interface {
	// ListParticipants returns distinct user emails appearing on any leaf node.
	// These are the only way to find people covered solely by a pattern rule
	// (students), who have no enumerable row in the directory.
	ListParticipants(ctx context.Context) ([]string, error)
}

// userTokenPrefix marks an impersonation override: the actor fully assumes the
// identity with this email (see ResolveEffectiveUserTokens).
const userTokenPrefix = "user:"

// groupTokenPrefix is the canonical prefix of group tokens.
const groupTokenPrefix = "group:"

// Service implements role-switch overrides and identity resolution.
type Service struct {
	store          Store
	roles          common.RoleProvider
	requestTimeout time.Duration

	overridesWriteMu sync.Mutex
	groupOverrides   atomic.Value // stores immutable map[string]string snapshots

	log *zap.SugaredLogger
}

// NewService constructs the identity service.
func NewService(store Store, roles common.RoleProvider, requestTimeout time.Duration, log *zap.SugaredLogger) *Service {
	if store == nil {
		panic("identity.NewService requires a non-nil store")
	}
	if roles == nil {
		panic("identity.NewService requires a non-nil role provider")
	}
	if requestTimeout <= 0 {
		requestTimeout = 30 * time.Second
	}
	svc := &Service{
		store:          store,
		roles:          roles,
		requestTimeout: requestTimeout,
		log:            log,
	}
	// Seed the first snapshot so readers can load without special cases.
	svc.groupOverrides.Store(map[string]string{})
	return svc
}

// NormalizeGroupToken normalizes a raw group token into canonical `group:<name>` form.
// Returns "" when input is blank or has an unsupported token shape.
func NormalizeGroupToken(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	if strings.HasPrefix(value, groupTokenPrefix) {
		return value
	}
	if strings.Contains(value, ":") {
		return ""
	}
	return groupTokenPrefix + value
}

// ApplyRoleSwitchOverride returns effective tokens by replacing all group tokens
// with the provided override group token, while preserving non-group tokens.
func ApplyRoleSwitchOverride(originalTokens common.TokenList, overrideGroupToken *string) common.TokenList {
	if overrideGroupToken == nil || *overrideGroupToken == "" {
		return originalTokens
	}
	override := *overrideGroupToken
	out := make(common.TokenList, 0, len(originalTokens)+1)
	for _, token := range originalTokens {
		if !strings.HasPrefix(token, groupTokenPrefix) {
			out = append(out, token)
		}
	}
	return append(out, override)
}

// ResolveEffectiveUserTokens applies a temporary actor-specific role-switch
// override. Two modes, distinguished by the stored override value:
//   - group override ("group:…"): swap group tokens, preserve everything else —
//     the actor keeps their own user/root identity and acts within the group.
//   - identity override ("user:…"): replace the ENTIRE token set with the target
//     identity's tokens, so the actor fully impersonates that user, including
//     dropping their own root-admin grant.
func (s *Service) ResolveEffectiveUserTokens(actorEmail string, originalTokens common.TokenList) common.TokenList {
	canonicalActor := canonicalActorEmail(actorEmail)
	if canonicalActor == "" {
		s.log.Warnf("ResolveEffectiveUserTokens called with empty actorEmail, returning original tokens")
		return originalTokens
	}

	override := s.currentGroupOverrides()[canonicalActor]
	if override == "" {
		return originalTokens
	}

	if email, ok := strings.CutPrefix(override, userTokenPrefix); ok {
		ctx, cancel := s.newCtx()
		defer cancel()
		tokens, err := s.roles.GetUserTokens(ctx, &common.UserClaims{Email: email})
		if err != nil || len(tokens) == 0 {
			s.log.Warnf("ResolveEffectiveUserTokens: impersonation of %q failed (err=%v, tokens=%d); using original tokens", email, err, len(tokens))
			return originalTokens
		}
		return tokens
	}

	return ApplyRoleSwitchOverride(originalTokens, common.Ptr(override))
}

// ResolveEffectiveEmail returns the email the actor is acting AS. It equals the
// actor's own email unless an identity impersonation override is active, in which
// case it returns the impersonated identity's email (so email-scoped views follow
// the assumed user). Group overrides do not change the acting email.
func (s *Service) ResolveEffectiveEmail(actorEmail string) string {
	canonical := canonicalActorEmail(actorEmail)
	if canonical == "" {
		return actorEmail
	}
	if override := s.currentGroupOverrides()[canonical]; override != "" {
		if email, ok := strings.CutPrefix(override, userTokenPrefix); ok {
			return email
		}
	}
	return actorEmail
}

// SetUserGroupSwitchForActor stores a temporary effective group for one actor.
func (s *Service) SetUserGroupSwitchForActor(actorEmail, groupToken string) error {
	normalizedActor := canonicalActorEmail(actorEmail)
	if normalizedActor == "" {
		return fmt.Errorf("actor email must not be empty")
	}

	normalizedGroup := NormalizeGroupToken(groupToken)
	if normalizedGroup == "" {
		return fmt.Errorf("group_token must not be empty")
	}

	s.overridesWriteMu.Lock()
	defer s.overridesWriteMu.Unlock()

	current := s.currentGroupOverrides()
	next := cloneGroupOverrides(current, 1)
	next[normalizedActor] = normalizedGroup
	s.groupOverrides.Store(next)
	return nil
}

// SetUserImpersonationForActor makes the actor fully assume the given identity
// (see ResolveEffectiveUserTokens, identity mode). The endpoint is root-gated, and
// the effective tokens are resolved live from the role provider, so an unknown
// email simply yields an empty view. There is deliberately no whitelist of
// assumable identities — a root admin may become any address, which is the only
// way to reach someone who is a member through a pattern rule. Stored in the same
// per-actor override slot, so it replaces any active group override and vice versa.
func (s *Service) SetUserImpersonationForActor(actorEmail, targetEmail string) error {
	normalizedActor := canonicalActorEmail(actorEmail)
	if normalizedActor == "" {
		return fmt.Errorf("actor email must not be empty")
	}
	target := canonicalActorEmail(targetEmail)
	if target == "" {
		return fmt.Errorf("impersonate_user must not be empty")
	}

	s.overridesWriteMu.Lock()
	defer s.overridesWriteMu.Unlock()

	current := s.currentGroupOverrides()
	next := cloneGroupOverrides(current, 1)
	next[normalizedActor] = userTokenPrefix + target
	s.groupOverrides.Store(next)
	return nil
}

// ClearUserGroupSwitchForActor removes the temporary override for one actor.
func (s *Service) ClearUserGroupSwitchForActor(actorEmail string) {
	normalizedActor := canonicalActorEmail(actorEmail)
	if normalizedActor == "" {
		return
	}

	s.overridesWriteMu.Lock()
	defer s.overridesWriteMu.Unlock()

	current := s.currentGroupOverrides()
	next := cloneGroupOverrides(current, 0)
	delete(next, normalizedActor)
	s.groupOverrides.Store(next)
}

// GetUserGroupSwitchForActor retrieves the temporary override for one actor.
func (s *Service) GetUserGroupSwitchForActor(actorEmail string) *string {
	actor := canonicalActorEmail(actorEmail)
	if actor == "" {
		return nil
	}
	if override := s.currentGroupOverrides()[actor]; override != "" {
		return common.Ptr(override)
	}
	return nil
}

// SearchPrincipals returns what a token field may be filled with: groups matched
// on token, display name or description, and users matched on their email
// address ONLY.
//
// The asymmetry is deliberate. A group is an organizational label and may be
// browsed; a person is not, so you have to know (part of) someone's address to
// find them — you cannot search the staff by name. Users come from the role
// provider plus everyone already participating in the tree, which is how members
// covered only by a pattern rule (students) become findable at all.
//
// Both halves are best-effort: a directory outage yields the local half rather
// than an error, so a form still works.
func (s *Service) SearchPrincipals(query string, limit int) ([]common.GroupSummary, []string, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	groups, err := s.roles.SearchGroups(ctx, query, limit)
	if err != nil {
		s.log.Warnw("principal search: group search failed", "error", err)
		groups = nil
	}

	byEmail := map[string]struct{}{}
	users := []string{}
	addUser := func(email string) {
		email = strings.TrimSpace(email)
		if email == "" || len(users) >= limit {
			return
		}
		key := strings.ToLower(email)
		if _, dup := byEmail[key]; dup {
			return
		}
		byEmail[key] = struct{}{}
		users = append(users, email)
	}

	// An empty query must not dump the directory: users only appear once the
	// caller has typed something to match on.
	if needle := strings.ToLower(strings.TrimSpace(query)); needle != "" {
		if found, err := s.roles.SearchUsers(ctx, query, limit); err != nil {
			s.log.Warnw("principal search: user search failed", "error", err)
		} else {
			for _, email := range found {
				addUser(email)
			}
		}
		if participants, err := s.store.ListParticipants(ctx); err != nil {
			s.log.Warnw("principal search: participant lookup failed", "error", err)
		} else {
			for _, email := range participants {
				if strings.Contains(strings.ToLower(email), needle) {
					addUser(email)
				}
			}
		}
		sort.Strings(users)
	}

	return groups, users, nil
}

// newCtx returns a context with the configured request deadline.
func (s *Service) newCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), s.requestTimeout)
}

// currentGroupOverrides returns the latest immutable override snapshot.
func (s *Service) currentGroupOverrides() map[string]string {
	return s.groupOverrides.Load().(map[string]string)
}

// cloneGroupOverrides creates a writable copy for copy-on-write updates.
func cloneGroupOverrides(current map[string]string, extraCapacity int) map[string]string {
	next := make(map[string]string, len(current)+extraCapacity)
	maps.Copy(next, current)
	return next
}

// canonicalActorEmail normalizes actor identifiers used as override map keys.
func canonicalActorEmail(actorEmail string) string {
	return strings.ToLower(strings.TrimSpace(actorEmail))
}
