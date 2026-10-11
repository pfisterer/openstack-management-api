package reconciler

import (
	"slices"
	"strings"

	"github.com/pfisterer/openstack-management-api/internal/core/model"
	osclient "github.com/pfisterer/openstack-management-api/internal/provider/openstack/client"
)

// syncTerminationTag publishes a leaf's termination date on its OpenStack project
// as <TerminationTagPrefix><RFC3339>, so "what runs out when" can be answered from
// OpenStack alone — `openstack project list` and the Keystone API both return tags,
// and an operator holding neither an account here nor database access can read the
// date off the project itself. The value is the stored timestamp verbatim rather
// than a truncated date: it is what the API would answer, and a tag is a bad place
// to lose precision.
//
// Publishing only. Nothing enforces the date (see Node.TerminationDate) — a project
// past it keeps running, it is now merely visible.
//
// The owner rides along as <ContactTagPrefix><email>: whoever finds a project
// about to run out in OpenStack needs someone to write to, and that question
// used to be answered only once a project was released. Only the owner — the
// one person responsible — and kept current, so a transfer moves the tag too.
//
// Writes only on an actual change, because this runs for every managed leaf on every
// tick: an unconditional update would be one Keystone write per project per interval
// for a value that changes maybe twice in a project's life. Clearing the date in the
// tree removes the tag on the next tick, and every other tag (managed, resource-id,
// pending-deletion) is carried over untouched.
func (r *Reconciler) syncManagedTags(leaf model.Node, osProject osclient.ProjectInfo) {
	termination := ""
	if leaf.TerminationDate != nil {
		termination = *leaf.TerminationDate
	}

	// One rebuild and at most one write for both tags. Two independent syncs
	// would each rebuild from this same snapshot, so whichever wrote second
	// would carry the other's old value back — an update that silently undoes
	// the one before it, on exactly the ticks where both changed.
	newTags, changed := applyPrefixedTags(osProject.Tags,
		prefixedTag{r.cfg.TerminationTagPrefix, termination},
		prefixedTag{r.cfg.StatusTagPrefix, leaf.Status},
		prefixedTag{r.cfg.ContactTagPrefix, leaf.OwnerEmail()},
	)
	if !changed {
		return
	}

	r.log.Infow("Updating tags on OS project",
		"node_id", leaf.ID, "os_project_id", osProject.ID,
		"status", leaf.Status, "termination", termination, "contact", leaf.OwnerEmail(),
		"tags", newTags, "dry_run", r.cfg.DryRun)

	if r.cfg.DryRun {
		return
	}
	if _, err := r.osClient.UpdateProject(osProject.ID, osclient.ProjectUpdateOpts{
		Tags: &newTags,
	}); err != nil {
		r.log.Warnw("Failed to update tags on OS project",
			"node_id", leaf.ID, "os_project_id", osProject.ID, "error", err)
	}
}

// prefixedTag is one "<prefix><value>" tag the reconciler owns: it replaces any
// existing tag with that prefix, and an empty value removes it.
type prefixedTag struct {
	prefix string
	value  string
}

// applyPrefixedTags brings every owned prefix to its desired value in one pass and
// reports whether anything actually changed. An empty prefix is skipped, which is
// how a tag is switched off by configuration.
//
// The caller writes only when changed is true, because this runs for every managed
// leaf on every tick: an unconditional update would be one Keystone write per
// project per interval for values that change a handful of times in a project's
// life. Tags outside the owned prefixes (managed, resource-id, pending-deletion)
// are carried over untouched.
//
// "Changed" is decided per prefix, NOT by comparing the two lists: the owned tags
// are re-appended at the end, so a positional comparison would report a change on
// the first pass and then again on every pass if Keystone ever hands the list back
// in a different order — a write per project per tick, forever, with no difference
// to show for it.
func applyPrefixedTags(tags []string, owned ...prefixedTag) ([]string, bool) {
	changed := false
	out := make([]string, 0, len(tags)+len(owned))

	for _, tag := range tags {
		kept := true
		for _, o := range owned {
			if o.prefix != "" && strings.HasPrefix(tag, o.prefix) {
				kept = false
				if tag != o.prefix+o.value {
					changed = true // replaced, or removed because value is empty
				}
				break
			}
		}
		if kept {
			out = append(out, tag)
		}
	}

	for _, o := range owned {
		if o.prefix == "" || o.value == "" {
			continue
		}
		desired := o.prefix + o.value
		out = append(out, desired)
		if !slices.Contains(tags, desired) {
			changed = true // newly added
		}
	}
	return out, changed
}
