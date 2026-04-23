package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// SetEligibilityRuleRequest is the request body for PUT /v1/eligibility/:token.
type SetEligibilityRuleRequest struct {
	EligibleRequesters common.TokenList `json:"eligible_requesters" binding:"required"`
}

// listMyEligibilityRules returns all eligibility rules owned by the caller's tokens.
//
//	@Summary		List my eligibility rules
//	@Description	Returns all token eligibility rules where an owner token matches the caller's effective tokens.
//	@Tags			eligibility
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{array}		common.TokenEligibilityRule	"List of eligibility rules."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				listMyEligibilityRules
//	@Router			/v1/eligibility [get]
func listMyEligibilityRules(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		rules, err := svc.GetMyEligibilityRules(auth.EffectiveTokens)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, rules)
	}
}

// setEligibilityRule creates or replaces the eligibility rule for a token the caller owns.
//
//	@Summary		Set eligibility rule
//	@Description	Creates or replaces the list of requester tokens eligible to request projects from the given owner token. The caller must hold the owner token.
//	@Tags			eligibility
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			token	path		string						true	"Owner token (URL-encoded, e.g. group%3Adept_cs_admin)"
//	@Param			rule	body		SetEligibilityRuleRequest	true	"Eligible requesters"
//	@Success		200		{object}	common.TokenEligibilityRule	"Updated eligibility rule."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden — caller does not hold the owner token."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				setEligibilityRule
//	@Router			/v1/eligibility/{token} [put]
func setEligibilityRule(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		ownerToken := c.Param("token")
		var req SetEligibilityRuleRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		rule, err := svc.SetEligibilityRule(ownerToken, req.EligibleRequesters, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, rule)
	}
}

// deleteEligibilityRule removes the eligibility rule for a token the caller owns.
//
//	@Summary		Delete eligibility rule
//	@Description	Removes the eligibility rule for the given owner token. The caller must hold the owner token.
//	@Tags			eligibility
//	@Security		Bearer
//	@Param			token	path	string	true	"Owner token (URL-encoded)"
//	@Success		204	"No content."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				deleteEligibilityRule
//	@Router			/v1/eligibility/{token} [delete]
func deleteEligibilityRule(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		ownerToken := c.Param("token")
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		if err := svc.DeleteEligibilityRule(ownerToken, auth.UserEmail, auth.EffectiveTokens); err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
