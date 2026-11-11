package tree

import (
	"fmt"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// refusal is a message for people that is also one of the common sentinels (errors.Is), without the sentinel's own English text in it: the sentinel decides the status of the answer, msg is what the caller reads.
type refusal struct {
	msg  string
	kind error
}

func (r *refusal) Error() string { return r.msg }

func (r *refusal) Unwrap() error { return r.kind }

// refuse is a refusal of kind (common.ErrForbidden, ErrConflict, ErrNotFound) with a German text for people.
func refuse(kind error, format string, args ...any) error {
	return &refusal{msg: fmt.Sprintf(format, args...), kind: kind}
}

// invalid is a validation message for people (the fallback answers it with 400); %w wraps as in fmt.Errorf. A function of its own because the texts are German sentences, capitalised and with a full stop, which staticcheck refuses in a literal fmt.Errorf.
func invalid(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// errNoPermission is the refusal when the caller may not do what was asked.
var errNoPermission = refuse(common.ErrForbidden, "Keine Berechtigung.")

// errNodeNotFound is the refusal when the budget or project does not exist (or no longer exists).
var errNodeNotFound = refuse(common.ErrNotFound, "Budget oder Projekt nicht gefunden.")
