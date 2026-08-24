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
	// ExpiresAt is null for a token that does not expire.
	//
	// A pointer, so that "no expiry" is null and not the year 1: Go's zero time
	// serialises to 0001-01-01T00:00:00Z, and a client comparing that against
	// now would read a permanent token as long expired.
	ExpiresAt *time.Time `json:"expires_at" example:"2025-12-31T23:59:59Z"`
	ReadOnly  bool       `json:"read_only" example:"false"`
	CreatedAt time.Time  `json:"created_at" example:"2025-11-04T12:00:00Z"`
	// Description is what the owner wrote down about this token. A memory aid,
	// never a permission.
	Description string `json:"description" example:"nightly quota report"`
	// LastUsedAt is when the token last authenticated a request, to the nearest
	// minute, or null if it never has — which is what makes revoking a
	// forgotten token safe to do.
	LastUsedAt *time.Time `json:"last_used_at" example:"2025-11-05T08:30:00Z"`
}

// nilIfZero maps Go's zero time to a JSON null. Both callers say something
// specific with it: no expiry, and never used.
func nilIfZero(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// TokensResponse is the list returned by GET /v1/tokens.
type TokensResponse struct {
	Tokens []Token `json:"tokens"`
}

// CreateTokenRequest is the body of POST /v1/tokens.
type CreateTokenRequest struct {
	ReadOnly bool `json:"read_only"`
	// Description is the owner's note about the token, at most 100 characters.
	Description string `json:"description" example:"nightly quota report"`
	// TTLHours is how long the token should live. Omitted or 0 means the
	// configured default; -1 means it never expires, and is refused unless the
	// deployment allows it — the same convention as token.NeverExpires, so
	// there is one way to say "no expiry" from the request down to the row.
	TTLHours int `json:"ttl_hours" example:"720"`
}

func toAPIToken(rec token.Record) Token {
	return Token{
		ID:          rec.ID,
		User:        rec.Subject,
		Prefix:      rec.Prefix,
		ExpiresAt:   nilIfZero(rec.ExpiresAt),
		ReadOnly:    rec.ReadOnly,
		CreatedAt:   rec.CreatedAt,
		Description: rec.Description,
		LastUsedAt:  nilIfZero(rec.LastUsedAt),
	}
}

// TokenConfig is what the token endpoints need.
type TokenConfig struct {
	// Service issues and checks this service's tokens. Nil disables the
	// endpoints, which is what the memory-only development mode wants.
	Service *token.Service
	// TTL is the lifetime policy: the default when a request names none, the
	// maximum it may ask for, and whether "no expiry" is on the table at all.
	// The table itself is the library's; these are this deployment's answers.
	TTL token.TTLPolicy
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

		ttl, err := cfg.TTL.Resolve(req.TTLHours)
		if err != nil {
			// Asked for a lifetime this deployment does not grant. The message
			// names the limit; none of this is a server fault.
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		issued, err := cfg.Service.Issue(c.Request.Context(), subject, token.IssueOptions{
			TTL:         ttl,
			ReadOnly:    req.ReadOnly,
			Description: req.Description,
		})
		// The description is the caller's text: too long is a 400, and it is
		// never echoed into the log.
		if errors.Is(err, token.ErrDescriptionTooLong) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
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
