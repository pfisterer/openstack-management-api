package model

// ResolveMaxProjectTermDays is the maximum project term that applies below the given nodes, in any order: the shortest any of them sets (Node.MaxProjectTermDays). Nil when none sets one. Unlike the other budget settings it is also copied down to a budget created or moved below (the tree's fitMaxTerm), but what applies is still the shortest along the chain.
func ResolveMaxProjectTermDays(chain []Node) *int {
	var shortest *int
	for _, n := range chain {
		if n.MaxProjectTermDays != nil && (shortest == nil || *n.MaxProjectTermDays < *shortest) {
			shortest = n.MaxProjectTermDays
		}
	}
	return shortest
}
