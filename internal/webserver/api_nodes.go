package webserver

import (
	"errors"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/identity"
)

// getNode returns a single node by ID if the caller is authorized to view it.
//
//	@Summary		Get node
//	@Description	Fetches a single node (budget or project leaf). Accessible by the leaf owner, authorized users, eligible requesters of a budget, and managers of the node's ancestor chain.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			id	path		string	true	"Node ID"
//	@Success		200	{object}	model.Node	"The node."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		500	{object}	map[string]any	"Internal server error."
//	@ID				getNode
//	@Router			/v1/nodes/{id} [get]
func getNode(cfg APIConfig) gin.HandlerFunc {
	return respond(cfg, http.StatusOK, func(c *gin.Context, auth identity.AuthContext) (*model.Node, error) {
		node, err := cfg.Service.GetNode(c.Param("id"), auth.EffectiveTokens)
		if err == nil && node == nil {
			return nil, common.ErrNotFound
		}
		return node, err
	})
}

// listNodeChildren returns the direct children of a budget node.
//
//	@Summary		List node children
//	@Description	Returns the direct children (budgets and project leaves) of a budget. Only managers of the budget or its ancestors may list children.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			id			path		string	true	"Parent node ID"
//	@Param			limit		query		int		false	"Maximum number of entries to return" default(100)
//	@Param			offset		query		int		false	"Offset into the result set" default(0)
//	@Param			kind		query		string	false	"Only budgets or only projects"	Enums(budget, project)
//	@Param			q			query		string	false	"Text matched against name, purpose, id, owner, OpenStack project and tokens"
//	@Param			status		query		string	false	"Comma-separated statuses to include"
//	@Param			group		query		string	false	"A group (or any) token to filter by, see group_mode"
//	@Param			group_mode	query		string	false	"access: the token is owner, admin or member of the project; owner: the owner holds the token"	Enums(access, owner)
//	@Param			sort		query		string	false	"Sort key: name, owner, status, termination_date, created_at, servers, reserved:<resource id>, used:<resource id> or used:<id>,<id>,… (sum in use)"
//	@Param			order		query		string	false	"Sort order"	Enums(asc, desc)
//	@Param			allocated	query		bool	false	"Instead of the direct children: the projects anywhere below that draw an allocation from this budget"
//	@Param			deep		query		bool	false	"Instead of the direct children: the projects of the whole subtree, sub-budgets included"
//	@Param			area		query		string	false	"Only the projects of one area (holding or asking for any of its resources), and those holding nothing yet"	Enums(cloud, workspaces, llm)
//	@Success		200	{object}	tree.NodePage	"List of child nodes, with the total number of matches."
//	@Failure		400	{object}	map[string]any	"Invalid filter."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@ID				listNodeChildren
//	@Router			/v1/nodes/{id}/children [get]
func listNodeChildren(cfg APIConfig) gin.HandlerFunc {
	return listPage(cfg, func(c *gin.Context, auth identity.AuthContext, limit, offset int) (tree.NodePage, error) {
		filter := tree.ChildFilter{
			Kind:      c.Query("kind"),
			Query:     c.Query("q"),
			Group:     c.Query("group"),
			GroupMode: c.Query("group_mode"),
			Sort:      c.Query("sort"),
			Desc:      c.Query("order") == "desc",
			Allocated: c.Query("allocated") == "true",
			Deep:      c.Query("deep") == "true",
			Area:      c.Query("area"),
		}
		for _, st := range strings.Split(c.Query("status"), ",") {
			if st = strings.TrimSpace(st); st != "" {
				filter.Statuses = append(filter.Statuses, st)
			}
		}
		if err := filter.Validate(); err != nil {
			return tree.NodePage{}, err
		}
		return cfg.Service.ListChildren(c.Param("id"), filter, auth.EffectiveTokens, limit, offset)
	})
}

// listMyNodes returns the project leaves the current user owns or administers.
//
//	@Summary		List my project leaves
//	@Description	Retrieves the project leaves the current (effective) user owns, administers through the project's admin scope, or is an authorized user (member) of — directly or through a group. Which of these applies follows from owner, admin_scope and authorized_users of each node.
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
	return listPage(cfg, func(c *gin.Context, auth identity.AuthContext, limit, offset int) (tree.NodePage, error) {
		return cfg.Service.ListMine(auth.UserEmail, auth.EffectiveTokens, limit, offset)
	})
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
	return listPage(cfg, func(c *gin.Context, auth identity.AuthContext, limit, offset int) (tree.NodePage, error) {
		scope := c.Query("scope")
		if scope != "" && scope != "direct" && scope != "subtree" {
			return tree.NodePage{}, errors.New(`scope must be "direct" or "subtree"`)
		}
		return cfg.Service.ListToManage(auth.EffectiveTokens, scope == "subtree", limit, offset)
	})
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
	return listPage(cfg, func(c *gin.Context, auth identity.AuthContext, limit, offset int) (tree.NodePage, error) {
		return cfg.Service.ListMyBudgets(auth.EffectiveTokens, limit, offset)
	})
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
	return listPage(cfg, func(c *gin.Context, auth identity.AuthContext, limit, offset int) (tree.NodePage, error) {
		return cfg.Service.ListEligibleForMe(auth.EffectiveTokens, limit, offset)
	})
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
	return listPage(cfg, func(c *gin.Context, auth identity.AuthContext, limit, offset int) (tree.NodePage, error) {
		ownerTokens := c.QueryArray("owner_token")
		if len(ownerTokens) == 0 {
			return tree.NodePage{}, errors.New("at least one owner_token query parameter is required")
		}
		return cfg.Service.ListEligibleForOwner(auth.EffectiveTokens, common.TokenList(ownerTokens), limit, offset)
	})
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
	return listPage(cfg, func(c *gin.Context, auth identity.AuthContext, limit, offset int) (tree.NodePage, error) {
		query := strings.TrimSpace(c.Query("q"))
		if query == "" {
			return tree.NodePage{}, errors.New("q must not be empty")
		}
		return cfg.Service.SearchNodes(auth.EffectiveTokens, query, limit, offset)
	})
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
//	@Success		201		{object}	model.Node	"Created node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Parent not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				createNode
//	@Router			/v1/nodes [post]
func createNode(cfg APIConfig) gin.HandlerFunc {
	return writeNode(cfg, http.StatusCreated, func(c *gin.Context, req tree.CreateNodeRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.CreateNode(req, uiActor(auth), auth.UserEmail, auth.EffectiveTokens)
	})
}

// updateNode applies a direct edit to a budget node.
//
//	@Summary		Update node
//	@Description	Direct edit of a budget. Policy fields (name, admin scope, eligible requesters, auto-approve) require a manager of the node or its ancestors; limit and termination date require a manager of the parent chain. A project leaf accepts only a rename and a change of its admin scope (who administers it with the owner), by its owner, its admins or a manager; neither needs approval.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Node ID"
//	@Param			request	body		tree.UpdateNodeRequest	true	"Update data"
//	@Success		200		{object}	model.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				updateNode
//	@Router			/v1/nodes/{id} [put]
func updateNode(cfg APIConfig) gin.HandlerFunc {
	return writeNode(cfg, http.StatusOK, func(c *gin.Context, req tree.UpdateNodeRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.UpdateNode(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
}

// requestNodeChange proposes changes; most need approval by the parent chain.
//
//	@Summary		Request node change
//	@Description	Proposes changes (limit, termination date, authorized users). A pending node is amended in place. On an approved project, a change that gives resources back, ends sooner, changes members only, or stays within the budget's auto-approve policy takes effect at once (status approved); anything else transitions to change_pending until a manager of the parent chain decides.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Node ID"
//	@Param			request	body		tree.ChangeNodeRequest	true	"Proposed changes"
//	@Success		200		{object}	model.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				requestNodeChange
//	@Router			/v1/nodes/{id}/request-change [post]
func requestNodeChange(cfg APIConfig) gin.HandlerFunc {
	return writeNode(cfg, http.StatusOK, func(c *gin.Context, req tree.ChangeNodeRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.RequestChange(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
}

// approveNode approves a pending node or the pending changes of a node.
//
//	@Summary		Approve node
//	@Description	Approves a pending or change_pending node (budget or leaf). Only managers of the PARENT chain may approve. modified_limit lets the approver grant a different limit than requested. expected_revision is the node's revision as the approver saw it; if the request changed since (amended, replaced, shortened), the approval is refused with 409 and the request has to be reloaded and decided again.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Node ID"
//	@Param			request	body		tree.ApproveNodeRequest	true	"Approval data"
//	@Success		200		{object}	model.Node	"Approved node."
//	@Failure		400		{object}	map[string]any	"Bad request (expected_revision missing) or insufficient capacity."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status, or the request changed since expected_revision; reload."
//	@Failure		500		{object}	map[string]any	"Internal server error."
//	@ID				approveNode
//	@Router			/v1/nodes/{id}/approve [post]
func approveNode(cfg APIConfig) gin.HandlerFunc {
	read := jsonBody[tree.ApproveNodeRequest]("expected_revision is required: the revision of the request as it was shown")
	return handle(cfg, http.StatusOK, read, func(c *gin.Context, req tree.ApproveNodeRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.ApproveNode(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
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
//	@Success		200		{object}	model.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				rejectNode
//	@Router			/v1/nodes/{id}/reject [post]
func rejectNode(cfg APIConfig) gin.HandlerFunc {
	return handle(cfg, http.StatusOK, lenientJSONBody[tree.RejectNodeRequest], func(c *gin.Context, req tree.RejectNodeRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.RejectNode(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
}

// releaseNode releases an approved project leaf.
//
//	@Summary		Release node
//	@Description	Releases an approved project leaf, returning its capacity to the budget chain and driving OpenStack deprovisioning. Allowed for the owner, the project's admins and managers of the parent chain. With delete it is also marked for deletion (see requestNodeDeletion); 409 where projects cannot be deleted.
//	@Tags			nodes
//	@Accept			json
//	@Security		Bearer
//	@Param			id		path	string					true	"Node ID"
//	@Param			request	body	tree.ReleaseNodeRequest	false	"Release options"
//	@Success		200	{object}	model.Node	"Released node."
//	@Failure		400	{object}	map[string]any	"Invalid request body."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		409	{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				releaseNode
//	@Router			/v1/nodes/{id}/release [post]
func releaseNode(cfg APIConfig) gin.HandlerFunc {
	// The body is optional: a plain release sends none.
	return handle(cfg, http.StatusOK, optionalJSONBody[tree.ReleaseNodeRequest], func(c *gin.Context, req tree.ReleaseNodeRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.ReleaseNode(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
}

// requestNodeDeletion marks a released or archived project for deletion.
//
//	@Summary		Delete a released project for good
//	@Description	Marks a released or archived project leaf for deletion: the reconciler empties its OpenStack project (servers, volumes, snapshots, images, networks), deletes it and then removes the leaf. Cannot be undone once started. Allowed for the owner, the project's admins and managers of the parent chain; 409 where projects cannot be deleted or the project is not released.
//	@Tags			nodes
//	@Security		Bearer
//	@Param			id	path	string	true	"Node ID"
//	@Success		200	{object}	model.Node	"Node marked for deletion."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		409	{object}	map[string]any	"Conflict: not released, or deletion is switched off."
//	@ID				requestNodeDeletion
//	@Router			/v1/nodes/{id}/deletion [post]
func requestNodeDeletion(cfg APIConfig) gin.HandlerFunc {
	return respond(cfg, http.StatusOK, func(c *gin.Context, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.RequestDeletion(c.Param("id"), uiActor(auth), auth.EffectiveTokens)
	})
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
//	@Success		200		{object}	model.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request or insufficient capacity."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				reparentNode
//	@Router			/v1/nodes/{id}/reparent [post]
func reparentNode(cfg APIConfig) gin.HandlerFunc {
	return writeNode(cfg, http.StatusOK, func(c *gin.Context, req tree.ReparentNodeRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.ReparentNode(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
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
//	@Success		200		{object}	model.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				transferNodeOwner
//	@Router			/v1/nodes/{id}/transfer-owner [post]
func transferNodeOwner(cfg APIConfig) gin.HandlerFunc {
	return writeNode(cfg, http.StatusOK, func(c *gin.Context, req tree.TransferOwnerRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.TransferOwner(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
}

// setNodeAttributes replaces a node's own attributes.
//
//	@Summary		Set a node's attributes
//	@Description	Replaces the node's own attributes: groups of key/value pairs such as {"billing": {"cost_center": "4711"}}, which the platform does not interpret. Nodes below inherit each group whole from the nearest node that sets it; a group set to {} stops that. A group or key name is lower-case letters, digits, '-' and '_'. On a budget its managers or those above may set them, on a project the managers of its budgets. They are returned only to those who look after the node: its managers and those above, and on a project its owner and admins.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string						true	"Node ID"
//	@Param			request	body		tree.SetAttributesRequest	true	"The node's own attributes"
//	@Success		200		{object}	model.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@ID				setNodeAttributes
//	@Router			/v1/nodes/{id}/attributes [put]
func setNodeAttributes(cfg APIConfig) gin.HandlerFunc {
	return writeNode(cfg, http.StatusOK, func(c *gin.Context, req tree.SetAttributesRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.SetAttributes(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
}

// removeNodeExternalGroup takes a group given access in OpenStack off a project.
//
//	@Summary		Remove an external group from a project
//	@Description	Removes a group that was assigned to the project in OpenStack rather than through the portal. The reconciler keeps such groups assigned while the project names them; after this the next pass removes the assignment in OpenStack. The project's owner and admins and the managers of its budgets may do it, without approval.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string	true	"Node ID"
//	@Param			groupId	path		string	true	"OpenStack group ID"
//	@Success		200		{object}	model.Node	"Updated node."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@ID				removeNodeExternalGroup
//	@Router			/v1/nodes/{id}/external-groups/{groupId} [delete]
func removeNodeExternalGroup(cfg APIConfig) gin.HandlerFunc {
	return respond(cfg, http.StatusOK, func(c *gin.Context, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.RemoveExternalGroup(c.Param("id"), c.Param("groupId"), uiActor(auth), auth.EffectiveTokens)
	})
}

// setNodeAllocation grants, changes or removes what a project draws from a
// budget above its own.
//
//	@Summary		Set a project's allocation from a budget above its own
//	@Description	Grants, changes or removes the resources a project draws from a budget above its own budget, on top of its own limit. The allocation is charged to that budget and the budgets above it, not to the ones in between. Granting or raising needs a manager of that budget or above and a reason; the project's owner and admins may lower or remove it. An empty limit removes the allocation. An availability comes from one place only: the project's own limit or one allocation.
//	@Tags			nodes
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string					true	"Project node ID"
//	@Param			request	body		tree.AllocationRequest	true	"The whole allocation from that budget"
//	@Success		200		{object}	model.Node	"Updated project."
//	@Failure		400		{object}	map[string]any	"Bad request."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@Failure		409		{object}	map[string]any	"Conflict: the node changed status; reload."
//	@ID				setNodeAllocation
//	@Router			/v1/nodes/{id}/allocations [put]
func setNodeAllocation(cfg APIConfig) gin.HandlerFunc {
	return writeNode(cfg, http.StatusOK, func(c *gin.Context, req tree.AllocationRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.SetAllocation(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
}

// listAllocationSources lists the budgets the caller may allocate to a project from.
//
//	@Summary		List budgets a project may draw an allocation from
//	@Description	The approved budgets above the project's own budget that the caller manages, nearest first, with their usage — the budgets PUT /v1/nodes/{id}/allocations accepts from this caller. Empty for anyone who manages none of them.
//	@Tags			nodes
//	@Produce		json
//	@Security		Bearer
//	@Param			id	path		string	true	"Project node ID"
//	@Success		200	{array}		model.Node	"Budgets, nearest first."
//	@Failure		400	{object}	map[string]any	"Not a project."
//	@Failure		401	{object}	map[string]any	"Unauthorized."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@ID				listAllocationSources
//	@Router			/v1/nodes/{id}/allocation-sources [get]
func listAllocationSources(cfg APIConfig) gin.HandlerFunc {
	return respond(cfg, http.StatusOK, func(c *gin.Context, auth identity.AuthContext) ([]model.Node, error) {
		return cfg.Service.AllocationSources(c.Param("id"), auth.EffectiveTokens)
	})
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
//	@Success		200		{object}	model.Node	"Updated node (status still imported, promote flag set)."
//	@Failure		400		{object}	map[string]any	"Bad request or insufficient capacity."
//	@Failure		401		{object}	map[string]any	"Unauthorized."
//	@Failure		403		{object}	map[string]any	"Forbidden."
//	@Failure		404		{object}	map[string]any	"Not found."
//	@ID				promoteNode
//	@Router			/v1/nodes/{id}/promote [post]
func promoteNode(cfg APIConfig) gin.HandlerFunc {
	return writeNode(cfg, http.StatusOK, func(c *gin.Context, req tree.PromoteNodeRequest, auth identity.AuthContext) (model.Node, error) {
		return cfg.Service.PromoteNode(c.Param("id"), req, uiActor(auth), auth.EffectiveTokens)
	})
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
	return respond(cfg, http.StatusNoContent, func(c *gin.Context, auth identity.AuthContext) (struct{}, error) {
		return struct{}{}, cfg.Service.DeleteNode(c.Param("id"), uiActor(auth), auth.EffectiveTokens)
	})
}
