package tree

import (
	"unicode/utf8"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// validateName refuses a name longer than common.MaxNameLength characters. Names are shown in every listing and become OpenStack project names; a megabyte of name fills each of them.
func validateName(name string) error {
	if n := utf8.RuneCountInString(name); n > common.MaxNameLength {
		return invalid("Der Name ist %d Zeichen lang, erlaubt sind höchstens %d.", n, common.MaxNameLength)
	}
	return nil
}

// validateReason refuses a reason longer than common.MaxReasonLength characters. Every reason is kept in the node's history.
func validateReason(reason string) error {
	if n := utf8.RuneCountInString(reason); n > common.MaxReasonLength {
		return invalid("Die Begründung ist %d Zeichen lang, erlaubt sind höchstens %d.", n, common.MaxReasonLength)
	}
	return nil
}

// validateOptionalReason is validateReason for a reason that may be absent.
func validateOptionalReason(reason *string) error {
	if reason == nil {
		return nil
	}
	return validateReason(*reason)
}
