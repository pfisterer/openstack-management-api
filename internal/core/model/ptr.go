package model

import "cmp"

// ClonePtr is a copy of *v that shares nothing with v (for a value without pointers inside); nil stays nil.
func ClonePtr[T any](v *T) *T {
	if v == nil {
		return nil
	}
	c := *v
	return &c
}

// MinPtr is a copy of the smaller of two optional values; nil sets nothing, so it is the other one, and nil when both are. The "only stricter" rule of the budget settings that are a plain number (local storage, idle times, the maximum project term).
func MinPtr[T cmp.Ordered](a, b *T) *T {
	switch {
	case a == nil:
		return ClonePtr(b)
	case b == nil || *a <= *b:
		return ClonePtr(a)
	default:
		return ClonePtr(b)
	}
}
