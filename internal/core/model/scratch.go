package model

import "fmt"

// Local storage of a workspace is what a workspace or a GPU kernel at the AI service may write outside its home and the project's shared folder: the container's writable layer (packages installed into the image's environment, /tmp), its logs and its scratch directories. Kubernetes counts it as ephemeral storage and evicts a pod above it. The home and ~/project-shared are the project's volume and do not count.
//
// A budget sets the most the projects below it get (Node.WorkspaceScratchGB). Like the release maximums this is no quantity: nothing is summed or handed down in parts. It holds for every project below the budget, a budget below may only lower it, and what applies to a project is the smallest value along its chain of budgets, root included (ResolveWorkspaceScratchGB). Where no budget sets one, the AI service applies its default (1 GB).

// MaxWorkspaceScratchGB bounds the value a budget may set, the same as the AI service's default upper bound: a node's disk also holds the images and the other pods.
const MaxWorkspaceScratchGB = 200

// StricterScratch is the smaller of two values; nil sets nothing.
func StricterScratch(a, b *int) *int {
	switch {
	case a == nil:
		return cloneInt(b)
	case b == nil || *a <= *b:
		return cloneInt(a)
	default:
		return cloneInt(b)
	}
}

// ResolveWorkspaceScratchGB is what applies below the given nodes, in any order: the smallest value any of them sets. Nil when none sets one.
func ResolveWorkspaceScratchGB(chain []Node) *int {
	var out *int
	for _, n := range chain {
		out = StricterScratch(out, n.WorkspaceScratchGB)
	}
	return out
}

// ValidateWorkspaceScratchGB checks a budget's own value against the bounds and against inherited, what the budgets above allow: a budget may only lower it, never raise it.
func ValidateWorkspaceScratchGB(v, inherited *int) error {
	if v == nil {
		return nil
	}
	if *v < 1 || *v > MaxWorkspaceScratchGB {
		return fmt.Errorf("local storage per workspace must be between 1 and %d GB", MaxWorkspaceScratchGB)
	}
	if inherited != nil && *v > *inherited {
		return fmt.Errorf("local storage per workspace: the budget above allows at most %d GB, so this one cannot allow %d", *inherited, *v)
	}
	return nil
}
