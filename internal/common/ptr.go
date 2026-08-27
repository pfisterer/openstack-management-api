package common

// Ptr returns a pointer to v. It exists so a caller can hand an optional field
// (a *string / *bool set only sometimes) an inline literal without a temporary
// variable. Package-local copies of this had grown in tree/ and identity/; one
// definition keeps the whole module saying "pointer to a value" the same way.
func Ptr[T any](v T) *T {
	return &v
}
