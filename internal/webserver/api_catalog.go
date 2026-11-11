package webserver

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/provider/openstack/catalog"
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

// catalogError answers err with the status it stands for: ErrUnavailable with 503, what the catalogue names otherwise (invalid, and the not-found and conflict it shares with the tree) with its message as respondError answers it; anything else is the service's own failure, answered with a generic message and logged.
func (cfg APIConfig) catalogError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, catalog.ErrUnavailable):
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": err.Error()})
	case errors.Is(err, catalog.ErrInvalid), errors.Is(err, catalog.ErrNotFound), errors.Is(err, catalog.ErrConflict):
		cfg.respondError(c, err)
	default:
		cfg.respondInternal(c, err)
	}
}

// catalogOp is the shape of a catalogue handler: 503 where there is nothing to manage, the caller (401 without one), the body as read says (400 where it refuses it), the step with the catalogue and the actor's email, its error through catalogError, and what it returns as JSON with status — no body for 204. done, where given, is told what was done: the log line.
func catalogOp[In, Out any](cfg APIConfig, status int, read bodyReader[In], step func(c *gin.Context, admin *catalog.Admin, actor string, in In) (Out, error), done func(out Out, actor string)) gin.HandlerFunc {
	return func(c *gin.Context) {
		if cfg.CatalogAdmin == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the catalogue cannot be managed here"})
			return
		}
		auth, ok := caller(c)
		if !ok {
			return
		}
		var in In
		if !read(c, &in) {
			return
		}
		out, err := step(c, cfg.CatalogAdmin, auth.ActorEmail, in)
		if err != nil {
			cfg.catalogError(c, err)
			return
		}
		if done != nil {
			done(out, auth.ActorEmail)
		}
		if status == http.StatusNoContent {
			c.Status(status)
			return
		}
		c.JSON(status, out)
	}
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
	return catalogOp(cfg, http.StatusOK, noBody[struct{}], func(c *gin.Context, admin *catalog.Admin, _ string, _ struct{}) ([]catalog.View, error) {
		return admin.List(c.Request.Context())
	}, nil)
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
	return catalogOp(cfg, http.StatusCreated, jsonBody[catalog.NewEntry](""), func(c *gin.Context, admin *catalog.Admin, actor string, in catalog.NewEntry) (catalog.Entry, error) {
		return admin.Add(c.Request.Context(), in, actor)
	}, func(e catalog.Entry, actor string) {
		log.Infow("Availability added", "resource", e.ID, "grant_type", e.Grant.Type, "target", e.Grant.Target, "actor", actor)
	})
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
	return catalogOp(cfg, http.StatusOK, noBody[struct{}], func(c *gin.Context, admin *catalog.Admin, _ string, _ struct{}) (catalog.Status, error) {
		return admin.Status(c.Request.Context(), c.Param("id"))
	}, nil)
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
	return catalogOp(cfg, http.StatusOK, jsonBody[catalog.NewEntry](""), func(c *gin.Context, admin *catalog.Admin, actor string, in catalog.NewEntry) (catalog.Entry, error) {
		return admin.Update(c.Request.Context(), c.Param("id"), in, actor)
	}, nil)
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
	return catalogOp(cfg, http.StatusOK, noBody[struct{}], func(c *gin.Context, admin *catalog.Admin, actor string, _ struct{}) (WithdrawResponse, error) {
		e, n, err := admin.Withdraw(c.Request.Context(), c.Param("id"), actor)
		return WithdrawResponse{Entry: e, Changed: n}, err
	}, func(out WithdrawResponse, actor string) {
		log.Infow("Availability withdrawn", "resource", out.Entry.ID, "nodes_changed", out.Changed, "actor", actor)
	})
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
	return catalogOp(cfg, http.StatusOK, noBody[struct{}], func(c *gin.Context, admin *catalog.Admin, actor string, _ struct{}) (catalog.Entry, error) {
		return admin.Restore(c.Request.Context(), c.Param("id"), actor)
	}, func(e catalog.Entry, actor string) {
		log.Infow("Availability restored", "resource", e.ID, "actor", actor)
	})
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
	return catalogOp(cfg, http.StatusNoContent, noBody[struct{}], func(c *gin.Context, admin *catalog.Admin, actor string, _ struct{}) (string, error) {
		id := c.Param("id")
		return id, admin.Remove(c.Request.Context(), id, actor)
	}, func(id string, actor string) {
		log.Infow("Availability removed", "resource", id, "actor", actor)
	})
}
