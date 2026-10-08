package webserver

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/catalog"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"go.uber.org/zap"
)

// RegisterCatalogAdminRoutes mounts the root admins' management of the
// availabilities in the resource catalogue (package catalog).
func RegisterCatalogAdminRoutes(v1 *gin.RouterGroup, cfg APIConfig, rootAdminTokens common.TokenList, log *zap.SugaredLogger) {
	g := v1.Group("/admin/catalog")
	g.Use(requireRootAdmin(rootAdminTokens, log))
	{
		g.GET("", listCatalog(cfg))
		g.POST("", addCatalogEntry(cfg, log))
		g.GET("/:id", getCatalogEntry(cfg))
		g.PUT("/:id", updateCatalogEntry(cfg))
		g.POST("/:id/withdraw", withdrawCatalogEntry(cfg, log))
		g.POST("/:id/restore", restoreCatalogEntry(cfg, log))
		g.DELETE("/:id", removeCatalogEntry(cfg, log))
	}
}

// catalogError answers err with the status it stands for.
func catalogError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, catalog.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, catalog.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, catalog.ErrUnavailable):
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

// withCatalog answers 503 where there is nothing to manage, and the actor's
// email otherwise.
func withCatalog(cfg APIConfig, c *gin.Context) (*catalog.Admin, string, bool) {
	if cfg.CatalogAdmin == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the catalogue cannot be managed here"})
		return nil, "", false
	}
	auth, err := mustGetAuthContext(c)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return nil, "", false
	}
	return cfg.CatalogAdmin, auth.ActorEmail, true
}

// listCatalog lists the availabilities.
//
//	@Summary		List availabilities
//	@Description	The availabilities of the resource catalogue (networks, images, flavours) with their state and how many nodes hold each. Requires a root admin token.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{array}		catalog.View	"The availabilities."
//	@Failure		403	{object}	map[string]any	"Forbidden."
//	@ID				listCatalog
//	@Router			/v1/admin/catalog [get]
func listCatalog(cfg APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		admin, _, ok := withCatalog(cfg, c)
		if !ok {
			return
		}
		out, err := admin.List(c.Request.Context())
		if err != nil {
			catalogError(c, err)
			return
		}
		c.JSON(http.StatusOK, out)
	}
}

// addCatalogEntry offers a new availability.
//
//	@Summary		Add an availability
//	@Description	Offers a network, image or flavour as an availability. The target is checked in OpenStack first: it must exist, a flavour must be private, an image shared and owned by the scope parent. The root holds it right away. A removed id can be added again; it starts clean. Requires a root admin token.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		catalog.NewEntry	true	"The availability."
//	@Success		201		{object}	catalog.Entry		"Added."
//	@Failure		400		{object}	map[string]any		"Invalid, or the target cannot be granted."
//	@Failure		409		{object}	map[string]any		"The id is taken."
//	@Failure		503		{object}	map[string]any		"OpenStack is not connected yet."
//	@ID				addCatalogEntry
//	@Router			/v1/admin/catalog [post]
func addCatalogEntry(cfg APIConfig, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		admin, actor, ok := withCatalog(cfg, c)
		if !ok {
			return
		}
		var in catalog.NewEntry
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		e, err := admin.Add(c.Request.Context(), in, actor)
		if err != nil {
			catalogError(c, err)
			return
		}
		log.Infow("Availability added", "resource", e.ID, "grant_type", e.Grant.Type, "target", e.Grant.Target, "actor", actor)
		c.JSON(http.StatusCreated, e)
	}
}

// getCatalogEntry tells who holds an availability.
//
//	@Summary		Get an availability with its holders
//	@Description	The availability, the nodes holding it (own limit, special allocation, pending change, per-requester cap) and the projects OpenStack grants it to, split into projects of this portal and others. Requires a root admin token.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Param			id	path		string			true	"Resource id"
//	@Success		200	{object}	catalog.Status	"The availability."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@ID				getCatalogEntry
//	@Router			/v1/admin/catalog/{id} [get]
func getCatalogEntry(cfg APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		admin, _, ok := withCatalog(cfg, c)
		if !ok {
			return
		}
		st, err := admin.Status(c.Request.Context(), c.Param("id"))
		if err != nil {
			catalogError(c, err)
			return
		}
		c.JSON(http.StatusOK, st)
	}
}

// updateCatalogEntry changes name, group and message.
//
//	@Summary		Update an availability
//	@Description	Changes how an availability is presented: name, group, message. The id and the grant cannot change — moving an id to another target would move every existing grant with it. Requires a root admin token.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			id		path		string				true	"Resource id"
//	@Param			request	body		catalog.NewEntry	true	"Name, group and message; id and grant are ignored."
//	@Success		200		{object}	catalog.Entry		"Updated."
//	@Failure		404		{object}	map[string]any		"Not found."
//	@ID				updateCatalogEntry
//	@Router			/v1/admin/catalog/{id} [put]
func updateCatalogEntry(cfg APIConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		admin, actor, ok := withCatalog(cfg, c)
		if !ok {
			return
		}
		var in catalog.NewEntry
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		e, err := admin.Update(c.Request.Context(), c.Param("id"), in, actor)
		if err != nil {
			catalogError(c, err)
			return
		}
		c.JSON(http.StatusOK, e)
	}
}

// WithdrawResponse is a withdrawn availability and how many nodes changed.
type WithdrawResponse struct {
	Entry   catalog.Entry `json:"entry"`
	Changed int           `json:"changed"`
}

// withdrawCatalogEntry takes an availability away everywhere.
//
//	@Summary		Withdraw an availability
//	@Description	Sets the availability to 0 on every node, the root included, and stops offering it; the reconciler revokes it in OpenStack on its next pass. Running in OpenStack is not stopped: a VM keeps its flavour, a network with ports in the project stays shared until they are gone. Can be repeated, and undone with restore. Requires a root admin token.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Param			id	path		string				true	"Resource id"
//	@Success		200	{object}	WithdrawResponse	"Withdrawn."
//	@Failure		404	{object}	map[string]any		"Not found."
//	@Failure		409	{object}	map[string]any		"Removed already."
//	@ID				withdrawCatalogEntry
//	@Router			/v1/admin/catalog/{id}/withdraw [post]
func withdrawCatalogEntry(cfg APIConfig, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		admin, actor, ok := withCatalog(cfg, c)
		if !ok {
			return
		}
		e, n, err := admin.Withdraw(c.Request.Context(), c.Param("id"), actor)
		if err != nil {
			catalogError(c, err)
			return
		}
		log.Infow("Availability withdrawn", "resource", e.ID, "nodes_changed", n, "actor", actor)
		c.JSON(http.StatusOK, WithdrawResponse{Entry: e, Changed: n})
	}
}

// restoreCatalogEntry offers a withdrawn availability again.
//
//	@Summary		Restore an availability
//	@Description	Offers a withdrawn availability again. Nothing is granted back: the root holds it, everyone else starts from 0. Requires a root admin token.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Param			id	path		string			true	"Resource id"
//	@Success		200	{object}	catalog.Entry	"Restored."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		409	{object}	map[string]any	"Not withdrawn."
//	@ID				restoreCatalogEntry
//	@Router			/v1/admin/catalog/{id}/restore [post]
func restoreCatalogEntry(cfg APIConfig, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		admin, actor, ok := withCatalog(cfg, c)
		if !ok {
			return
		}
		e, err := admin.Restore(c.Request.Context(), c.Param("id"), actor)
		if err != nil {
			catalogError(c, err)
			return
		}
		log.Infow("Availability restored", "resource", e.ID, "actor", actor)
		c.JSON(http.StatusOK, e)
	}
}

// removeCatalogEntry takes a withdrawn availability out of the catalogue.
//
//	@Summary		Remove an availability
//	@Description	Removes a withdrawn availability from the catalogue and from every node. Refused while a node still holds it or OpenStack still grants it to a project of this portal — afterwards nothing would ever revoke it. Requires a root admin token.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Param			id	path	string	true	"Resource id"
//	@Success		204	"Removed."
//	@Failure		404	{object}	map[string]any	"Not found."
//	@Failure		409	{object}	map[string]any	"Not withdrawn, or still held or granted."
//	@ID				removeCatalogEntry
//	@Router			/v1/admin/catalog/{id} [delete]
func removeCatalogEntry(cfg APIConfig, log *zap.SugaredLogger) gin.HandlerFunc {
	return func(c *gin.Context) {
		admin, actor, ok := withCatalog(cfg, c)
		if !ok {
			return
		}
		id := c.Param("id")
		if err := admin.Remove(c.Request.Context(), id, actor); err != nil {
			catalogError(c, err)
			return
		}
		log.Infow("Availability removed", "resource", id, "actor", actor)
		c.Status(http.StatusNoContent)
	}
}
