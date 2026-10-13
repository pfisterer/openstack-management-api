// Package identity provides the model-agnostic identity features: acting on behalf of another person (the X-Act-As header) and the principal search behind every token field. Nothing in here depends on the resource tree model.
package identity

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/pfisterer/openstack-management-api/internal/core/roles"

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

// userTokenPrefix is the canonical prefix of user tokens.
const userTokenPrefix = "user:"

// Service resolves who a caller acts as and searches principals.
type Service struct {
	store          Store
	roles          roles.Provider
	requestTimeout time.Duration

	log *zap.SugaredLogger
}

// NewService constructs the identity service.
func NewService(store Store, roles roles.Provider, requestTimeout time.Duration, log *zap.SugaredLogger) *Service {
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
	return svc
}

// TokensFor resolves the tokens of the person a caller acts on behalf of, live from the role provider. A person the provider does not know is just their user token, so an address that is only covered by a pattern rule, or not known at all, still yields a faithful view: what that person would see.
func (s *Service) TokensFor(email string) (common.TokenList, error) {
	ctx, cancel := s.newCtx()
	defer cancel()
	tokens, err := s.roles.GetUserTokens(ctx, &common.UserClaims{Email: email})
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return common.TokenList{userTokenPrefix + email}, nil
	}
	return tokens, nil
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
func (s *Service) SearchPrincipals(query string, limit int) ([]roles.GroupSummary, []string, error) {
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
