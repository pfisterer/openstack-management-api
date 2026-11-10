package tree

// RequireGroupMembership switches requireGroupMembership for a test and returns a function that restores it.
func RequireGroupMembership(on bool) (restore func()) {
	was := requireGroupMembership
	requireGroupMembership = on
	return func() { requireGroupMembership = was }
}
