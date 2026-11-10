package model

import "fmt"

// The batch priority says how the batch jobs of a project at the AI service run: guaranteed (priority as a workspace, never evicted) or preemptible (a workspace of the GPU class evicts the job, which starts again from the beginning).
//
// A budget sets it for the projects below it (Node.BatchPriority). Like the local storage per workspace this is a policy, no quantity: it holds for every project below the budget, a budget below may only tighten it (preemptible is stricter than guaranteed), and what applies to a project is the strictest value along its chain of budgets, root included (ResolveBatchPriority). Where no budget sets one, the AI service treats the project as preemptible.

const (
	// BatchPriorityGuaranteed runs the jobs like a workspace: never evicted.
	BatchPriorityGuaranteed = "guaranteed"
	// BatchPriorityPreemptible lets a workspace evict the jobs.
	BatchPriorityPreemptible = "preemptible"
)

// StricterBatchPriority is the stricter of two values: preemptible over guaranteed; nil sets nothing.
func StricterBatchPriority(a, b *string) *string {
	switch {
	case a == nil:
		return ClonePtr(b)
	case b == nil || *a == BatchPriorityPreemptible:
		return ClonePtr(a)
	default:
		return ClonePtr(b)
	}
}

// ResolveBatchPriority is what applies below the given nodes, in any order: preemptible if any of them says so, guaranteed if some say that and none preemptible. Nil when none sets one.
func ResolveBatchPriority(chain []Node) *string {
	var out *string
	for _, n := range chain {
		out = StricterBatchPriority(out, n.BatchPriority)
	}
	return out
}

// ValidateBatchPriority checks a budget's own value against the allowed values and against inherited, what the budgets above resolve to: a budget may only tighten it, never guarantee what is preemptible above.
func ValidateBatchPriority(v, inherited *string) error {
	if v == nil {
		return nil
	}
	if *v != BatchPriorityGuaranteed && *v != BatchPriorityPreemptible {
		return fmt.Errorf("batch priority must be %q or %q", BatchPriorityGuaranteed, BatchPriorityPreemptible)
	}
	if inherited != nil && *inherited == BatchPriorityPreemptible && *v == BatchPriorityGuaranteed {
		return fmt.Errorf("batch priority: the budget above makes batch jobs preemptible, so this one cannot guarantee them")
	}
	return nil
}
