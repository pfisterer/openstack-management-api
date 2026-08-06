package osclient

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gophercloud/gophercloud"
	"github.com/gophercloud/gophercloud/openstack/identity/v3/users"
)

// Pre-creating ("preseeding") federated users
//
// Keystone creates a federated account only on the user's first SSO login, so
// without pre-creation a role can not be assigned before that login ever
// happened. Pre-creation works because the account Keystone would create is
// fully predictable:
//
//   - it is found again by (idp_id, protocol_id, unique_id);
//   - unique_id is the URL-quoted value the IdP asserts as the user identifier —
//     keystone/auth/plugins/mapped.py does `user['id'] = parse.quote(user_id)`,
//     and nothing downstream changes it (shadow_federated_user stores it as is);
//   - the public user ID is derived from it: the default (sha256) ID generator
//     hashes the mapping values ordered by key — domain_id, entity_type,
//     local_id — i.e. sha256(domain_id + "user" + unique_id).
//
// Two details are ours, not Keystone's, and both are worth knowing:
//
//   - Case: Keystone does NOT normalize case anywhere. The lowercasing below
//     matches what Keycloak asserts (it stores usernames lowercased), so it is a
//     property of the IdP, not of Keystone. If an IdP ever asserted a mixed-case
//     username, the pre-created account would not match and the login would
//     create a second one.
//   - Identifier: the asserted value is the preferred_username claim, which is
//     not necessarily the email address. Everywhere in this deployment the two
//     are identical, so the email is used — but that assumption is checked
//     rather than trusted, see FindOrCreateUser.
const federatedEntityType = "user"

// keystoneQuote mirrors Python's urllib.parse.quote(s) with its default
// safe="/" — the exact function Keystone applies to the asserted user id.
// Go's url.QueryEscape is NOT equivalent: it encodes a space as "+" instead of
// "%20" and escapes "/". For the usernames seen here the two agree, but the
// pre-created account is worthless if the encoding differs by one byte, so this
// follows the original.
func keystoneQuote(s string) string {
	const upperhex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch >= '0' && ch <= '9':
			b.WriteByte(ch)
		case ch == '_' || ch == '.' || ch == '-' || ch == '~' || ch == '/':
			b.WriteByte(ch)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[ch>>4])
			b.WriteByte(upperhex[ch&0x0F])
		}
	}
	return b.String()
}

// FederatedUniqueID returns the unique_id Keystone will record for the given
// asserted username (see the notes above for case handling).
func FederatedUniqueID(username string) string {
	return keystoneQuote(strings.ToLower(strings.TrimSpace(username)))
}

// FederatedUserID returns the public user ID Keystone derives for a federated
// identity, so an account can be looked up exactly instead of by name.
func FederatedUserID(domainID, uniqueID string) string {
	sum := sha256.Sum256([]byte(domainID + federatedEntityType + uniqueID))
	return hex.EncodeToString(sum[:])
}

// PreseedConflict reports that a Keystone account could not be pre-created or
// reused without guessing. It is deliberately not a plain error string: the
// reconciler collects these and surfaces them in its status, because guessing
// here produces an account nobody ever logs into while the role assignment
// silently points nowhere.
type PreseedConflict struct {
	// Email is the address the platform knows the person by.
	Email string
	// Reason explains what was found and what to do about it.
	Reason string
}

func (e *PreseedConflict) Error() string {
	return fmt.Sprintf("cannot pre-create %s: %s", e.Email, e.Reason)
}

// findOrCreateFederatedUser resolves the account an SSO login for email will end
// up on, creating it when it does not exist yet. byName is the account already
// found under that email (may be nil).
//
// Two lookups are combined on purpose: by email, because that is the identifier
// the platform works with, and by the derived ID, because that is the account
// the login actually binds to. Agreement between the two is what makes
// pre-creation safe; disagreement is reported instead of resolved by guessing,
// since either choice would silently strand the role assignment.
func (c *OpenStackClient) findOrCreateFederatedUser(email string, byName *users.User) (*users.User, error) {
	uniqueID := FederatedUniqueID(email)
	expectedID := FederatedUserID(c.federatedDomainID, uniqueID)

	derived, err := c.getUserByIDIfExists(expectedID)
	if err != nil {
		return nil, fmt.Errorf("look up derived federated id for %q: %w", email, err)
	}

	switch {
	// The account found by email IS the one the login will use.
	case byName != nil && byName.ID == expectedID:
		return byName, nil

	// Nothing under this email, but the login target already exists — an earlier
	// real login created it under a different name. Reusing it is exactly right;
	// creating another account here would be the duplicate we want to avoid.
	case byName == nil && derived != nil:
		c.log.Infow("Reusing existing federated account for pre-seeding",
			"email", email, "user_id", derived.ID, "name", derived.Name)
		return derived, nil

	// An account carries this email, but it is not the one the login binds to.
	// Typically the OIDC username differs from the email (the asserted
	// preferred_username is what counts), or the account is a plain local user.
	case byName != nil:
		return nil, &PreseedConflict{
			Email: email,
			Reason: fmt.Sprintf(
				"a Keystone user with this name exists (id %s), but a login for this address resolves to id %s (unique_id %q, idp %q). "+
					"Most likely the OIDC username differs from the email address, or the account is a local (non-federated) one. "+
					"Assign the role to the correct account manually, or correct the username — pre-creating now would produce an account nobody logs into",
				byName.ID, expectedID, uniqueID, c.federatedIdPID),
		}

	// Neither exists: pre-create the account the login will find.
	default:
		link := FederatedLink{
			IdPID:      c.federatedIdPID,
			ProtocolID: c.federatedProtocolID,
			UniqueID:   uniqueID,
		}
		created, err := c.CreateFederatedUser(email, email, link)
		if err != nil {
			return nil, fmt.Errorf("create federated user %q: %w", email, err)
		}
		// The ID Keystone assigned must be the one a login will resolve to,
		// otherwise the account is invisible to that login.
		if created.ID != expectedID {
			// Take the account back out. Leaving it behind is what turned a
			// stable failure into a self-sustaining loop: the rejected account
			// carries our managed description and holds no role, so the orphan
			// sweep at the end of the very same run deleted it — and the next
			// run five minutes later created it again, with a fresh UUID. That
			// cycle ran against Keystone until somebody noticed.
			//
			// Cleaning up here rather than relying on the sweep also keeps the
			// two decisions independent: whoever changes the sweep later cannot
			// silently resurrect the loop.
			cleanup := ""
			if err := c.DeleteUser(created.ID); err != nil {
				cleanup = fmt.Sprintf(" (the account could not be removed either: %v)", err)
			}
			return nil, &PreseedConflict{
				Email: email,
				Reason: fmt.Sprintf(
					"pre-created account got id %s, but a login for unique_id %q resolves to id %s — this cloud derives federated ids differently than assumed. The unusable account was removed again%s; grant the role by hand on the account the login actually uses, or let the user log in once so it exists",
					created.ID, uniqueID, expectedID, cleanup),
			}
		}
		return created, nil
	}
}

// getUserByIDIfExists returns nil (and no error) when the user does not exist,
// so a missing account is not confused with a failed lookup.
func (c *OpenStackClient) getUserByIDIfExists(userID string) (*users.User, error) {
	user, err := users.Get(c.Identity, userID).Extract()
	if err != nil {
		var notFound gophercloud.ErrDefault404
		if errors.As(err, &notFound) {
			return nil, nil
		}
		return nil, err
	}
	return user, nil
}
