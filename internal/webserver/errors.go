package webserver

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// errorToStatus maps sentinel errors to HTTP status codes.
//
// What the service marks as its own failure (common.ErrInternal: the store, the role provider) is a 500. Everything else falls back to 400: the operations' validation messages carry no sentinel, and they are what the caller has to read to correct the request.
func errorToStatus(err error) int {
	switch {
	case errors.Is(err, common.ErrInternal):
		return http.StatusInternalServerError
	case errors.Is(err, common.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, common.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, common.ErrConflict):
		// 409, not 400: the request was fine, the client's view of the node was
		// stale. A client can tell the two apart and reload instead of asking
		// the user to correct an input that was never wrong.
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// internalErrorMessage is all a caller learns of a failure of the service: the detail (SQL, hosts, timeouts) goes to the log.
const internalErrorMessage = "internal error"

// respondError answers err: a 4xx with the operation's message, a 5xx with a generic one and the detail in the log.
func (cfg APIConfig) respondError(c *gin.Context, err error) {
	status := errorToStatus(err)
	if status >= http.StatusInternalServerError {
		cfg.logInternal(c, err)
		c.JSON(status, gin.H{"error": internalErrorMessage})
		return
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

// respondInternal answers an error that can only be the service's own with a generic 500.
func (cfg APIConfig) respondInternal(c *gin.Context, err error) {
	cfg.logInternal(c, err)
	c.JSON(http.StatusInternalServerError, gin.H{"error": internalErrorMessage})
}

func (cfg APIConfig) logInternal(c *gin.Context, err error) {
	if cfg.Log != nil {
		cfg.Log.Errorw("request failed", "method", c.Request.Method, "path", c.FullPath(), "error", err)
	}
}
