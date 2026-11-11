package identity

import (
	"errors"
	"net/http"

	"github.com/pfisterer/cloud-self-service-golib/apierr"
	"github.com/pfisterer/cloud-self-service-golib/authn"
)

// The answers of authentication and acting on behalf, in German like every message to people on the platform, each with a stable code a client matches on instead of the text.
var (
	ErrUnauthenticated      = apierr.New(http.StatusUnauthorized, "unauthenticated", "Anmeldung erforderlich.")
	ErrAPITokenRejected     = apierr.New(http.StatusUnauthorized, "unauthenticated", "API-Token ungültig, abgelaufen oder gelöscht.")
	ErrSignInRejected       = apierr.New(http.StatusUnauthorized, "unauthenticated", "Anmeldung ungültig oder abgelaufen.")
	ErrSignInUnavailable    = apierr.New(http.StatusServiceUnavailable, "auth_unavailable", "Anmeldedienst gerade nicht erreichbar.")
	ErrInternal             = apierr.New(http.StatusInternalServerError, "internal", "Interner Fehler.")
	ErrActAsNeedsSignIn     = apierr.New(http.StatusForbidden, "act_as_forbidden", "Handeln im Auftrag anderer Personen ist nur nach einer Anmeldung möglich, nicht mit einem API-Token.")
	ErrActAsInvalid         = apierr.New(http.StatusBadRequest, "act_as_invalid", "X-Act-As muss eine E-Mail-Adresse sein.")
	ErrActAsForbidden       = apierr.New(http.StatusForbidden, "act_as_forbidden", "Handeln im Auftrag anderer Personen ist nur für Administratoren möglich.")
	ErrDirectoryUnavailable = apierr.New(http.StatusServiceUnavailable, "directory_unavailable", "Das Verzeichnis ist gerade nicht erreichbar, die Person, für die gehandelt wird, lässt sich nicht auflösen.")
)

// ActAsError is the answer to an error of authn.ActAs: 400 for a malformed header, 403 otherwise.
func ActAsError(err error) *apierr.Error {
	switch {
	case errors.Is(err, authn.ErrActAsInvalid):
		return ErrActAsInvalid
	case errors.Is(err, authn.ErrActAsNeedsSignIn):
		return ErrActAsNeedsSignIn
	default:
		return ErrActAsForbidden
	}
}
