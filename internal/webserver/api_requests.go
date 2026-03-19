package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// Request represents a resource allocation request.
type Request struct {
	ID                 string           `json:"id"`
	Status             string           `json:"status"`
	RequesterTokens    common.TokenList `json:"requester_tokens"`
	Resources          ResourceQuota    `json:"resources"`
	Reason             string           `json:"reason"`
	FundedBy           *string          `json:"funded_by"`
	Pending            *PendingChanges  `json:"pending"`
	TerminationDate    string           `json:"termination_date"`
	AuthorizedUsers    []AuthorizedUser `json:"authorized_users"`
	PotentialFunderIDs []string         `json:"potential_funder_ids,omitempty"`
	History            []HistoryEntry   `json:"history"`
}

// PendingChanges contains proposed modifications to a request.
type PendingChanges struct {
	Quota           *ResourceQuota    `json:"quota,omitempty"`
	TerminationDate *string           `json:"termination_date,omitempty"`
	AuthorizedUsers *[]AuthorizedUser `json:"authorized_users,omitempty"`
}

// AuthorizedUser represents a user/group authorization entry with separate source and target roles.
type AuthorizedUser struct {
	Token         string `json:"token"`
	GroupRole     string `json:"group_role"`
	OpenstackRole string `json:"openstack_role"`
}

// HistoryEntry records a change to a request.
type HistoryEntry struct {
	Timestamp           string         `json:"timestamp"`
	Event               string         `json:"event"`
	Actor               string         `json:"actor"`
	Group               *string        `json:"group,omitempty"`
	StatusFrom          *string        `json:"status_from,omitempty"`
	StatusTo            string         `json:"status_to"`
	QuotaFrom           *ResourceQuota `json:"quota_from,omitempty"`
	QuotaTo             *ResourceQuota `json:"quota_to,omitempty"`
	TerminationDate     *string        `json:"termination_date,omitempty"`
	TerminationDateFrom *string        `json:"termination_date_from,omitempty"`
	TerminationDateTo   *string        `json:"termination_date_to,omitempty"`
	Reason              *string        `json:"reason,omitempty"`
}

// CreateRequestRequest contains fields for creating a resource request.
type CreateRequestRequest struct {
	Resources       ResourceQuota    `json:"resources" binding:"required"`
	Reason          string           `json:"reason" binding:"required"`
	TerminationDate string           `json:"termination_date" binding:"required"`
	AuthorizedUsers []AuthorizedUser `json:"authorized_users"`
}

// UpdateRequestRequest contains fields for proposing changes to a request.
type UpdateRequestRequest struct {
	Resources       *ResourceQuota    `json:"resources"`
	TerminationDate *string           `json:"termination_date"`
	AuthorizedUsers *[]AuthorizedUser `json:"authorized_users"`
}

// ApproveRequestRequest contains fields for approving a request.
type ApproveRequestRequest struct {
	DelegationID  string         `json:"delegation_id" binding:"required"`
	ModifiedQuota *ResourceQuota `json:"modified_quota"`
}

// RejectRequestRequest contains optional reason for rejection.
type RejectRequestRequest struct {
	Reason *string `json:"reason"`
}

// listRequests returns resource requests filtered by mode.
//
//	@Summary		List requests
//	@Description	Retrieves resource requests. Mode 'manage' returns requests the user can approve, 'mine' returns user's own requests.
//	@Tags			requests
//	@Produce		json
//	@Security		Bearer
//	@Param			mode	query		string	false	"Filter mode: 'manage' or 'mine'"	default(manage)
//	@Param			limit	query		int		false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int		false	"Offset into the result set" default(0)
//	@Success		200		{array}	Request	"List of requests."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				listRequests
//	@Router			/v1/requests [get]
func listRequests(cfg ResourceAPIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		queryMode := c.DefaultQuery("mode", "manage")
		if queryMode != "manage" && queryMode != "mine" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid mode parameter"})
			return
		}

		// Parse pagination parameters with defaults
		limit, offset, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}

		// Resolve user context for authorization
		userEmail, userTokens, err := ResolveEffectiveAuthContext(c, cfg.Service)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}

		// Fetch requests based on mode: 'mine' for user's own requests, 'manage' for requests user can approve.
		var requests []Request

		if queryMode == "mine" {
			requests, err = cfg.Service.ListRequestsBy(userEmail, limit, offset)
		} else {
			requests, err = cfg.Service.ListRequestsManagedBy(userEmail, userTokens, limit, offset)
		}

		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, requests)
	}
}

// createRequest creates a new resource request.
//
//	@Summary		Create request
//	@Description	Creates a new resource allocation request. Request starts in 'pending' state.
//	@Tags			requests
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		CreateRequestRequest	true	"Request creation data"
//	@Success		201		{object}	Request	"Created request."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				createRequest
//	@Router			/v1/requests [post]
func createRequest(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req CreateRequestRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		userEmail, userTokens, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		request, err := svc.CreateRequest(req, userEmail, userEmail, userTokens)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusCreated, request)
	}
}

// updateRequest proposes changes to an existing request.
//
//	@Summary		Update request
//	@Description	Proposes changes to an approved request. Request moves to 'change_pending' state.
//	@Tags			requests
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string				true	"Request ID"
//	@Param			request	body		UpdateRequestRequest	true	"Request update data"
//	@Success		200		{object}	Request	"Updated request."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				updateRequest
//	@Router			/v1/requests/{id} [put]
func updateRequest(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req UpdateRequestRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		userEmail, _, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.UpdateRequest(id, req, userEmail)
		if err != nil {
			status := http.StatusBadRequest
			if err.Error() == "invalid ID" {
				status = http.StatusNotFound
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}

// approveRequest approves a pending or change_pending request.
//
//	@Summary		Approve request
//	@Description	Approves a request and allocates resources from the specified delegation. Request moves to 'approved' state.
//	@Tags			requests
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string				true	"Request ID"
//	@Param			approval	body	ApproveRequestRequest	true	"Approval data"
//	@Success		200		{object}	Request	"Approved request."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				approveRequest
//	@Router			/v1/requests/{id}/approve [post]
func approveRequest(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req ApproveRequestRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		userEmail, userTokens, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.ApproveRequest(id, req, userEmail, userEmail, userTokens)
		if err != nil {
			status := http.StatusBadRequest
			if err.Error() == "request not found" || err.Error() == "delegation not found" {
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

// rejectRequest rejects a pending or change_pending request.
//
//	@Summary		Reject request
//	@Description	Rejects a request. Request moves to 'rejected' or 'change_rejected' state.
//	@Tags			requests
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string				true	"Request ID"
//	@Param			rejection	body	RejectRequestRequest	false	"Rejection reason"
//	@Success		200		{object}	Request	"Rejected request."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				rejectRequest
//	@Router			/v1/requests/{id}/reject [post]
func rejectRequest(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req RejectRequestRequest
		_ = c.ShouldBindJSON(&req)

		userEmail, _, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.RejectRequest(id, req, userEmail)
		if err != nil {
			status := http.StatusBadRequest
			if err.Error() == "request not found" {
				status = http.StatusNotFound
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}

// releaseRequest releases resources back to the funding delegation.
//
//	@Summary		Release request
//	@Description	Releases an approved request, returning resources to the funding delegation. Request moves to 'released' state.
//	@Tags			requests
//	@Security		Bearer
//	@Param			id	path	string	true	"Request ID"
//	@Success		200	{object}	Request	"Released request."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				releaseRequest
//	@Router			/v1/requests/{id}/release [post]
func releaseRequest(cfg ResourceAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		userEmail, _, err := ResolveEffectiveAuthContext(c, svc)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.ReleaseRequest(id, userEmail)
		if err != nil {
			status := http.StatusBadRequest
			if err.Error() == "request not found" {
				status = http.StatusNotFound
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}
