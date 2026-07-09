package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// CreateProjectRequest contains fields for creating a project request.
type CreateProjectRequest struct {
	Quota               common.ProjectQuota     `json:"quota" binding:"required"`
	Reason              string                  `json:"reason" binding:"required"`
	TerminationDate     string                  `json:"termination_date" binding:"required"`
	AuthorizedUsers     []common.AuthorizedUser `json:"authorized_users"`
	FundingDelegationID string                  `json:"funding_delegation_id" binding:"required"`
}

// UpdateProjectRequest contains fields for proposing changes to a project request.
type UpdateProjectRequest struct {
	Quota           *common.ProjectQuota     `json:"quota"`
	TerminationDate *string                  `json:"termination_date"`
	AuthorizedUsers *[]common.AuthorizedUser `json:"authorized_users"`
}

// ApproveProjectRequest contains fields for approving a request.
type ApproveProjectRequest struct {
	DelegationID  string               `json:"delegation_id" binding:"required"`
	ModifiedQuota *common.ProjectQuota `json:"modified_quota"`
}

// RejectProjectRequest contains optional reason for rejection.
type RejectProjectRequest struct {
	Reason *string `json:"reason"`
}

// PromoteProjectRequest contains fields for promoting an openstack_only project to a
// managed project. Only root admins may call this endpoint. The caller supplies the
// owner's tokens so the service can resolve which delegations the owner is eligible
// for; the selected funding delegation must be among them and must have sufficient
// remaining capacity for the effective quota. The existing OpenStack project is then
// adopted by the reconciler on its next run.
type PromoteProjectRequest struct {
	// OwnerTokens are the effective tokens of the person who owns the OpenStack project.
	// The service resolves eligible delegations against these tokens.
	OwnerTokens         common.TokenList        `json:"owner_tokens" binding:"required"`
	FundingDelegationID string                  `json:"funding_delegation_id" binding:"required"`
	Reason              string                  `json:"reason" binding:"required"`
	TerminationDate     string                  `json:"termination_date" binding:"required"`
	AuthorizedUsers     []common.AuthorizedUser `json:"authorized_users"`
	// Quota overrides the project's current OpenStack quota. When omitted or empty,
	// the project's existing quota is kept unchanged.
	Quota common.ProjectQuota `json:"quota,omitempty"`
}

// getProject returns a single project by ID if the caller is authorized to view it.
//
//	@Summary		Get project
//	@Description	Fetches a single project by ID. Accessible by the requester or any delegation manager in the funding ancestry chain.
//	@Tags			projects
//	@Produce		json
//	@Security		Bearer
//	@Param			id	path		string	true	"Project ID"
//	@Success		200	{object}	common.Project	"The project."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				getProject
//	@Router			/v1/projects/{id} [get]
func getProject(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")

		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}

		req, err := svc.GetProjectByID(id, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		if req == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}

		c.JSON(http.StatusOK, req)
	}
}

// listMyProjects returns projects created by the current user.
//
// @Summary      List my projects
// @Description  Retrieves projects created by the current user.
// @Tags         projects
// @Produce      json
// @Security     Bearer
// @Param        limit   query   int     false   "Maximum number of entries to return" default(100)
// @Param        offset  query   int     false   "Offset into the result set" default(0)
// @Success      200     {array} common.Project "List of projects."
// @Failure      401     {object} map[string]any "Unauthorized."
// @Failure      500     {object} map[string]any "Internal server error."
// @ID           listMyProjects
// @Router       /v1/projects/mine [get]
func listMyProjects(cfg ProjectAPIConfig) gin.HandlerFunc {
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

		projects, err := svc.ListProjectsBy(auth.UserEmail, limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, projects)
	}
}

// listProjectsToManage returns projects the current user can approve/manage.
//
// @Summary      List projects to manage
// @Description  Retrieves projects the current user can approve or manage.
// @Tags         projects
// @Produce      json
// @Security     Bearer
// @Param        limit   query   int     false   "Maximum number of entries to return" default(100)
// @Param        offset  query   int     false   "Offset into the result set" default(0)
// @Success      200     {array} common.Project "List of projects."
// @Failure      401     {object} map[string]any "Unauthorized."
// @Failure      500     {object} map[string]any "Internal server error."
// @ID           listProjectsToManage
// @Router       /v1/projects/manage [get]
func listProjectsToManage(cfg ProjectAPIConfig) gin.HandlerFunc {
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

		projects, err := svc.ListProjectsManagedBy(auth.UserEmail, auth.EffectiveTokens, limit, offset)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, projects)
	}
}

// createProject creates a new project.
//
//	@Summary		Create project
//	@Description	Creates a new project. Project starts in 'pending' state.
//	@Tags			projects
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		CreateProjectRequest	true	"Project creation data"
//	@Success		201		{object}	common.Project	"Created project."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				createProject
//	@Router			/v1/projects [post]
func createProject(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req CreateProjectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		project, err := svc.CreateProject(req, auth.UserEmail, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusCreated, project)
	}
}

// updateProject proposes changes to an existing project.
//
//	@Summary		Update project
//	@Description	Proposes changes to an approved project. Project moves to 'change_pending' state.
//	@Tags			projects
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string				true	"Project ID"
//	@Param			request	body		UpdateProjectRequest	true	"Project update data"
//	@Success		200		{object}	common.Project	"Updated project."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				updateProject
//	@Router			/v1/projects/{id} [put]
func updateProject(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req UpdateProjectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.UpdateProject(id, req, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}

// approveProject approves a pending or change_pending project.
//
//	@Summary		Approve project
//	@Description	Approves a project and allocates resources from the specified delegation. Project moves to 'approved' state.
//	@Tags			projects
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string				true	"Project ID"
//	@Param			approval	body	ApproveProjectRequest	true	"Approval data"
//	@Success		200		{object}	common.Project	"Approved project."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				approveProject
//	@Router			/v1/projects/{id}/approve [post]
func approveProject(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req ApproveProjectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.ApproveProject(id, req, auth.UserEmail, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}

// rejectProject rejects a pending or change_pending project.
//
//	@Summary		Reject project
//	@Description	Rejects a project. Project moves to 'rejected' or 'change_rejected' state.
//	@Tags			projects
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string				true	"Project ID"
//	@Param			rejection	body	RejectProjectRequest	false	"Rejection reason"
//	@Success		200		{object}	common.Project	"Rejected project."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				rejectProject
//	@Router			/v1/projects/{id}/reject [post]
func rejectProject(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req RejectProjectRequest
		_ = c.ShouldBindJSON(&req)

		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.RejectProject(id, req, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}

// markProjectForPromotion marks an openstack_only project for adoption on the next reconcile run.
//
//	@Summary		Promote openstack_only project (root admin only)
//	@Description	Root admins only. Marks an openstack_only project for promotion to a managed project. The caller supplies the owner's tokens so the service can verify the selected funding delegation is eligible for the owner and has sufficient remaining capacity. The reconciler adopts the existing OpenStack project on its next run and transitions the record to 'pending' for normal approval.
//	@Tags			projects
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string				true	"Project ID"
//	@Param			request	body		PromoteProjectRequest	true	"Promotion data"
//	@Success		200		{object}	common.Project	"Updated project (status still openstack_only, promote_on_reconcile flag set)."
//	@Failure		400		{object}	map[string]any	"Bad request or insufficient capacity."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden — caller is not a root admin, or project is not openstack_only."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				markProjectForPromotion
//	@Router			/v1/projects/{id}/promote [post]
func markProjectForPromotion(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		var req PromoteProjectRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.MarkProjectForPromotion(id, req, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}

// releaseProject releases resources back to the funding delegation.
//
//	@Summary		Release project
//	@Description	Releases an approved project, returning resources to the funding delegation. Project moves to 'released' state.
//	@Tags			projects
//	@Security		Bearer
//	@Param			id	path	string	true	"Project ID"
//	@Success		200	{object}	common.Project	"Released project."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				releaseProject
//	@Router			/v1/projects/{id}/release [post]
func releaseProject(cfg ProjectAPIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		id := c.Param("id")
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		updated, err := svc.ReleaseProject(id, auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, updated)
	}
}
