package webserver

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/tree"
)

// getNode returns a single node by ID if the caller is authorized to view it.
//
//	@Summary		Get node
//	@Description	Fetches a single node (budget or project leaf). Accessible by the leaf owner, authorized users, eligible requesters of a budget, and managers of the node's ancestor chain.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			id	path		string	true	"Node ID"
//	@Success		200	{object}	tree.Node	"The node."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				getNode
//	@Router			/v1/nodes/{id} [get]
func getNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.GetNode(c.Param("id"), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		if node == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// listNodeChildren returns the direct children of a budget node.
//
//	@Summary		List node children
//	@Description	Returns the direct children (budgets and project leaves) of a budget. Only managers of the budget or its ancestors may list children.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string	true	"Parent node ID"
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{object}	tree.NodePage	"List of child nodes, with the total number of matches."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@ID				listNodeChildren
//	@Router			/v1/nodes/{id}/children [get]
func listNodeChildren(cfg APIConfig) gin.HandlerFunc {
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
		children, err := svc.ListChildren(c.Param("id"), auth.EffectiveTokens, limit, offset)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, children)
	}
}

// listMyNodes returns the project leaves owned by the current user.
//
//	@Summary		List my project leaves
//	@Description	Retrieves the project leaves owned by the current (effective) user.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{object}	tree.NodePage	"List of project leaves, with the total number of matches."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@ID				listMyNodes
//	@Router			/v1/nodes/mine [get]
func listMyNodes(cfg APIConfig) gin.HandlerFunc {
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
		nodes, err := svc.ListMine(auth.UserEmail, limit, offset)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, nodes)
	}
}

// listNodesToManage returns nodes awaiting a decision by the caller.
//
//	@Summary		List nodes to manage
//	@Description	Retrieves pending / change-pending nodes (budgets and leaves) and imported leaves hanging directly under the budgets the caller administers. Set scope=subtree to include everything below them, including requests addressed to the managers of delegated sub-budgets.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			scope	query		string	false	"direct (default) or subtree" Enums(direct, subtree)
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{object}	tree.NodePage	"List of nodes, with the total number of matches."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@ID				listNodesToManage
//	@Router			/v1/nodes/to-manage [get]
func listNodesToManage(cfg APIConfig) gin.HandlerFunc {
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
		scope := c.Query("scope")
		if scope != "" && scope != "direct" && scope != "subtree" {
			c.JSON(http.StatusBadRequest, gin.H{"error": `scope must be "direct" or "subtree"`})
			return
		}
		nodes, err := svc.ListToManage(auth.EffectiveTokens, scope == "subtree", limit, offset)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, nodes)
	}
}

// listMyBudgets returns the budgets the caller directly administers.
//
//	@Summary		List my budgets
//	@Description	Retrieves the budgets whose admin scope contains one of the caller's tokens ("budgets delegated to me"), with subtree usage attached.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{object}	tree.NodePage	"List of budgets, with the total number of matches."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@ID				listMyBudgets
//	@Router			/v1/nodes/my-budgets [get]
func listMyBudgets(cfg APIConfig) gin.HandlerFunc {
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
		nodes, err := svc.ListMyBudgets(auth.EffectiveTokens, limit, offset)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, nodes)
	}
}

// listEligibleBudgets returns the budgets the caller may request under.
//
//	@Summary		List budgets eligible for me
//	@Description	Retrieves the approved budgets the caller may submit requests under (their tokens appear in the budget's eligible requesters).
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{object}	tree.NodePage	"List of budgets, with the total number of matches."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@ID				listEligibleBudgets
//	@Router			/v1/nodes/eligible-for-me [get]
func listEligibleBudgets(cfg APIConfig) gin.HandlerFunc {
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
		nodes, err := svc.ListEligibleForMe(auth.EffectiveTokens, limit, offset)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, nodes)
	}
}

// listEligibleBudgetsForOwner returns budgets a specific owner may request under.
//
//	@Summary		List budgets eligible for an owner (root admin only)
//	@Description	Root admins only. Returns the approved budgets the specified owner tokens may request under. Used by the promote flow.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			owner_token	query		[]string	true	"Owner token(s) to resolve eligible budgets for"	collectionFormat(multi)
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{object}	tree.NodePage	"List of budgets, with the total number of matches."
//	@Failure		400	{object}	map[string]any	"Bad request — no owner tokens supplied."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden — caller is not a root admin."
//	@ID				listEligibleBudgetsForOwner
//	@Router			/v1/nodes/eligible-for-owner [get]
func listEligibleBudgetsForOwner(cfg APIConfig) gin.HandlerFunc {
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
		nodes, err := svc.ListEligibleForOwner(auth.EffectiveTokens, common.TokenList(ownerTokens), limit, offset)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, nodes)
	}
}

// searchNodes finds nodes anywhere below the budgets the caller administers.
//
//	@Summary		Search the managed tree
//	@Description	Full-text search over every node below the budgets the caller administers (and those budgets themselves). Matches name, purpose, ID, owner, creator, status, the linked OpenStack project and the tokens on the node. Returns a flat list — the tree is paginated, so a client cannot filter it locally.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			q		query		string	true	"Search query (non-empty)"
//	@Param			limit	query		int	false	"Maximum number of entries to return" default(100)
//	@Param			offset	query		int	false	"Offset into the result set" default(0)
//	@Success		200	{object}	tree.NodePage	"Matching nodes, with the total number of matches."
//	@Failure		400	{object}	map[string]any	"Bad request — empty query or invalid pagination."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@ID				searchNodes
//	@Router			/v1/nodes/search [get]
func searchNodes(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		limit, offset, err := parsePagination(c)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pagination parameters"})
			return
		}
		query := strings.TrimSpace(c.Query("q"))
		if query == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "q must not be empty"})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		page, err := svc.SearchNodes(auth.EffectiveTokens, query, limit, offset)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, page)
	}
}

// createNode creates a child node (budget or project leaf) under a parent budget.
//
//	@Summary		Create node
//	@Description	Creates a child node under a parent budget. Managers of the parent chain create directly approved children; eligible requesters submit a pending request, which the parent's auto-approve policy may approve immediately.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		tree.CreateNodeRequest	true	"Node creation data"
//	@Success		201		{object}	tree.Node	"Created node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Parent not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				createNode
//	@Router			/v1/nodes [post]
func createNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req tree.CreateNodeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.CreateNode(req, tree.UIActor(auth.UserEmail), auth.UserEmail, auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, node)
	}
}

// updateNode applies a direct edit to a budget node.
//
//	@Summary		Update node
//	@Description	Direct edit of a budget. Policy fields (name, admin scope, eligible requesters, auto-approve) require a manager of the node or its ancestors; limit and termination date require a manager of the parent chain. A project leaf accepts a name-only edit (rename) by its owner or a manager.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Node ID"
//	@Param			request	body		tree.UpdateNodeRequest	true	"Update data"
//	@Success		200		{object}	tree.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				updateNode
//	@Router			/v1/nodes/{id} [put]
func updateNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req tree.UpdateNodeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.UpdateNode(c.Param("id"), req, tree.UIActor(auth.UserEmail), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// requestNodeChange proposes changes requiring approval by the parent chain.
//
//	@Summary		Request node change
//	@Description	Proposes changes (limit, termination date, authorized users). A pending node is amended in place; an approved node transitions to change_pending until a manager of the parent chain decides.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Node ID"
//	@Param			request	body		tree.ChangeNodeRequest	true	"Proposed changes"
//	@Success		200		{object}	tree.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				requestNodeChange
//	@Router			/v1/nodes/{id}/request-change [post]
func requestNodeChange(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req tree.ChangeNodeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.RequestChange(c.Param("id"), req, tree.UIActor(auth.UserEmail), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// approveNode approves a pending node or the pending changes of a node.
//
//	@Summary		Approve node
//	@Description	Approves a pending or change_pending node (budget or leaf). Only managers of the PARENT chain may approve. modified_limit lets the approver grant a different limit than requested.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Node ID"
//	@Param			request	body		tree.ApproveNodeRequest	false	"Approval data"
//	@Success		200		{object}	tree.Node	"Approved node."
//	@Failure		400		{object}	map[string]any	"Bad request or insufficient capacity."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				approveNode
//	@Router			/v1/nodes/{id}/approve [post]
func approveNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req tree.ApproveNodeRequest
		_ = c.ShouldBindJSON(&req)
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.ApproveNode(c.Param("id"), req, tree.UIActor(auth.UserEmail), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// rejectNode rejects a pending node or discards pending changes.
//
//	@Summary		Reject node
//	@Description	Rejects a pending node (terminal) or discards the pending changes of a change_pending node (back to approved). Only managers of the parent chain may reject.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Node ID"
//	@Param			request	body		tree.RejectNodeRequest	false	"Rejection reason"
//	@Success		200		{object}	tree.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				rejectNode
//	@Router			/v1/nodes/{id}/reject [post]
func rejectNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req tree.RejectNodeRequest
		_ = c.ShouldBindJSON(&req)
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.RejectNode(c.Param("id"), req, tree.UIActor(auth.UserEmail), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// releaseNode releases an approved project leaf.
//
//	@Summary		Release node
//	@Description	Releases an approved project leaf, returning its capacity to the budget chain and driving OpenStack deprovisioning. Allowed for the owner or managers of the parent chain.
//	@Tags			nodes
//	@Security		Bearer
//	@Param			id	path	string	true	"Node ID"
//	@Success		200	{object}	tree.Node	"Released node."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		409	{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				releaseNode
//	@Router			/v1/nodes/{id}/release [post]
func releaseNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.ReleaseNode(c.Param("id"), tree.UIActor(auth.UserEmail), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// reparentNode moves a node under a new parent budget.
//
//	@Summary		Reparent node
//	@Description	Moves a node under a new parent budget. Requires management rights on the current parent chain AND on the new parent. Active nodes are capacity-checked against the new ancestor chain.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string						true	"Node ID"
//	@Param			request	body		tree.ReparentNodeRequest	true	"New parent"
//	@Success		200		{object}	tree.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request or insufficient capacity."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				reparentNode
//	@Router			/v1/nodes/{id}/reparent [post]
func reparentNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req tree.ReparentNodeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.ReparentNode(c.Param("id"), req, tree.UIActor(auth.UserEmail), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// transferNodeOwner hands a project leaf to a new responsible person.
//
//	@Summary		Transfer node owner
//	@Description	Transfers ownership of a project leaf to another user. Only managers of the parent chain may transfer.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string						true	"Node ID"
//	@Param			request	body		tree.TransferOwnerRequest	true	"New owner"
//	@Success		200		{object}	tree.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				transferNodeOwner
//	@Router			/v1/nodes/{id}/transfer-owner [post]
func transferNodeOwner(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req tree.TransferOwnerRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.TransferOwner(c.Param("id"), req, tree.UIActor(auth.UserEmail), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// promoteNode converts an imported leaf into a managed request.
//
//	@Summary		Promote imported node
//	@Description	Promotes an imported OpenStack project leaf: reparents it under the given budget, assigns an owner and flags it for the reconciler, which tags the OpenStack project and transitions the leaf to pending for the normal approval cycle. Requires management rights on the unassigned chain (root) and on the target budget.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Node ID"
//	@Param			request	body		tree.PromoteNodeRequest	true	"Promotion data"
//	@Success		200		{object}	tree.Node	"Updated node (status still imported, promote flag set)."
//	@Failure		400		{object}	map[string]any	"Bad request or insufficient capacity."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@ID				promoteNode
//	@Router			/v1/nodes/{id}/promote [post]
func promoteNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		var req tree.PromoteNodeRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		node, err := svc.PromoteNode(c.Param("id"), req, tree.UIActor(auth.UserEmail), auth.EffectiveTokens)
		if err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, node)
	}
}

// deleteNode deletes a budget subtree.
//
//	@Summary		Delete budget
//	@Description	Deletes a budget and its subtree. Refused while any node in the subtree is pending, awaiting a change decision, active, or an unpromoted import.
//	@Tags			nodes
//	@Security		Bearer
//	@Param			id	path	string	true	"Node ID"
//	@Success		204	"No content."
//	@Failure		400	{object}	map[string]any	"Subtree still has undecided or active nodes."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@ID				deleteNode
//	@Router			/v1/nodes/{id} [delete]
func deleteNode(cfg APIConfig) gin.HandlerFunc {
	svc := cfg.Service
	return func(c *gin.Context) {
		auth, err := mustGetAuthContext(c)
		if err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unable to resolve user context"})
			return
		}
		if err := svc.DeleteNode(c.Param("id"), tree.UIActor(auth.UserEmail), auth.EffectiveTokens); err != nil {
			c.JSON(errorToStatus(err), gin.H{"error": err.Error()})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
