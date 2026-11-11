package webserver

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/apierr"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/openstack-management-api/internal/common"
	"github.com/pfisterer/openstack-management-api/internal/identity"
	"github.com/pfisterer/openstack-management-api/internal/provider/openstack/catalog"
)

// errInternal is all a caller learns of a failure of the service: the detail (SQL, hosts, timeouts) goes to the log.
var errInternal = identity.ErrInternal

// serviceErrors is how this service answers the errors of its operations (golib's apierr: the mechanism is shared, this table is the service's).
//
// What the service marks as its own failure (common.ErrInternal: the store, the role provider) is a 500 with a generic message. Everything else falls back to 400 with the operation's message: the operations' validation messages carry no sentinel, and they are what the caller has to read to correct the request.
var serviceErrors = apierr.Classifier{
	Rules: []apierr.Rule{
		apierr.Is(common.ErrInternal, http.StatusInternalServerError, errInternal.Code, errInternal.Msg),
		apierr.IsWithText(common.ErrForbidden, http.StatusForbidden, "forbidden"),
		apierr.IsWithText(common.ErrNotFound, http.StatusNotFound, "not_found"),
		// 409, not 400: the request was fine, the client's view of the node was
		// stale. A client can tell the two apart and reload instead of asking
		// the user to correct an input that was never wrong.
		apierr.IsWithText(common.ErrConflict, http.StatusConflict, "conflict"),
	},
	Fallback: func(err error) *apierr.Error { return apierr.New(http.StatusBadRequest, "invalid", err.Error()) },
}

// catalogErrors is how the catalogue's administration answers: ErrUnavailable with 503, what the catalogue names (invalid, and the not-found and conflict it shares with the tree) with its message as serviceErrors answers it; anything else is the service's own failure.
var catalogErrors = apierr.Classifier{
	Rules: append([]apierr.Rule{
		apierr.Is(catalog.ErrUnavailable, http.StatusServiceUnavailable, "openstack_unavailable", "OpenStack ist noch nicht verbunden."),
		apierr.IsWithText(catalog.ErrInvalid, http.StatusBadRequest, "invalid"),
	}, serviceErrors.Rules...),
}

// respondError answers err by serviceErrors: a 4xx with the operation's message, a 5xx with a generic one and the detail in the log.
func (cfg APIConfig) respondError(c *gin.Context, err error) {
	cfg.respond(c, serviceErrors.Classify(err))
}

// respondInternal answers an error that can only be the service's own with a generic 500.
func (cfg APIConfig) respondInternal(c *gin.Context, err error) {
	cfg.respond(c, errInternal.WithCause(err))
}

// respond answers e, logging the cause of a 5xx.
func (cfg APIConfig) respond(c *gin.Context, e *apierr.Error) {
	if e.Status >= http.StatusInternalServerError {
		cfg.logInternal(c, e.Cause())
	}
	ginweb.AbortError(c, e)
}

// respondBadRequest answers 400 with a message of this service.
func respondBadRequest(c *gin.Context, msg string) {
	ginweb.AbortError(c, apierr.New(http.StatusBadRequest, "invalid", msg))
}

func (cfg APIConfig) logInternal(c *gin.Context, err error) {
	if cfg.Log != nil {
		cfg.Log.Errorw("request failed", "method", c.Request.Method, "path", c.FullPath(), "error", err)
	}
}
