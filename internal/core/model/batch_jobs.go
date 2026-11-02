package model

import "fmt"

// Batch jobs are GPU jobs without a workspace at the AI service. A budget decides whether the projects below it have them and within which bounds (Node.BatchJobs): how many jobs a person may have running and waiting at the same time, and how long one job may run at the latest.
//
// Like a resource of a budget they are there or not: a budget that sets none has none, and so have the projects below it, as a resource missing from a limit is 0. The root sets the most anybody gets; a budget below may switch them on only where the budget above has them, and with values at most those above. A budget that passes on the limit of the one above (InheritsLimit) passes on its batch jobs too and sets none of its own. What applies to a project is the strictest value of each bound along its chain of budgets (ResolveBatchJobs), so a budget above that lowers its values or switches them off holds for everything below without anything being carried down.

// MaxJobHours is the longest run time a budget may allow, the AI service's upper bound (workspace.MaxJobHours there).
const MaxJobHours = 24

// BatchJobs are a budget's batch jobs; set, they are on.
type BatchJobs struct {
	// JobsPerPerson is how many batch jobs a person may have running and waiting at the same time in a project below; at least 1.
	JobsPerPerson int `json:"jobs_per_person" example:"2"`
	// MaxHours is how long one job may run at the latest, 1 to MaxJobHours.
	MaxHours int `json:"max_hours" example:"4"`
}

// On tells whether the value allows batch jobs at all.
func (b *BatchJobs) On() bool { return b != nil && b.JobsPerPerson > 0 && b.MaxHours > 0 }

// Clone returns a copy, nil for nil.
func (b *BatchJobs) Clone() *BatchJobs {
	if b == nil {
		return nil
	}
	c := *b
	return &c
}

// ResolveBatchJobs is what applies below the given nodes, in any order: off when one of them that does not inherit its limit sets none, else the fewest jobs and the shortest run time any of them allows. Nil (off) for an empty chain or one of inheriting nodes only.
func ResolveBatchJobs(chain []Node) *BatchJobs {
	var out *BatchJobs
	for _, n := range chain {
		if n.InheritsLimit {
			continue
		}
		if !n.BatchJobs.On() {
			return nil
		}
		if out == nil {
			out = n.BatchJobs.Clone()
			continue
		}
		out.JobsPerPerson = min(out.JobsPerPerson, n.BatchJobs.JobsPerPerson)
		out.MaxHours = min(out.MaxHours, n.BatchJobs.MaxHours)
	}
	return out
}

// ValidateBatchJobs checks a budget's own value against the bounds and against inherited, what the budgets above allow (ResolveBatchJobs over them); root tells that there is nothing above, so only the bounds apply. A budget below may switch them on only where those above have them, with values at most theirs.
func ValidateBatchJobs(v, inherited *BatchJobs, root bool) error {
	if v == nil {
		return nil
	}
	if v.JobsPerPerson < 1 {
		return fmt.Errorf("batch jobs: at least 1 job per person; to switch them off, clear them")
	}
	if v.MaxHours < 1 || v.MaxHours > MaxJobHours {
		return fmt.Errorf("batch jobs: the run time must be between 1 and %d hours", MaxJobHours)
	}
	switch {
	case root:
		return nil
	case !inherited.On():
		return fmt.Errorf("batch jobs: the budget above has none, so this one cannot have any")
	case v.JobsPerPerson > inherited.JobsPerPerson:
		return fmt.Errorf("batch jobs: the budget above allows at most %d jobs per person, so this one cannot allow %d", inherited.JobsPerPerson, v.JobsPerPerson)
	case v.MaxHours > inherited.MaxHours:
		return fmt.Errorf("batch jobs: the budget above allows at most %d hours per job, so this one cannot allow %d", inherited.MaxHours, v.MaxHours)
	}
	return nil
}
