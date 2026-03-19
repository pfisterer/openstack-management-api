package webserver

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	goset "github.com/hashicorp/go-set"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Identity represents a user or group in the identity catalog.
type Identity struct {
	ID     string           `json:"id"`
	Label  string           `json:"label"`
	Email  string           `json:"email"`
	Tokens common.TokenList `json:"tokens"`
}

// setRoleSwitchGroupRequest defines the payload for selecting a temporary group override.
type setRoleSwitchGroupRequest struct {
	GroupToken string `json:"group_token" binding:"required"`
}

// roleSwitchStateResponse describes the current role switch context.
type roleSwitchStateResponse struct {
	Enabled            bool             `json:"enabled"`
	Allowed            bool             `json:"allowed"`
	ActiveIdentity     Identity         `json:"active_identity"`
	OriginalTokens     common.TokenList `json:"original_tokens"`
	EffectiveTokens    common.TokenList `json:"effective_tokens"`
	OverrideGroupToken *string          `json:"override_group_token"`
}

// NormalizeGroupToken normalizes a raw group token into canonical `group:<name>` form.
//
// Parameter:
// - raw: Input token value from config, claim, or request.
//
// Returns:
// - Canonical `group:<name>` token when input is valid.
// - Empty string when input is blank or has an unsupported token shape.
func NormalizeGroupToken(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}

	if strings.HasPrefix(value, "group:") {
		return value
	}

	if strings.Contains(value, ":") {
		return ""
	}

	return "group:" + value
}

// strictGroupTokenSet builds a set from configuration values and enforces strict
// canonical shape. Any non-empty non-group token is treated as invalid input.
func strictGroupTokenSet(values common.TokenList) (*goset.Set[string], bool) {
	set := goset.New[string](len(values))
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		if normalized == "" || !strings.HasPrefix(normalized, "group:") {
			return nil, false
		}
		if normalized == "group:" {
			return nil, false
		}
		set.Insert(normalized)
	}
	return set, true
}

// userGroupTokenSet builds a set of valid group tokens from user tokens.
// Non-group tokens are ignored.
func userGroupTokenSet(values common.TokenList) *goset.Set[string] {
	set := goset.New[string](len(values))
	for _, value := range values {
		normalized := strings.TrimSpace(value)
		if strings.HasPrefix(normalized, "group:") && normalized != "group:" {
			set.Insert(normalized)
		}
	}
	return set
}

// canUseRoleSwitch checks whether any user token matches configured role-switch groups.
//
// Parameters:
// - userTokens: Effective or original tokens resolved for the authenticated user.
// - allowedGroups: Role-switch allowlist from configuration.
//
// Returns:
// - true when a user group token exactly matches an allowed group token.
// - false when no match exists, inputs are empty, or allowedGroups contain invalid entries.
func canUseRoleSwitch(userTokens common.TokenList, allowedGroups common.TokenList) bool {
	if len(allowedGroups) == 0 || len(userTokens) == 0 {
		return false
	}

	allowedKeys, ok := strictGroupTokenSet(allowedGroups)
	if ok {
		userKeys := userGroupTokenSet(userTokens)
		return !allowedKeys.Intersect(userKeys).Empty()
	}
	return false
}

func buildRoleSwitchStateResponse(c *gin.Context, cfg ResourceAPIConfig, allowed bool) (roleSwitchStateResponse, error) {
	svc := cfg.Service
	userEmail, userTokens, err := ResolveEffectiveAuthContext(c, svc)
	if err != nil {
		return roleSwitchStateResponse{}, err
	}
	_, originalTokens, err := ResolveOriginalAuthContext(c)
	if err != nil {
		return roleSwitchStateResponse{}, err
	}

	activeIdentity := Identity{
		ID:     userEmail,
		Label:  userEmail,
		Email:  userEmail,
		Tokens: userTokens,
	}

	var overrideGroupToken *string
	if override := svc.GetUserGroupSwitchForActor(userEmail); override != nil {
		overrideGroupToken = override
	}

	return roleSwitchStateResponse{
		Enabled:            len(cfg.RoleSwitchGroups) > 0,
		Allowed:            allowed,
		ActiveIdentity:     activeIdentity,
		OriginalTokens:     originalTokens,
		EffectiveTokens:    userTokens,
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
func getRoleSwitch(cfg ResourceAPIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		_, originalTokens, err := ResolveOriginalAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		allowed := canUseRoleSwitch(originalTokens, cfg.RoleSwitchGroups)
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
func setRoleSwitch(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		actorEmail, originalTokens, err := ResolveOriginalAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		if !canUseRoleSwitch(originalTokens, cfg.RoleSwitchGroups) {
			c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
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
func clearRoleSwitch(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		actorEmail, originalTokens, err := ResolveOriginalAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		if !canUseRoleSwitch(originalTokens, cfg.RoleSwitchGroups) {
			c.JSON(http.StatusForbidden, gin.H{"error": "forbidden"})
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
