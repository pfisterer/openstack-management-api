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
	"go.uber.org/zap"
)

const apiTokenPrefix = "os_mgt_"
const userDataKey = "__api_userData"
const userTokensKey = "__api_userTokens"

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
	if claims == nil {
		return ""
	}

	userEmail := strings.TrimSpace(claims.Email)
	if userEmail == "" {
		userEmail = strings.TrimSpace(claims.PreferredUsername)
	}
	if userEmail == "" {
		userEmail = strings.TrimSpace(claims.Subject)
	}
	return userEmail
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

// ResolveEffectiveAuthContext returns the authenticated identity and effective
// token set after applying actor-specific role-switch overrides.
func ResolveEffectiveAuthContext(c *gin.Context, svc ResourceAPIService) (string, common.TokenList, error) {
	if svc == nil {
		return "", nil, fmt.Errorf("missing resource service")
	}

	userEmail, originalTokens, err := ResolveOriginalAuthContext(c)
	if err != nil {
		return "", nil, err
	}

	return userEmail, svc.ResolveEffectiveUserTokens(userEmail, originalTokens), nil
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
		if !strings.HasPrefix(token, "group:") {
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
		claims := &common.UserClaims{
			Email: "root.admin@uni.example",
			Name:  "Root Admin",
		}
		c.Set(userDataKey, claims)
		c.Set(userTokensKey, common.TokenList{"group:root_uni"})
		c.Next()
	}
}
