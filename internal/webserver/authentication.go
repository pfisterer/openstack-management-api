package webserver

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/oidcauth"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"go.uber.org/zap"
)

const apiTokenPrefix = "os_mgt_"
const userDataKey = "__api_userData"
const userTokensKey = "__api_userTokens"
const authContextKey = "__api_authContext"

// The read-only-token rule (recording the flag, reading it, and refusing writes
// for it) lives in cloud-self-service-golib/ginweb, shared with the other
// services. This file records the flag via ginweb.SetReadOnly once it has
// resolved the token.

// AuthContext holds the resolved identity for a single request, set once by
// EffectiveAuthMiddleware and read by handlers via mustGetAuthContext.
//
// ActorEmail is the real authenticated caller (used for role-switch override
// bookkeeping). UserEmail is the EFFECTIVE identity used for scoping — it equals
// ActorEmail normally, but becomes the impersonated identity while an identity
// role switch is active, so email-scoped views ("my projects", created-by, …)
// reflect the assumed user.
type AuthContext struct {
	ActorEmail      string
	UserEmail       string
	OriginalTokens  common.TokenList
	EffectiveTokens common.TokenList
}

// EffectiveAuthMiddleware resolves both the original and effective token sets
// once per request and stores them in the Gin context. Must run after the auth
// middleware that populates userDataKey / userTokensKey.
func EffectiveAuthMiddleware(svc APIService) gin.HandlerFunc {
	return func(c *gin.Context) {
		actorEmail, originalTokens, err := ResolveOriginalAuthContext(c)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		effectiveTokens := svc.ResolveEffectiveUserTokens(actorEmail, originalTokens)
		effectiveEmail := svc.ResolveEffectiveEmail(actorEmail)
		c.Set(authContextKey, AuthContext{
			ActorEmail:      actorEmail,
			UserEmail:       effectiveEmail,
			OriginalTokens:  originalTokens,
			EffectiveTokens: effectiveTokens,
		})
		c.Next()
	}
}

// mustGetAuthContext returns the AuthContext set by EffectiveAuthMiddleware.
func mustGetAuthContext(c *gin.Context) (AuthContext, error) {
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
// resolved in auth middleware before any role-switch override is applied.
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

func CombinedAuthMiddleware(oidcVerifier *oidcauth.Verifier, tokenLookup common.TokenLookupFunc, userTokenResolver common.UserTokenResolverFunc, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()

		// Allow preflight OPTIONS requests without authentication
		if c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Headers") != "" {
			log.Infof("Allowing pre-flight request without authentication")
			c.Next()
			return
		}

		// Get the Authorization header
		authHeader := c.GetHeader("Authorization")

		tokenString, ok := authn.CutBearerPrefix(authHeader)
		if !ok {
			// The header value itself is never logged: a client sending a valid
			// token under an unexpected scheme would write its credential into
			// the log. The scheme alone is what makes this diagnosable.
			log.Warnf("Missing or invalid Authorization header (scheme %q)", authn.Scheme(authHeader))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing or invalid Authorization Bearer header"})
			return
		}

		if userTokenResolver == nil {
			log.Error("User token resolver function is not configured")
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}

		var claims *common.UserClaims

		// Check if token is an API key
		if strings.HasPrefix(tokenString, apiTokenPrefix) {
			if tokenLookup == nil {
				log.Error("API token lookup function is not configured")
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
				return
			}

			// Look up the token in storage
			token, err := tokenLookup(ctx, tokenString)
			if err != nil {
				log.Warnf("storage error: %v", err)
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
				return
			}

			// Check if a token was found
			if !token.Found {
				log.Warn("Invalid API token, got nil token, returning unauthorized")
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
				return
			}

			// Recorded here, enforced by the route group: what counts as a write
			// is a property of the OPERATION, and only the REST routes can read
			// that off the HTTP method (see RejectWritesForReadOnlyTokens).
			ginweb.SetReadOnly(c, token.ReadOnly)

			// Email, not PreferredUsername. Claims.Identity prefers the e-mail
			// and the role provider resolves groups by it, so a token filled
			// into the wrong claim authenticates cleanly and then belongs to no
			// group — which reads as a permissions problem and is an identity
			// one. Tokens are issued under the actor's e-mail (see
			// api_tokens.go), so this puts back exactly what was put in.
			claims = &common.UserClaims{
				Email: token.Subject,
			}
		} else {
			if oidcVerifier == nil {
				log.Error("OIDC verifier is not configured")
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
				return
			}

			// common.UserClaims is authn.Claims, which is what the verifier
			// returns — no conversion needed.
			verifiedClaims, err := oidcVerifier.Verify(ctx, tokenString)
			if err != nil {
				// A token nobody can judge right now is not a rejected token.
				// 401 would tell the browser to drop its session and sign in
				// again — impossible while the provider is away, so the user
				// would loop through a login that cannot finish and read it as
				// our fault.
				if errors.Is(err, oidcauth.ErrKeysUnavailable) {
					log.Warnw("cannot verify tokens: the identity provider is unreachable", "error", err)
					c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
						"error": "sign-in is temporarily unavailable: the identity provider cannot be reached",
					})
					return
				}
				log.Warnf("Failed to verify bearer token: %v", err)
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
				return
			}
			claims = verifiedClaims
			// An interactive login carries no read-only flag; a token does.
			ginweb.SetReadOnly(c, false)
		}

		resolvedTokens, err := userTokenResolver(ctx, claims)
		if err != nil {
			log.Warnf("failed to resolve user tokens: %v", err)
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			return
		}

		c.Set(userDataKey, claims)
		c.Set(userTokensKey, resolvedTokens)
		c.Next()
	}
}

// DummyAuthMiddleware injects a default user in group:uni_root for development/testing without SSO.
func DummyAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Get the dummy user email from header X-Dummy-Auth-User
		dev_user := c.GetHeader("X-Dummy-Auth-User")

		// If not set, default to the root
		if strings.TrimSpace(dev_user) == "" {
			dev_user = "root.admin@uni.example"
		}

		// Set the user in the context as if it was authenticated, with a token representing group membership for "uni_root".
		claims := &common.UserClaims{
			Email: dev_user,
			Name:  fmt.Sprintf("User: %s", dev_user),
		}

		//Resolve the tokens from the identies for the email given
		identities, _ := mockdata.DefaultMockTreeState()
		var userTokens common.TokenList = []string{}
		for _, identity := range identities {
			if identity.Email == dev_user {
				userTokens = identity.Tokens
				break
			}
		}

		// The dev email may not be one of the mock identities — the self-service UI
		// defaults dummy auth to real DHBW emails (e.g. dennis.pfisterer@dhbw.de),
		// which map to no tokens here and make every handler dead-end with
		// "no user tokens found". Fall back to the root mock identity's tokens, i.e.
		// the same behaviour as sending no X-Dummy-Auth-User header at all, so any
		// dev email can drive the full API. Dev-only (DummyAuth must be enabled).
		if len(userTokens) == 0 {
			for _, identity := range identities {
				if identity.Email == "root.admin@uni.example" {
					userTokens = identity.Tokens
					break
				}
			}
			fmt.Printf("DummyAuthMiddleware: dev user '%s' is not a mock identity; falling back to root tokens\n", dev_user)
		}

		// Set the claims and tokens in the context for downstream handlers to use.
		fmt.Printf("DummyAuthMiddleware: setting dummy auth for user '%s' with tokens: %v\n", dev_user, userTokens)
		ginweb.SetReadOnly(c, false)
		c.Set(userDataKey, claims)
		c.Set(userTokensKey, userTokens)
		c.Next()
	}
}
