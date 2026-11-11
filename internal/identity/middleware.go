package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/credential"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

const apiTokenPrefix = "os_mgt_"
const userDataKey = "__api_userData"
const userTokensKey = "__api_userTokens"
const authContextKey = "__api_authContext"
const viaAPITokenKey = "__api_viaAPIToken"

// viaAPIToken reports whether the request authenticated with an API token rather than a sign-in.
func viaAPIToken(c *gin.Context) bool { return c.GetBool(viaAPITokenKey) }

// The read-only-token rule (recording the flag, reading it, and refusing writes
// for it) lives in cloud-self-service-golib/ginweb, shared with the other
// services. This file records the flag via ginweb.SetReadOnly once it has
// resolved the token.

// AuthContext holds the resolved identity for a single request, set once by EffectiveAuthMiddleware and read by handlers via FromContext.
//
// ActorEmail is the person signed in. UserEmail is the person the request acts as: the actor themself, or the person named in the X-Act-As header when an admin acts on their behalf. Everything is scoped on UserEmail and EffectiveTokens — the admin sees and does what that person would — while ActorEmail is what the history and the log record as the one who actually acted.
type AuthContext struct {
	ActorEmail      string
	UserEmail       string
	OriginalTokens  common.TokenList
	EffectiveTokens common.TokenList
	// ViaAPIToken says the caller authenticated with an API token rather than by signing in.
	ViaAPIToken bool
}

// OnBehalf reports whether the request acts on behalf of somebody else.
func (a AuthContext) OnBehalf() bool { return a.UserEmail != a.ActorEmail }

// Effective resolves the tokens of the person a caller acts on behalf of (*Service).
type Effective interface {
	TokensFor(email string) (common.TokenList, error)
}

// EffectiveAuthMiddleware resolves who the request is from and who it acts as, once per request, and stores both in the Gin context. Must run after the auth middleware that populates userDataKey / userTokensKey.
//
// mayActAs are the tokens that allow acting on behalf of another person (the root admins). The check runs on the actor's own tokens, never on the person's. A forbidden or malformed X-Act-As is refused rather than ignored: an admin who believes they act for somebody else and actually acts as themself would act on the wrong things.
func EffectiveAuthMiddleware(svc Effective, mayActAs common.TokenList, log *zap.SugaredLogger) gin.HandlerFunc {
	allowed := common.CanonicalTokenSet(mayActAs)
	return func(c *gin.Context) {
		actorEmail, originalTokens, err := ResolveOriginalAuthContext(c)
		if err != nil {
			ginweb.AbortError(c, ErrUnauthenticated)
			return
		}
		// Canonical spelling on the way in, so a role provider that hands back
		// "user:A.B@x" still matches the "user:a.b@x" stored on a node.
		originalTokens = common.CanonicalTokens(originalTokens)
		auth := AuthContext{ActorEmail: actorEmail, UserEmail: actorEmail, OriginalTokens: originalTokens, EffectiveTokens: originalTokens, ViaAPIToken: viaAPIToken(c)}

		if header := c.GetHeader(authn.ActAsHeader); header != "" {
			if viaAPIToken(c) {
				ginweb.AbortError(c, ErrActAsNeedsSignIn)
				return
			}
			target, err := authn.ActAs(header, actorEmail, len(allowed) > 0 && allowed.ContainsAny(originalTokens))
			if err != nil {
				ginweb.AbortError(c, ActAsError(err))
				return
			}
			if target != actorEmail {
				tokens, err := svc.TokensFor(target)
				if err != nil {
					log.Warnw("acting on behalf: cannot resolve the person's tokens", "actor", actorEmail, "as", target, "error", err)
					ginweb.AbortError(c, ErrDirectoryUnavailable)
					return
				}
				auth.UserEmail = target
				auth.EffectiveTokens = common.CanonicalTokens(tokens)
				if authn.IsWrite(c.Request.Method) {
					log.Infow("acting on behalf", "actor", actorEmail, "as", target, "method", c.Request.Method, "path", c.Request.URL.Path)
				}
			}
		}

		c.Set(authContextKey, auth)
		c.Next()
	}
}

// FromContext returns the AuthContext set by EffectiveAuthMiddleware.
func FromContext(c *gin.Context) (AuthContext, error) {
	v, ok := c.Get(authContextKey)
	if !ok {
		return AuthContext{}, fmt.Errorf("auth context not set")
	}
	ctx, ok := v.(AuthContext)
	if !ok {
		return AuthContext{}, fmt.Errorf("invalid auth context type")
	}
	return ctx, nil
}

// Use common.UserClaims everywhere

// OIDCVerifierConfig holds the minimal configuration for OIDC token verification.
// JWKSURL is what keeps a dead provider from taking this service down with it —
// see the oidcauth package for the whole story.
type OIDCVerifierConfig = oidcauth.Config

// NewOIDCAuthVerifier initializes the ID token verifier.
func NewOIDCAuthVerifier(cfg OIDCVerifierConfig, log *zap.SugaredLogger) (*oidcauth.Verifier, error) {
	return oidcauth.New(cfg, log)
}

func authenticatedEmailFromClaims(claims *common.UserClaims) string {
	return claims.Identity()
}

// ResolveOriginalAuthContext returns the authenticated identity and token set
// resolved in auth middleware, before acting on behalf of anybody.
func ResolveOriginalAuthContext(c *gin.Context) (string, common.TokenList, error) {
	claimsAny, ok := c.Get(userDataKey)
	if !ok {
		return "", nil, fmt.Errorf("missing user claims in request context")
	}

	claims, ok := claimsAny.(*common.UserClaims)
	if !ok || claims == nil {
		return "", nil, fmt.Errorf("invalid user claims in request context")
	}

	userEmail := authenticatedEmailFromClaims(claims)
	if userEmail == "" {
		return "", nil, fmt.Errorf("missing user identity in claims")
	}

	tokensAny, ok := c.Get(userTokensKey)
	if !ok {
		return "", nil, fmt.Errorf("missing user tokens in request context")
	}

	tokens, ok := tokensAny.(common.TokenList)
	if !ok {
		return "", nil, fmt.Errorf("invalid user tokens in request context")
	}

	return userEmail, tokens, nil
}

// CombinedAuthMiddleware authenticates a request: which case its credential is (none, an API token os_mgt_…, an ID token, rejected, the provider away) is decided by golib's credential.Resolve, the same in every service of the platform; the answers here are this service's. An API token is resolved to its subject by tokenLookup, an ID token verified by oidcVerifier; then userTokenResolver resolves the caller's groups.
func CombinedAuthMiddleware(oidcVerifier *oidcauth.Verifier, tokenLookup TokenLookupFunc, userTokenResolver UserTokenResolverFunc, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// Allow preflight OPTIONS requests without authentication
		if c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Headers") != "" {
			log.Infof("Allowing pre-flight request without authentication")
			c.Next()
			return
		}

		if userTokenResolver == nil {
			log.Error("User token resolver function is not configured")
			ginweb.AbortError(c, ErrInternal)
			return
		}

		authHeader := c.GetHeader("Authorization")
		var tokens credential.Tokens
		if tokenLookup != nil {
			tokens = lookupTokens(tokenLookup)
		} else if raw, ok := authn.CutBearerPrefix(authHeader); ok && strings.HasPrefix(raw, apiTokenPrefix) {
			log.Error("API token lookup function is not configured")
			ginweb.AbortError(c, ErrInternal)
			return
		}
		var verifier credential.Verifier
		if oidcVerifier != nil {
			verifier = oidcVerifier
		}

		p, err := credential.Resolve(ctx, authHeader, tokens, verifier)
		switch {
		case err == nil:
		case errors.Is(err, credential.ErrMissing):
			// The header value itself is never logged: a client sending a valid
			// token under an unexpected scheme would write its credential into
			// the log. The scheme alone is what makes this diagnosable.
			log.Warnf("Missing or invalid Authorization header (scheme %q)", authn.Scheme(authHeader))
			ginweb.AbortError(c, ErrUnauthenticated)
			return
		case errors.Is(err, token.ErrNotFound):
			log.Warn("Invalid API token, returning unauthorized")
			ginweb.AbortError(c, ErrAPITokenRejected)
			return
		case errors.Is(err, credential.ErrKeysUnavailable):
			// A token nobody can judge right now is not a rejected token.
			// 401 would tell the browser to drop its session and sign in
			// again — impossible while the provider is away, so the user
			// would loop through a login that cannot finish and read it as
			// our fault.
			log.Warnw("cannot verify tokens: the identity provider is unreachable", "error", err)
			ginweb.AbortError(c, ErrSignInUnavailable)
			return
		case errors.Is(err, credential.ErrRejected):
			log.Warnf("Failed to verify bearer token: %v", err)
			ginweb.AbortError(c, ErrSignInRejected)
			return
		case errors.Is(err, credential.ErrNoVerifier):
			log.Error("OIDC verifier is not configured")
			ginweb.AbortError(c, ErrInternal)
			return
		default:
			log.Warnf("storage error: %v", err)
			ginweb.AbortError(c, ErrInternal)
			return
		}

		// Recorded here, enforced by the route group: what counts as a write
		// is a property of the OPERATION, and only the REST routes can read
		// that off the HTTP method (see RejectWritesForReadOnlyTokens). An
		// interactive login carries no read-only flag; a token does.
		ginweb.SetReadOnly(c, p.ReadOnly)
		if p.ViaAPIToken() {
			MarkViaAPIToken(c)
		}
		// For an API token the claims hold its subject as Email, not
		// PreferredUsername. Claims.Identity prefers the e-mail and the role
		// provider resolves groups by it, so a token filled into the wrong
		// claim authenticates cleanly and then belongs to no group — which
		// reads as a permissions problem and is an identity one. Tokens are
		// issued under the actor's e-mail (see api_tokens.go), so this puts
		// back exactly what was put in.
		claims := p.Claims

		resolvedTokens, err := userTokenResolver(ctx, claims)
		if err != nil {
			log.Warnf("failed to resolve user tokens: %v", err)
			ginweb.AbortError(c, ErrInternal)
			return
		}

		c.Set(userDataKey, claims)
		c.Set(userTokensKey, resolvedTokens)
		c.Next()
	}
}

// lookupTokens is a TokenLookupFunc as golib's credential.Tokens: a credential with the prefix os_mgt_ is this service's API token.
type lookupTokens TokenLookupFunc

func (lookupTokens) Owns(secret string) bool { return strings.HasPrefix(secret, apiTokenPrefix) }

func (f lookupTokens) Lookup(ctx context.Context, secret string) (*token.Record, error) {
	res, err := f(ctx, secret)
	if err != nil {
		return nil, err
	}
	if !res.Found {
		return nil, token.ErrNotFound
	}
	return &token.Record{Subject: res.Subject, ReadOnly: res.ReadOnly}, nil
}

// MarkViaAPIToken records that the request authenticated with an API token, which the API holds against acting on behalf of somebody and issuing further tokens.
func MarkViaAPIToken(c *gin.Context) { c.Set(viaAPITokenKey, true) }

// SetAuthenticated records an authenticated caller and the tokens resolved for
// it, as the auth middlewares do; EffectiveAuthMiddleware reads them. For an
// authentication that lives elsewhere — the development auth.
func SetAuthenticated(c *gin.Context, claims *common.UserClaims, tokens common.TokenList) {
	c.Set(userDataKey, claims)
	c.Set(userTokensKey, tokens)
}
