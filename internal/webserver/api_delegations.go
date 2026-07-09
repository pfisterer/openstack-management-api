package webserver

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// CreateDelegationRequest contains fields for creating a delegation.
type CreateDelegationRequest struct {
	Name               string                  `json:"name" binding:"required"`
	AdminScope         common.TokenList        `json:"admin_scope" binding:"required"`
	DelegationStrategy string                  `json:"delegation_strategy" binding:"required,oneof=pool allowance"`
	Quota              common.ProjectResources `json:"quota" binding:"required"`
	EndDate            *string                 `json:"end_date"`
	ParentID           *string                 `json:"parent_id"`
	CanDelegate        bool                    `json:"can_delegate"`
}

// Validate checks if the delegation strategy is valid.
func (r *CreateDelegationRequest) Validate() error {
	if r.DelegationStrategy != common.DelegationStrategyPool && r.DelegationStrategy != common.DelegationStrategyAllowance {
		return fmt.Errorf("delegation_strategy must be '%s' or '%s'", common.DelegationStrategyPool, common.DelegationStrategyAllowance)
	}
	return nil
}

// UpdateDelegationRequest contains fields for updating a delegation.
type UpdateDelegationRequest struct {
	Name               *string                  `json:"name"`
	AdminScope         *common.TokenList        `json:"admin_scope"`
	DelegationStrategy *string                  `json:"delegation_strategy"`
	Quota              *common.ProjectResources `json:"quota"`
	EndDate            *string                  `json:"end_date"`
}

// listDelegationsToMe returns delegations where the user can receive resources.
//
// This endpoint returns all delegations where the authenticated user's tokens match the admin_scope.
// It includes all delegations the user is a member of, regardless of whether they have delegation rights (CanDelegate).
// Internally, this is implemented by calling GetDelegations with requireCanDelegate = false.
//
//	@Summary		Get delegations delegated to me
//	@Description	Retrieves delegations where the authenticated user's tokens match the delegation scope.
//	@Tags			delegations
//	@Produce		json
//	@Security		Bearer
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{array}		common.Delegation	"List of delegations."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				listDelegationsToMe
//	@Router			/v1/delegations/delegated-to-me [get]
func listDelegationsToMe(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, offset, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		delegations, err := svc.GetDelegationsDelegatedToMe(auth.EffectiveTokens, limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, delegations)
	}
}

// listDelegationsMadeByMe returns delegations created by the authenticated user.
//
// This endpoint returns all delegations where CreatedBy matches the user's email.
//
// @Summary Get delegations made by me
// @Description Retrieves delegations created by the authenticated user.
// @Tags delegations
// @Produce json
// @Security Bearer
// @Param limit query int false "Maximum number of entries to return" default(100)
// @Param offset query int false "Offset into the result set" default(0)
// @Success 200 {array} common.Delegation "List of delegations."
// @Failure 401 {object} map[string]any "Unauthorized."
// @Failure 500 {object} map[string]any "Internal server error."
// @ID listDelegationsMadeByMe
// @Router /v1/delegations/made-by-me [get]
func listDelegationsMadeByMe(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, offset, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		delegations, err := svc.GetDelegationsByAdminScope(auth.EffectiveTokens, limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, delegations)
	}
}

// listDelegationsEligibleForOwner returns delegations a specific owner may submit requests to.
// Only root admins may call this — used by the promote flow to show which delegations
// are valid funding choices for a given project owner.
//
//	@Summary		List delegations eligible for owner (root admin only)
//	@Description	Root admins only. Returns delegations the specified owner tokens may request projects from. Used by the promote flow.
//	@Tags			delegations
//	@Produce		json
//	@Security		Bearer
//	@Param			owner_token	query		[]string	true	"Owner token(s) to resolve eligible delegations for"	collectionFormat(multi)
//	@Success		200	{array}		common.Delegation	"List of delegations."
//	@Failure		400	{object}	map[string]any	"Bad request — no owner tokens supplied."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden — caller is not a root admin."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				listDelegationsEligibleForOwner
//	@Router			/v1/delegations/eligible-for-owner [get]
func listDelegationsEligibleForOwner(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		ownerTokens := c.QueryArray("owner_token")
		if len(ownerTokens) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "at least one owner_token query parameter is required"})
			return
		}
		limit, offset, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}
		delegations, err := svc.ListDelegationsEligibleForOwner(auth.EffectiveTokens, common.TokenList(ownerTokens), limit, offset)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, delegations)
	}
}

// listDelegationsEligibleForMe returns delegations the caller may submit project requests to.
//
//	@Summary		List delegations eligible for me
//	@Description	Returns delegations the authenticated user may request projects from, via eligibility rules or direct allowance membership.
//	@Tags			delegations
//	@Produce		json
//	@Security		Bearer
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{array}		common.Delegation	"List of delegations."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				listDelegationsEligibleForMe
//	@Router			/v1/delegations/eligible-for-me [get]
func listDelegationsEligibleForMe(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, offset, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		delegations, err := svc.ListDelegationsEligibleForMe(auth.EffectiveTokens, limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, delegations)
	}
}

// createDelegation creates a new project delegation.
//
//	@Summary		Create delegation
//	@Description	Creates a new delegation (admin group) for project management.
//	@Tags			delegations
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			delegation	body		CreateDelegationRequest	true	"Delegation creation data"
//	@Success		201			{object}	common.Delegation	"Created delegation."
//	@Failure		400			{object}	map[string]any	"Bad request."
//	@Failure		401			{object}	map[string]any	"Unauthorized."
//	@Failure		403			{object}	map[string]any	"Forbidden."
//	@Failure		500			{object}	map[string]any	"Internal server error."
//	@ID				createDelegation
//	@Router			/v1/delegations [post]
func createDelegation(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req CreateDelegationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}

		delegation, err := svc.CreateDelegation(req, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, delegation)
	}
}

// updateDelegation updates an existing delegation.
//
//	@Summary		Update delegation
//	@Description	Updates properties of an existing delegation.
//	@Tags			delegations
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id			path		string					true	"Delegation ID"
//	@Param			delegation	body		UpdateDelegationRequest	true	"Delegation update data"
//	@Success		200			{object}	common.Delegation	"Updated delegation."
//	@Failure		400			{object}	map[string]any	"Bad request."
//	@Failure		401			{object}	map[string]any	"Unauthorized."
//	@Failure		403			{object}	map[string]any	"Forbidden."
//	@Failure		404			{object}	map[string]any	"Not found."
//	@Failure		500			{object}	map[string]any	"Internal server error."
//	@ID				updateDelegation
//	@Router			/v1/delegations/{id} [put]
func updateDelegation(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req UpdateDelegationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}

		updated, err := svc.UpdateDelegation(id, req, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}

// deleteDelegation deletes a delegation and all its descendants.
//
//	@Summary		Delete delegation
//	@Description	Deletes a delegation and all child delegations recursively.
//	@Tags			delegations
//	@Security		Bearer
//	@Param			id	path	string	true	"Delegation ID"
//	@Success		204	"No content."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				deleteDelegation
//	@Router			/v1/delegations/{id} [delete]
func deleteDelegation(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		err = svc.DeleteDelegation(id, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
