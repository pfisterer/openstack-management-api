package webserver

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/token"
	"go.uber.org/zap"
)

// Token is the API shape of an API token. Not the stored row: the columns are
// the shared module's to change, these field names are published — swag reads
// them into swagger.json, and the generated TypeScript client from that.
type Token struct {
	ID     uint   `json:"id" example:"1"`
	User   string `json:"user" example:"alice@example.edu"`
	Prefix string `json:"token_prefix" example:"os_mgt_ab12cd34"`
	// TokenString is set only in the response that creates a token. It is not
	// stored and cannot be recovered afterwards.
	TokenString string    `json:"token_string,omitempty" example:"os_mgt_abcdef123456"`
	ExpiresAt   time.Time `json:"expires_at" example:"2025-12-31T23:59:59Z"`
	ReadOnly    bool      `json:"read_only" example:"false"`
	CreatedAt   time.Time `json:"created_at" example:"2025-11-04T12:00:00Z"`
}

// TokensResponse is the list returned by GET /v1/tokens.
type TokensResponse struct {
	Tokens []Token `json:"tokens"`
}

// CreateTokenRequest is the body of POST /v1/tokens.
type CreateTokenRequest struct {
	ReadOnly bool `json:"read_only"`
}

func toAPIToken(rec token.Record) Token {
	return Token{
		ID:        rec.ID,
		User:      rec.Subject,
		Prefix:    rec.Prefix,
		ExpiresAt: rec.ExpiresAt,
		ReadOnly:  rec.ReadOnly,
		CreatedAt: rec.CreatedAt,
	}
}

// TokenConfig is what the token endpoints need.
type TokenConfig struct {
	// Service issues and checks this service's tokens. Nil disables the
	// endpoints, which is what the memory-only development mode wants.
	Service *token.Service
	// TTL is how long an issued token lives.
	TTL time.Duration
}

// RegisterTokenRoutes wires /v1/tokens. A nil Service registers nothing.
func RegisterTokenRoutes(v1 *gin.RouterGroup, cfg TokenConfig, log *zap.SugaredLogger) {
	if cfg.Service == nil {
		return
	}

	tokens := v1.Group("/tokens")
	{
		tokens.GET("", listTokens(cfg, log))
		tokens.POST("", createToken(cfg, log))
		tokens.DELETE("/:id", deleteToken(cfg, log))
	}
}

// tokenSubject is the identity a token belongs to: the ACTOR, never the
// effective identity.
//
// Everything else in this API scopes on auth.UserEmail, which becomes somebody
// else's address while a role switch is active — that is the point of the
// switch. A credential must not follow it. A token issued under an assumed
// identity would be a permanent authorization created from a temporary view,
// and it would keep working long after the switch was cleared.
func tokenSubject(c *gin.Context) (string, bool) {
	auth, err := mustGetAuthContext(c)
	if err != nil || auth.ActorEmail == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
		return "", false
	}
	return auth.ActorEmail, true
}

// listTokens returns the caller's API tokens.
// @Summary List API tokens
// @Description Retrieve all API tokens of the authenticated caller
// @Tags tokens
// @Produce json
// @Success 200 {object} TokensResponse
// @Failure 500 {object} map[string]string
// @Security ApiKeyAuth
// @ID listTokens
// @Router /v1/tokens [get]
func listTokens(cfg TokenConfig, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		subject, ok := tokenSubject(c)
		if !ok {
			return
		}

		records, err := cfg.Service.List(c.Request.Context(), subject)
		if err != nil {
			log.Errorw("listing tokens failed", "subject", subject, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve tokens"})
			return
		}

		out := make([]Token, 0, len(records))
		for _, rec := range records {
			out = append(out, toAPIToken(rec))
		}
		c.JSON(http.StatusOK, TokensResponse{Tokens: out})
	}
}

// createToken issues a new API token for the caller.
// @Summary Create an API token
// @Description Issue a new API token for the authenticated caller
// @Tags tokens
// @Accept json
// @Produce json
// @Param request body CreateTokenRequest true "Token options"
// @Success 201 {object} Token
// @Failure 400 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Security ApiKeyAuth
// @ID createToken
// @Router /v1/tokens [post]
func createToken(cfg TokenConfig, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		subject, ok := tokenSubject(c)
		if !ok {
			return
		}

		var req CreateTokenRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request"})
			return
		}

		issued, err := cfg.Service.Issue(c.Request.Context(), subject, cfg.TTL, req.ReadOnly)
		if err != nil {
			log.Errorw("issuing a token failed", "subject", subject, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create token"})
			return
		}

		out := toAPIToken(issued.Record)
		out.TokenString = issued.Secret
		c.JSON(http.StatusCreated, out)
	}
}

// deleteToken revokes one of the caller's API tokens.
// @Summary Revoke an API token
// @Description Delete one of the authenticated caller's API tokens
// @Tags tokens
// @Produce json
// @Param id path int true "ID of the token to revoke"
// @Success 200 {object} map[string]string
// @Failure 404 {object} map[string]string
// @Failure 500 {object} map[string]string
// @Security ApiKeyAuth
// @ID deleteToken
// @Router /v1/tokens/{id} [delete]
func deleteToken(cfg TokenConfig, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, err := strconv.ParseUint(c.Param("id"), 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid token id"})
			return
		}

		subject, ok := tokenSubject(c)
		if !ok {
			return
		}

		err = cfg.Service.Revoke(c.Request.Context(), subject, uint(id))
		switch {
		case errors.Is(err, token.ErrNotFound):
			// Not found and not yours give the same answer, so the response
			// cannot be used to discover which IDs exist.
			c.JSON(http.StatusNotFound, gin.H{"status": "not found"})
		case err != nil:
			log.Errorw("revoking a token failed", "subject", subject, "id", id, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete token"})
		default:
			c.JSON(http.StatusOK, gin.H{"status": "deleted"})
		}
	}
}
