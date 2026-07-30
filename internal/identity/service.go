// Package identity provides the model-agnostic identity features: role-switch
// (group override), full identity impersonation, and the assumable-identity
// picker. It was extracted from the former applogic service — nothing in here
// depends on the resource tree model.
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
	// ListIdentities returns the seeded identities (mock/dev data).
	ListIdentities(ctx context.Context) ([]common.Identity, error)
	// ListParticipants returns distinct user emails appearing on any leaf node.
	ListParticipants(ctx context.Context) ([]string, error)
}

// userTokenPrefix marks an impersonation override: the actor fully assumes the
// identity with this email (see ResolveEffectiveUserTokens).
const userTokenPrefix = "user:"

// groupTokenPrefix is the canonical prefix of group tokens.
const groupTokenPrefix = "group:"

// identityPickerGroupLimit bounds how many groups we expand to enumerate staff
// principals from the role provider. The provider has no user-search endpoint
// (only group search + group→members), so we enumerate the members of the known
// groups. Fine at the current scale; if the directory grows large this is the
// point to materialize a searchable principals table or add a subject-search
// endpoint upstream.
const identityPickerGroupLimit = 200

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

	return ApplyRoleSwitchOverride(originalTokens, ptr(override))
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

// ListAssumableIdentities returns the identities a root admin may impersonate via
// role switch. It fuses three sources, deduped case-insensitively by email:
//  1. seeded identities (mock/dev data), which carry richer labels + tokens;
//  2. staff principals enumerated from the role provider's known groups;
//  3. users who already participate in nodes (owner/authorized tokens) — the only
//     way pattern-covered members such as students surface, since a glob
//     membership has no enumerable rows.
//
// Role-provider lookups are best-effort: if the provider is unreachable the picker
// degrades to the locally known identities rather than failing outright. Used both
// to populate the UI picker and to validate impersonation targets, so the two can
// never disagree.
func (s *Service) ListAssumableIdentities() ([]common.Identity, error) {
	ctx, cancel := s.newCtx()
	defer cancel()

	byEmail := map[string]common.Identity{}
	add := func(id common.Identity) {
		email := strings.TrimSpace(id.Email)
		if email == "" {
			return
		}
		key := strings.ToLower(email)
		if existing, ok := byEmail[key]; ok {
			// Enrich an existing (leaner) entry with a label/tokens if we have them.
			if existing.Label == "" && id.Label != "" {
				existing.Label = id.Label
			}
			if len(existing.Tokens) == 0 && len(id.Tokens) > 0 {
				existing.Tokens = id.Tokens
			}
			byEmail[key] = existing
			return
		}
		if id.ID == "" {
			id.ID = email
		}
		byEmail[key] = id
	}

	// 1. Seeded identities first, so their richer label/tokens win on dedupe.
	seeded, err := s.store.ListIdentities(ctx)
	if err != nil {
		return nil, fmt.Errorf("list seeded identities: %w", err)
	}
	for _, id := range seeded {
		add(id)
	}

	// 2. Staff principals from the role provider's known groups (best-effort).
	if groups, err := s.roles.SearchGroupTokens(ctx, "", identityPickerGroupLimit); err != nil {
		s.log.Warnw("identity picker: group search failed, degrading to local identities", "error", err)
	} else {
		for _, group := range groups {
			emails, err := s.roles.GetGroupUsers(ctx, group)
			if err != nil {
				s.log.Warnw("identity picker: group member lookup failed", "group", group, "error", err)
				continue
			}
			for _, email := range emails {
				add(common.Identity{Email: email})
			}
		}
	}

	// 3. Users who already participate in nodes (surfaces pattern-covered members).
	if participants, err := s.store.ListParticipants(ctx); err != nil {
		s.log.Warnw("identity picker: participant lookup failed", "error", err)
	} else {
		for _, email := range participants {
			add(common.Identity{Email: email})
		}
	}

	out := make([]common.Identity, 0, len(byEmail))
	for _, id := range byEmail {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool {
		li, lj := out[i].Label, out[j].Label
		if li == "" {
			li = out[i].Email
		}
		if lj == "" {
			lj = out[j].Email
		}
		return strings.ToLower(li) < strings.ToLower(lj)
	})
	return out, nil
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
// email simply yields an empty view. The picker's fused identity list is therefore
// only quick-pick suggestions, not a whitelist. Stored in the same per-actor
// override slot, so it replaces any active group override and vice versa.
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
		return ptr(override)
	}
	return nil
}

// SearchGroupTokens returns matching group tokens via the role provider.
func (s *Service) SearchGroupTokens(query string, limit int) (common.TokenList, error) {
	ctx, cancel := s.newCtx()
	defer cancel()
	return s.roles.SearchGroupTokens(ctx, query, limit)
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

// ptr returns a pointer to the provided value for inline literals.
func ptr[T any](v T) *T {
	return &v
}
