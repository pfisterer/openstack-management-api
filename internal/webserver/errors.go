package webserver

import (
	"errors"
	"net/http"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// errorToStatus maps sentinel errors to HTTP status codes.
// Falls back to 400 Bad Request for unrecognised errors.
func errorToStatus(err error) int {
	switch {
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
