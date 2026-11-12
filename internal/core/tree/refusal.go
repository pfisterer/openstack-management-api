package tree

import (
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/openstack-management-api/internal/common"
)

// refuse is a refusal of kind (common.ErrForbidden, ErrConflict, ErrNotFound) with a text for people in German and English (msg.F: de and en formatted with the same args): kind decides the status of the answer (errors.Is), the text is what the caller reads, without kind's own English text in it.
func refuse(kind error, de, en string, args ...any) error {
	return msg.Wrap(kind, msg.F(de, en, args...))
}

// invalid is a validation message for people in German and English (the fallback answers it with 400); %w wraps as in fmt.Errorf (msg.Errorf). A function of its own because the texts are sentences, capitalised and with a full stop, which staticcheck refuses in a literal fmt.Errorf.
func invalid(de, en string, args ...any) error {
	return msg.Errorf(de, en, args...)
}

// errNoPermission is the refusal when the caller may not do what was asked.
var errNoPermission = refuse(common.ErrForbidden, "Keine Berechtigung.", "Not allowed.")

// errNodeNotFound is the refusal when the budget or project does not exist (or no longer exists).
var errNodeNotFound = refuse(common.ErrNotFound, "Budget oder Projekt nicht gefunden.", "Budget or project not found.")
