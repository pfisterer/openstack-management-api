package tree

import (
	"fmt"
	"unicode/utf8"

	"github.com/pfisterer/openstack-management-api/internal/common"
)

// validateName refuses a name longer than common.MaxNameLength characters. Names are shown in every listing and become OpenStack project names; a megabyte of name fills each of them.
func validateName(name string) error {
	if n := utf8.RuneCountInString(name); n > common.MaxNameLength {
		return fmt.Errorf("the name is %d characters long, at most %d are allowed", n, common.MaxNameLength)
	}
	return nil
}

// validateReason refuses a reason longer than common.MaxReasonLength characters. Every reason is kept in the node's history.
func validateReason(reason string) error {
	if n := utf8.RuneCountInString(reason); n > common.MaxReasonLength {
		return fmt.Errorf("the reason is %d characters long, at most %d are allowed", n, common.MaxReasonLength)
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
