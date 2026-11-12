package identity

import (
	"errors"
	"net/http"

	"github.com/pfisterer/cloud-self-service-golib/apierr"
	"github.com/pfisterer/cloud-self-service-golib/authn"
	"github.com/pfisterer/cloud-self-service-golib/msg"
)

// The answers of authentication and acting on behalf, in German and English like every message to people on the platform, each with a stable code a client matches on instead of the text.
var (
	ErrUnauthenticated      = apierr.New(http.StatusUnauthorized, "unauthenticated", msg.T("Anmeldung erforderlich.", "Sign-in required."))
	ErrAPITokenRejected     = apierr.New(http.StatusUnauthorized, "unauthenticated", msg.T("API-Token ungültig, abgelaufen oder gelöscht.", "API token invalid, expired or deleted."))
	ErrSignInRejected       = apierr.New(http.StatusUnauthorized, "unauthenticated", msg.T("Anmeldung ungültig oder abgelaufen.", "Sign-in invalid or expired."))
	ErrSignInUnavailable    = apierr.New(http.StatusServiceUnavailable, "auth_unavailable", msg.T("Anmeldedienst gerade nicht erreichbar.", "The sign-in service cannot be reached right now."))
	ErrInternal             = apierr.New(http.StatusInternalServerError, "internal", msg.T("Interner Fehler.", "Internal error."))
	ErrActAsNeedsSignIn     = apierr.New(http.StatusForbidden, "act_as_forbidden", msg.T("Handeln im Auftrag anderer Personen ist nur nach einer Anmeldung möglich, nicht mit einem API-Token.", "Acting on behalf of other people is only possible after signing in, not with an API token."))
	ErrActAsInvalid         = apierr.New(http.StatusBadRequest, "act_as_invalid", msg.T("X-Act-As muss eine E-Mail-Adresse sein.", "X-Act-As must be an e-mail address."))
	ErrActAsForbidden       = apierr.New(http.StatusForbidden, "act_as_forbidden", msg.T("Handeln im Auftrag anderer Personen ist nur für Administratoren möglich.", "Acting on behalf of other people is only possible for administrators."))
	ErrDirectoryUnavailable = apierr.New(http.StatusServiceUnavailable, "directory_unavailable", msg.T("Das Verzeichnis ist gerade nicht erreichbar, die Person, für die gehandelt wird, lässt sich nicht auflösen.", "The directory cannot be reached right now; the person acted for cannot be resolved."))
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
