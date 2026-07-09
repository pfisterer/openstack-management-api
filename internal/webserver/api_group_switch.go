package webserver

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

const groupTokenPrefix = "group:"

// setRoleSwitchGroupRequest defines the payload for selecting a temporary group override.
type setRoleSwitchGroupRequest struct {
	GroupToken string `json:"group_token" binding:"required"`
}

// roleSwitchStateResponse describes the current role switch context.
type roleSwitchStateResponse struct {
	Enabled            bool             `json:"enabled"`
	Allowed            bool             `json:"allowed"`
	ActiveIdentity     common.Identity  `json:"active_identity"`
	OriginalTokens     common.TokenList `json:"original_tokens"`
	EffectiveTokens    common.TokenList `json:"effective_tokens"`
	OverrideGroupToken *string          `json:"override_group_token"`
}

// NormalizeGroupToken normalizes a raw group token into canonical `group:<name>` form.
//
// Parameter:
// - raw: Input token value from config, claim, or project.
//
// Returns:
// - Canonical `group:<name>` token when input is valid.
// - Empty string when input is blank or has an unsupported token shape.
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

// canUseRoleSwitch reports whether the caller may perform a role switch: they must
// hold one of the configured role-switch tokens. Consistent with the other
// root-admin gates (requireRootAdmin / rootAdminTokens.ContainsAny), this matches
// ANY token type — user: or group: — via plain set membership, not group
// membership only. So a user granted the privilege by a bare user: token works,
// and a mixed allowlist no longer silently disables the feature.
func canUseRoleSwitch(userTokens common.TokenList, allowed common.TokenList) bool {
	if len(allowed) == 0 || len(userTokens) == 0 {
		return false
	}
	return common.NewTokenSet(allowed).ContainsAny(userTokens)
}

// requireRoleSwitch resolves the original auth context and checks the role-switch
// allowlist. Returns the actor email, original tokens, and true when the caller is
// permitted. Writes a 401/403 response and returns false when not permitted.
func requireRoleSwitch(c *gin.Context, cfg ProjectAPIConfig) (string, common.TokenList, bool) {
	auth, err := mustGetAuthContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
		return "", nil, false
	}
	if !canUseRoleSwitch(auth.OriginalTokens, cfg.RoleSwitchGroups) {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
		return "", nil, false
	}
	return auth.UserEmail, auth.OriginalTokens, true
}

func buildRoleSwitchStateResponse(c *gin.Context, cfg ProjectAPIConfig, allowed bool) (roleSwitchStateResponse, error) {
	auth, err := mustGetAuthContext(c)
	if err != nil {
		return roleSwitchStateResponse{}, err
	}

	activeIdentity := common.Identity{
		ID:     auth.UserEmail,
		Label:  auth.UserEmail,
		Email:  auth.UserEmail,
		Tokens: auth.EffectiveTokens,
	}

	var overrideGroupToken *string
	if override := cfg.Service.GetUserGroupSwitchForActor(auth.UserEmail); override != nil {
		overrideGroupToken = override
	}

	return roleSwitchStateResponse{
		Enabled:            len(cfg.RoleSwitchGroups) > 0,
		Allowed:            allowed,
		ActiveIdentity:     activeIdentity,
		OriginalTokens:     auth.OriginalTokens,
		EffectiveTokens:    auth.EffectiveTokens,
		OverrideGroupToken: overrideGroupToken,
	}, nil
}

// getRoleSwitch returns the current role-switch context for the authenticated user.
//
//	@Summary		Get role switch state
//	@Description	Returns whether role switching is enabled/allowed and the current effective/original token context.
//	@Tags			role-switch
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	roleSwitchStateResponse	"Current role-switch context."
//	@ID				getRoleSwitch
//	@Router			/v1/role-switch [get]
func getRoleSwitch(cfg ProjectAPIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		allowed := canUseRoleSwitch(auth.OriginalTokens, cfg.RoleSwitchGroups)
		response, err := buildRoleSwitchStateResponse(c, cfg, allowed)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

// setRoleSwitch sets a temporary group override for the authenticated actor.
//
//	@Summary		Set role switch group
//	@Description	Sets a temporary role-switch override group for the current actor if role switching is allowed.
//	@Tags			role-switch
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		setRoleSwitchGroupRequest	true	"Role-switch group selection request."
//	@Success		200		{object}	roleSwitchStateResponse	"Updated role-switch context."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@ID				setRoleSwitch
//	@Router			/v1/role-switch [put]
func setRoleSwitch(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		actorEmail, _, ok := requireRoleSwitch(c, cfg)
		if !ok {
			return
		}

		var req setRoleSwitchGroupRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if err := svc.SetUserGroupSwitchForActor(actorEmail, req.GroupToken); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		response, err := buildRoleSwitchStateResponse(c, cfg, true)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		c.JSON(http.StatusOK, response)
	}
}

// clearRoleSwitch clears the temporary group override for the authenticated actor.
//
//	@Summary		Clear role switch group
//	@Description	Clears the temporary role-switch override and restores the original token context.
//	@Tags			role-switch
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	roleSwitchStateResponse	"Updated role-switch context."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@ID				clearRoleSwitch
//	@Router			/v1/role-switch [delete]
func clearRoleSwitch(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		actorEmail, _, ok := requireRoleSwitch(c, cfg)
		if !ok {
			return
		}

		svc.ClearUserGroupSwitchForActor(actorEmail)
		response, err := buildRoleSwitchStateResponse(c, cfg, true)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		c.JSON(http.StatusOK, response)
	}
}
