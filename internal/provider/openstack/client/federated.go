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
//     local_id — i.e. sha256(domain_id + "user" + unique_id). Verified against
//     the live shadow users of two real logins: the formula reproduces both ids
//     exactly.
//
// What that derivation does NOT give us is a way to CREATE such an account.
// Keystone mints the ID itself on POST /v3/users and ignores one supplied in
// the request (verified), so a pre-created account always carries a UUID, never
// the derived ID. An earlier version of this code treated that difference as
// proof that the cloud "derives ids differently" and deleted the account again
// — which made pre-creation impossible everywhere, since no cloud can satisfy
// that test.
//
// The binding that actually matters is the federated_user row Keystone writes
// from the `federated` block: the login looks the account up by
// (idp_id, protocol_id, unique_id), not by ID. So a pre-created account is kept
// and given its role. Should a login nevertheless spawn its own shadow account,
// the next sync notices — the derived ID now exists, it wins, and the leftover
// pre-created duplicate is removed. Either way the platform converges, without
// having to be right about Keystone's internals up front.
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
// The order below is the whole design: the account a login actually lands on
// wins whenever it exists, and everything else is a stand-in that holds the
// role until that day. See the notes at the top for why a stand-in can never
// carry the login's ID, and why that is not a reason to refuse pre-creation.
func (c *OpenStackClient) findOrCreateFederatedUser(email string, byName *users.User) (*users.User, error) {
	uniqueID := FederatedUniqueID(email)
	derivedID := FederatedUserID(c.federatedDomainID, uniqueID)

	derived, err := c.getUserByIDIfExists(derivedID)
	if err != nil {
		return nil, fmt.Errorf("look up derived federated id for %q: %w", email, err)
	}

	// Keystone's user LIST omits federated attributes; only a GET on the single
	// user carries them (verified against the cloud this runs on). byName comes
	// from a list, so judging its federation link on that payload declares every
	// account we pre-created link-less from its second sync onwards — and then
	// refuses the role for good, on every project but the one whose sync created
	// the account. Re-read it before deciding anything about it.
	if byName != nil {
		full, err := c.getUserByIDIfExists(byName.ID)
		if err != nil {
			return nil, fmt.Errorf("read user %q: %w", email, err)
		}
		if full != nil {
			byName = full
		}
	}

	switch {
	// The account a login binds to exists — it is the only one worth holding a
	// role. A stand-in of OURS for the same person has served its purpose and is
	// removed, so the person does not end up with two accounts and a role on the
	// wrong one. Accounts we did not create are never touched.
	case derived != nil:
		if byName != nil && byName.ID != derived.ID && byName.Description == ManagedUserDescription {
			if err := c.DeleteUser(byName.ID); err != nil {
				c.log.Warnw("Could not remove the pre-created account the login superseded",
					"email", email, "preseeded_id", byName.ID, "login_id", derived.ID, "error", err)
			} else {
				c.log.Infow("Removed the pre-created account the login superseded",
					"email", email, "preseeded_id", byName.ID, "login_id", derived.ID)
			}
		}
		return derived, nil

	// Our own stand-in from an earlier run, still carrying the link the login
	// resolves by. Reuse it rather than creating a second one.
	case byName != nil && byName.Description == ManagedUserDescription && hasFederatedLink(byName, c.federatedIdPID, c.federatedProtocolID, uniqueID):
		return byName, nil

	// An account carries this email but is not the one a login binds to, and it
	// is not a stand-in of ours either: a plain local account, or one whose OIDC
	// username differs from the email. Guessing would strand the role on it.
	case byName != nil:
		return nil, &PreseedConflict{
			Email: email,
			Reason: fmt.Sprintf(
				"a Keystone user with this name exists (id %s) but carries no federation link for unique_id %q on idp %q, so a login would not use it (it would land on id %s). "+
					"Most likely the OIDC username differs from the email address, or the account is a local (non-federated) one. "+
					"Assign the role to the correct account manually, or correct the username",
				byName.ID, uniqueID, c.federatedIdPID, derivedID),
		}

	// Nobody yet: pre-create the stand-in and let it hold the role. Keystone
	// gives it a UUID of its own choosing — see the notes at the top; the login
	// finds it by the federation link, not by ID.
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
		return created, nil
	}
}

// hasFederatedLink reports whether a user carries the given IdP link. Keystone
// returns the block under "federated" and gophercloud collects unmodelled
// fields in Extra, so it arrives as nested []any/map[string]any.
func hasFederatedLink(user *users.User, idpID, protocolID, uniqueID string) bool {
	blocks, ok := user.Extra["federated"].([]any)
	if !ok {
		return false
	}
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok || block["idp_id"] != idpID {
			continue
		}
		protocols, ok := block["protocols"].([]any)
		if !ok {
			continue
		}
		for _, rawProto := range protocols {
			proto, ok := rawProto.(map[string]any)
			if !ok {
				continue
			}
			if proto["protocol_id"] == protocolID && proto["unique_id"] == uniqueID {
				return true
			}
		}
	}
	return false
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
