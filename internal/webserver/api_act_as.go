package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// actAsResponse tells the UI whether the caller may act on behalf of another person and on whose behalf the request acted.
type actAsResponse struct {
	// MayActAs: the signed-in person may send X-Act-As (a root admin).
	MayActAs bool `json:"may_act_as"`
	// Actor is the signed-in person.
	Actor string `json:"actor"`
	// ActingAs is the person acted for; empty without X-Act-As.
	ActingAs string `json:"acting_as,omitempty"`
	// EffectiveTokens are the tokens the request was judged by: the person's while acting for them.
	EffectiveTokens common.TokenList `json:"effective_tokens"`
}

// getActAs answers who the caller is and acts as.
//
//	@Summary		Who the caller is and acts as
//	@Description	Whether the signed-in person may act on behalf of another person with the X-Act-As header, and on whose behalf this request acted.
//	@Tags			act-as
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	actAsResponse	"The caller and the person acted for."
//	@ID				getActAs
//	@Router			/v1/act-as [get]
func getActAs(cfg APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		resp := actAsResponse{
			MayActAs:        len(cfg.ActAsTokens) > 0 && common.NewTokenSet(common.CanonicalTokens(cfg.ActAsTokens)).ContainsAny(auth.OriginalTokens),
			Actor:           auth.ActorEmail,
			EffectiveTokens: auth.EffectiveTokens,
		}
		if auth.OnBehalf() {
			resp.ActingAs = auth.UserEmail
		}
		c.JSON(http.StatusOK, resp)
	}
}
