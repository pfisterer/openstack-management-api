package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/openstack-management-api/internal/core/model"
	"github.com/pfisterer/openstack-management-api/internal/core/tree"
	"github.com/pfisterer/openstack-management-api/internal/identity"
)

// The shapes the REST handlers share: who the request is from, how its body and its page bounds are read, and how the service's answer becomes the response. A handler is the named function that carries the swagger annotations and says which service call it makes; everything around that call is here, once.

// caller is who the request is from, as identity.EffectiveAuthMiddleware resolved it. Without one the request is answered with 401 and ok is false.
func caller(c *gin.Context) (auth identity.AuthContext, ok bool) {
	auth, err := identity.FromContext(c)
	if err != nil {
		ginweb.AbortError(c, identity.ErrUnauthenticated)
		return identity.AuthContext{}, false
	}
	return auth, true
}

// uiActor is the caller of a change through the REST API: the person acted as, and the admin who acted for them when that was somebody else.
func uiActor(auth identity.AuthContext) model.Actor {
	return model.ActorFor(auth.UserEmail, auth.ActorEmail, "")
}

// pagination is the page a listing asks for (golib page: ?limit, default common.DefaultPageLimit, cut at common.MaxPageLimit, and ?offset). Bounds that do not parse answer 400 and ok is false.
func pagination(c *gin.Context) (limit, offset int, ok bool) {
	r, ok := ginweb.PageRequest(c)
	return r.Limit, r.Offset, ok
}

// bodyReader reads the request body into req, or answers the request and reports false.
type bodyReader[Req any] func(c *gin.Context, req *Req) bool

// noBody reads nothing: the handler takes no body.
func noBody[Req any](*gin.Context, *Req) bool { return true }

// jsonBody binds the body to req; one that does not bind is a 400 with the decoder's message, or with message where the handler has a better one.
func jsonBody[Req any](message string) bodyReader[Req] {
	return func(c *gin.Context, req *Req) bool {
		err := c.ShouldBindJSON(req)
		if err == nil {
			return true
		}
		if message == "" {
			respondBadRequest(c, "Der Inhalt der Anfrage lässt sich nicht lesen: "+err.Error())
		} else {
			respondBadRequest(c, message)
		}
		return false
	}
}

// optionalJSONBody is jsonBody where the body may be left out: none is the zero Req.
func optionalJSONBody[Req any](c *gin.Context, req *Req) bool {
	return c.Request.ContentLength == 0 || jsonBody[Req]("")(c, req)
}

// lenientJSONBody binds what binds; a body that does not is the zero Req. For an action whose body is a reason it can do without.
func lenientJSONBody[Req any](c *gin.Context, req *Req) bool {
	_ = c.ShouldBindJSON(req)
	return true
}

// handle is the shape of a handler: the body as read says (400 where it refuses it), the caller (401 without one), the service call — its error through respondError — and what it returns as JSON with status, or no body for 204.
func handle[Req, Out any](cfg APIConfig, status int, read bodyReader[Req], call func(c *gin.Context, req Req, auth identity.AuthContext) (Out, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req Req
		if !read(c, &req) {
			return
		}
		auth, ok := caller(c)
		if !ok {
			return
		}
		out, err := call(c, req, auth)
		if err != nil {
			cfg.respondError(c, err)
			return
		}
		if status == http.StatusNoContent {
			c.Status(status)
			return
		}
		c.JSON(status, out)
	}
}

// respond is handle for a handler without a body: the caller, the call, what it returns.
func respond[Out any](cfg APIConfig, status int, call func(c *gin.Context, auth identity.AuthContext) (Out, error)) gin.HandlerFunc {
	return handle(cfg, status, noBody[struct{}], func(c *gin.Context, _ struct{}, auth identity.AuthContext) (Out, error) { return call(c, auth) })
}

// writeNode is handle for a handler that writes a node from a JSON body and answers with the node.
func writeNode[Req any](cfg APIConfig, status int, call func(c *gin.Context, req Req, auth identity.AuthContext) (model.Node, error)) gin.HandlerFunc {
	return handle(cfg, status, jsonBody[Req](""), call)
}

// listPage is the shape of a listing handler: the page bounds (400 where they do not parse), the caller (401 without one), the service call — an error of the request's own, say a filter that does not validate, is a 400 with its message, as respondError answers it — and the page as JSON.
func listPage(cfg APIConfig, call func(c *gin.Context, auth identity.AuthContext, limit, offset int) (tree.NodePage, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit, offset, ok := pagination(c)
		if !ok {
			return
		}
		auth, ok := caller(c)
		if !ok {
			return
		}
		page, err := call(c, auth, limit, offset)
		if err != nil {
			cfg.respondError(c, err)
			return
		}
		c.JSON(http.StatusOK, page)
	}
}
