package webserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc"
	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/mockdata"
	"go.uber.org/zap"
)

const apiTokenPrefix = "os_mgt_"
const userDataKey = "__api_userData"
const userTokensKey = "__api_userTokens"
const authContextKey = "__api_authContext"

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
func EffectiveAuthMiddleware(svc ProjectAPIService) gin.HandlerFunc {
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
type OIDCVerifierConfig struct {
	IssuerURL string
	ClientID  string
}

// oidcAuthVerifier manages the OIDC token verification process.
type oidcAuthVerifier struct {
	Config   OIDCVerifierConfig
	Verifier *oidc.IDTokenVerifier
	Logger   *zap.SugaredLogger
}

// NewOIDCAuthVerifier initializes a new oidcAuthVerifier.
// It sets up the ID token verifier using the issuer URL and client ID.
func NewOIDCAuthVerifier(cfg OIDCVerifierConfig, log *zap.SugaredLogger) (*oidcAuthVerifier, error) {
	ctx := context.Background()
	// Discover the OIDC provider's configuration from the issuer URL
	// This fetches the JWKS endpoint and other metadata needed for verification.
	provider, err := oidc.NewProvider(ctx, cfg.IssuerURL)
	if err != nil {
		return nil, fmt.Errorf("failed to create OIDC provider for issuer '%s': %w", cfg.IssuerURL, err)
	}

	// Configure the ID token verifier.
	// The ClientID here acts as the expected audience (aud claim) for the token.
	oidcConfig := &oidc.Config{
		ClientID: cfg.ClientID,
	}
	verifier := provider.Verifier(oidcConfig)

	return &oidcAuthVerifier{
		Config:   cfg,
		Verifier: verifier,
		Logger:   log,
	}, nil
}

func authenticatedEmailFromClaims(claims *common.UserClaims) string {
	return claims.ResolveEmail()
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

func (m *oidcAuthVerifier) verifyBearerToken(ctx context.Context, rawIDToken string) (*common.UserClaims, error) {
	if m == nil || m.Verifier == nil {
		return nil, fmt.Errorf("oidc verifier is not configured")
	}

	// Verify the ID token's signature, issuer, audience, and expiry.
	idToken, err := m.Verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, err
	}

	if idToken.Expiry.Before(time.Now()) {
		return nil, fmt.Errorf("token expired")
	}

	var claims common.UserClaims
	if err := idToken.Claims(&claims); err != nil {
		return nil, fmt.Errorf("parse user claims from token: %w", err)
	}

	return &claims, nil
}

func CombinedAuthMiddleware(oidcVerifier *oidcAuthVerifier, tokenLookup common.TokenLookupFunc, userTokenResolver common.UserTokenResolverFunc, log *zap.SugaredLogger) gin.HandlerFunc {
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

		// Remove the bearer prefix from the Authorization header (if present)
		const bearerPrefix = "Bearer "
		tokenString, ok := strings.CutPrefix(authHeader, bearerPrefix)
		if !ok {
			log.Warnf("Missing or invalid Authorization header: %s", authHeader)
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

			// Check whether the operation is GET (read-only) and the token is read-only
			if c.Request.Method != http.MethodGet && token.ReadOnly {
				log.Warnf("Attempt to use read-only token for non-GET operation: %s %s", c.Request.Method, c.Request.URL.Path)
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "token is read-only"})
				return
			}

			claims = &common.UserClaims{
				PreferredUsername: token.Username,
			}
		} else {
			if oidcVerifier == nil {
				log.Error("OIDC verifier is not configured")
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
				return
			}

			verifiedClaims, err := oidcVerifier.verifyBearerToken(ctx, tokenString)
			if err != nil {
				log.Warnf("Failed to verify bearer token: %v", err)
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
				return
			}
			claims = verifiedClaims
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
		identities, _, _, _ := mockdata.DefaultMockResourceState()
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
		c.Set(userDataKey, claims)
		c.Set(userTokensKey, userTokens)
		c.Next()
	}
}
