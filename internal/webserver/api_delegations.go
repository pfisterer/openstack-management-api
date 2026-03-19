package webserver

import (
	"fmt"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// DelegationStrategy constants define how resources are allocated.
const (
	DelegationStrategyPool      = "pool"      // Resources pooled and allocated on-demand
	DelegationStrategyAllowance = "allowance" // Fixed allowance per user/group
)

// ResourceQuota represents resource limits and usage by resource ID.
type ResourceQuota map[string]int

// Resources contains limits and current usage.
type Resources struct {
	Limit ResourceQuota  `json:"limit"`
	Usage *ResourceQuota `json:"usage,omitempty"`
}

// Delegation represents an admin group that can allocate resources.
type Delegation struct {
	ID                 string           `json:"id"`
	Name               string           `json:"name"`
	ParentID           *string          `json:"parent_id"`
	CanDelegate        bool             `json:"can_delegate"`
	DelegationStrategy string           `json:"delegation_strategy" binding:"oneof=pool allowance"`
	DelegationScope    common.TokenList `json:"delegation_scope"`
	Resources          Resources        `json:"resources"`
	CreatedBy          string           `json:"created_by"`
	CreatedAt          string           `json:"created_at"`
	EndDate            *string          `json:"end_date"`
}

// CreateDelegationRequest contains fields for creating a delegation.
type CreateDelegationRequest struct {
	Name               string           `json:"name" binding:"required"`
	DelegationScope    common.TokenList `json:"delegation_scope" binding:"required"`
	DelegationStrategy string           `json:"delegation_strategy" binding:"required,oneof=pool allowance"`
	Resources          Resources        `json:"resources" binding:"required"`
	EndDate            *string          `json:"end_date"`
	ParentID           *string          `json:"parent_id"`
	CanDelegate        bool             `json:"can_delegate"`
}

// Validate checks if the delegation strategy is valid.
func (r *CreateDelegationRequest) Validate() error {
	if r.DelegationStrategy != DelegationStrategyPool && r.DelegationStrategy != DelegationStrategyAllowance {
		return fmt.Errorf("delegation_strategy must be '%s' or '%s'", DelegationStrategyPool, DelegationStrategyAllowance)
	}
	return nil
}

// UpdateDelegationRequest contains fields for updating a delegation.
type UpdateDelegationRequest struct {
	Name               *string           `json:"name"`
	DelegationScope    *common.TokenList `json:"delegation_scope"`
	DelegationStrategy *string           `json:"delegation_strategy"`
	Resources          *Resources        `json:"resources"`
	EndDate            *string           `json:"end_date"`
}

// listDelegationsToMe returns delegations where the user can receive resources.
//
// This endpoint returns all delegations where the authenticated user's tokens match the delegation_scope.
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
//	@Success		200	{array}	Delegation	"List of delegations."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				listDelegationsToMe
//	@Router			/v1/delegations/delegated-to-me [get]
func listDelegationsToMe(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, offset, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}
		_, userTokens, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		delegations, err := svc.GetDelegationsFor(userTokens, limit, offset)
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
// @Success 200 {array} Delegation "List of delegations."
// @Failure 401 {object} map[string]any "Unauthorized."
// @Failure 500 {object} map[string]any "Internal server error."
// @ID listDelegationsMadeByMe
// @Router /v1/delegations/made-by-me [get]
func listDelegationsMadeByMe(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, offset, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}
		_, userTokens, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		delegations, err := svc.GetDelegationsBy(userTokens, limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, delegations)
	}
}

// createDelegation creates a new resource delegation.
//
//	@Summary		Create delegation
//	@Description	Creates a new delegation (admin group) for resource management.
//	@Tags			delegations
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			delegation	body		CreateDelegationRequest	true	"Delegation creation data"
//	@Success		201			{object}	Delegation	"Created delegation."
//	@Failure		400			{object}	map[string]any	"Bad request."
//	@Failure		401			{object}	map[string]any	"Unauthorized."
//	@Failure		403			{object}	map[string]any	"Forbidden."
//	@Failure		500			{object}	map[string]any	"Internal server error."
//	@ID				createDelegation
//	@Router			/v1/delegations [post]
func createDelegation(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req CreateDelegationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		userEmail, _, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}

		delegation, err := svc.CreateDelegation(req, userEmail)
		if err != nil {
			status := http.StatusBadRequest
			if err.Error() == "forbidden" {
				status = http.StatusForbidden
			}
			c.JSON(status, gin.H{"error": err.Error()})
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
//	@Success		200			{object}	Delegation	"Updated delegation."
//	@Failure		400			{object}	map[string]any	"Bad request."
//	@Failure		401			{object}	map[string]any	"Unauthorized."
//	@Failure		403			{object}	map[string]any	"Forbidden."
//	@Failure		404			{object}	map[string]any	"Not found."
//	@Failure		500			{object}	map[string]any	"Internal server error."
//	@ID				updateDelegation
//	@Router			/v1/delegations/{id} [put]
func updateDelegation(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req UpdateDelegationRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		userEmail, _, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}

		updated, err := svc.UpdateDelegation(id, req, userEmail)
		if err != nil {
			status := http.StatusBadRequest
			if err.Error() == "group not found" {
				status = http.StatusNotFound
			}
			if err.Error() == "forbidden" {
				status = http.StatusForbidden
			}
			c.JSON(status, gin.H{"error": err.Error()})
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
func deleteDelegation(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		userEmail, _, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		err = svc.DeleteDelegation(id, userEmail)
		if err != nil {

			status := http.StatusBadRequest
			if err.Error() == "group not found" {
				status = http.StatusNotFound
			}
			if err.Error() == "forbidden" {
				status = http.StatusForbidden
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
