package reconciler

import (
	"cmp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
)

// keystoneProjectNameMaxLen is Keystone's hard limit for project names: the API
// schema caps "name" at 64 characters and rejects anything longer with 400.
const keystoneProjectNameMaxLen = 64

// managedDescriptionSuffix is appended to every managed project's description so a
// project is recognisable as ours in Horizon, where tags are not shown. It carries
// no meaning for the reconciler — identification runs on tags alone.
const managedDescriptionSuffix = " (managed project)"

// buildProjectName constructs the OS project name for a leaf: what the leaf is
// called, whose it is, and a short form of its ID, e.g.
// "Cloud Computing @ max.muster [p_7ad31c]".
//
// "What it is called" falls back to the purpose for leaves created before the
// name became mandatory — they carry no name at all. Without the fallback such a
// project ends up named after its bare node ID ("p_7ad31c42-21e7-…"), which is
// what a user sees in Horizon and Skyline and cannot tell apart from any other.
//
// "Whose it is" is the part of the owner's address before the @. Sharing makes
// equal names common — somebody who administers or uses other people's projects
// sees every "test" there is — and the owner is what tells them apart for a
// person. The whole address would not fit: a student address alone takes half
// of Keystone's 64 characters.
//
// Name and owner get the same share of what the ID leaves: whichever needs less
// hands the rest to the other, and only what exceeds its share is cut.
//
// The ID suffix is not decoration. Keystone enforces project-name uniqueness per
// *domain*, not per parent — two leaves named "Cloud Computing" under different
// budgets, or a name already taken by a foreign project elsewhere in the domain,
// would otherwise collide with a 409. The suffix makes the name collision-free by
// construction, so no retry-with-fallback logic is needed.
//
// Nothing parses the name back: a project is matched to its node via the
// resource-id tag, so renaming a node is safe at any time.
func buildProjectName(leaf model.Node) string {
	id := sanitizeProjectName(shortNodeID(leaf.ID))
	name := sanitizeProjectName(cmp.Or(leaf.Name, leaf.Reason))
	owner, _, _ := strings.Cut(leaf.OwnerEmail(), "@")
	owner = sanitizeProjectName(owner)

	switch {
	case id == "" && name == "":
		return "unnamed project" // defensive: a node always has an ID
	case id == "":
		return truncateRunes(name, keystoneProjectNameMaxLen)
	case name == "":
		return truncateRunes(id, keystoneProjectNameMaxLen)
	}

	suffix := " [" + id + "]"
	room := keystoneProjectNameMaxLen - utf8.RuneCountInString(suffix)
	if owner != "" {
		room -= utf8.RuneCountInString(ownerSeparator)
	}
	if room < 2 {
		// Pathologically long ID: drop the name, keep the identifying part.
		return truncateRunes(id, keystoneProjectNameMaxLen)
	}
	if owner == "" {
		return truncateRunes(name, room) + suffix
	}
	nameRoom, ownerRoom := shareRoom(utf8.RuneCountInString(name), utf8.RuneCountInString(owner), room)
	return truncateRunes(name, nameRoom) + ownerSeparator + truncateRunes(owner, ownerRoom) + suffix
}

// ownerSeparator stands between a project's name and its owner in the OS name.
const ownerSeparator = " @ "

// shareRoom splits room between two parts of length a and b: each is guaranteed
// half, and a part that needs less than its half leaves the rest to the other.
// Two parts that both need more get exactly the same length.
func shareRoom(a, b, room int) (int, int) {
	half := room / 2
	switch {
	case a+b <= room:
		return a, b
	case a <= half:
		return a, room - a
	case b <= half:
		return room - b, b
	}
	return half, half
}

// shortNodeID shortens a node ID for use inside a project name. Node IDs are a
// kind prefix plus a UUID ("p_7ad31c42-21e7-4fbd-aa3e-15a4660449be"), and 36
// characters of that are noise beside a six-letter purpose — the name is read by
// people, in Horizon and Skyline, next to dozens of others. Only the first six
// hex digits are kept, the same trade git makes with short commit hashes.
//
// Uniqueness survives it: the suffix only has to separate projects that share a
// NAME and an OWNER, and two of those collide only if their IDs also agree in
// the first six hex digits — one in sixteen million. The kind prefix stays so
// the suffix still reads as a node ID and can be pasted into a search.
//
// IDs that are not "<kind>_<uuid>" — the structural "root" and "unassigned"
// nodes — are left alone.
func shortNodeID(id string) string {
	prefix, rest, found := strings.Cut(id, "_")
	if !found {
		return id
	}
	block, _, _ := strings.Cut(rest, "-")
	if len(block) != 8 {
		return id
	}
	return prefix + "_" + block[:shortIDDigits]
}

// shortIDDigits is how many hex digits of the UUID a project name keeps.
const shortIDDigits = 6

// sanitizeProjectName makes an arbitrary node name safe for Keystone: it drops
// non-BMP runes (the project table is utf8mb3 — an emoji makes Keystone answer
// 500), drops control characters, and collapses whitespace runs to single spaces.
// Everything else — spaces, umlauts, slashes, punctuation — Keystone accepts.
// A name that is empty or all-whitespace is rejected with 400, so it comes back
// empty here and the caller falls back to the node ID.
func sanitizeProjectName(s string) string {
	var b strings.Builder
	pendingSpace := false
	for _, r := range s {
		switch {
		// Whitespace first: newlines and tabs are control characters too, but they
		// separate words and must collapse to a space rather than vanish.
		case unicode.IsSpace(r):
			pendingSpace = b.Len() > 0
		case r > 0xFFFF || unicode.IsControl(r) || r == utf8.RuneError:
			continue
		default:
			if pendingSpace {
				b.WriteRune(' ')
				pendingSpace = false
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}

// truncateRunes cuts s to at most n runes (Keystone counts characters, not bytes)
// and trims a trailing space so a cut never leaves a dangling separator.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:n]), " ")
}

// descriptionPathSeparator joins the budget path and the owner in a description.
const descriptionPathSeparator = " -> "

// budgetPath returns the names of the budgets a leaf hangs under, top-down and
// without the root, e.g. ["DHBW Mannheim", "Fakultät Technik", "Prof-X"]. It is
// what an operator in Horizon needs to tell where a project belongs. A budget
// missing from budgetByID ends the walk, as does a cycle.
func budgetPath(budgetByID map[string]model.Node, leaf model.Node) []string {
	var path []string
	seen := map[string]struct{}{}
	for id := leaf.ParentID; id != nil && *id != model.RootNodeID; {
		if _, dup := seen[*id]; dup {
			break
		}
		seen[*id] = struct{}{}
		budget, ok := budgetByID[*id]
		if !ok {
			break
		}
		name := budget.Name
		if name == "" {
			name = budget.ID
		}
		path = append(path, name)
		id = budget.ParentID
	}
	slices.Reverse(path)
	return path
}

// buildDescription constructs the OS project description for a leaf.
// Format: "budget -> … -> email: reason (managed project)", where the budgets are
// the leaf's path in the tree (see budgetPath) and email is the owner's address.
func buildDescription(leaf model.Node, path []string) string {
	prefix := path
	if email := leaf.OwnerEmail(); email != "" {
		prefix = append(slices.Clip(path), email)
	}
	body := leaf.Reason
	if body == "" {
		body = "Managed by DHBW resource management. Node: " + leaf.ID
	}
	if len(prefix) > 0 {
		body = strings.Join(prefix, descriptionPathSeparator) + ": " + body
	}
	return body + managedDescriptionSuffix
}
