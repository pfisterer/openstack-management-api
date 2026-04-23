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
	default:
		return http.StatusBadRequest
	}
}
