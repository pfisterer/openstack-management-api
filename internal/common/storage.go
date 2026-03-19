package common

// StorageConfiguration holds configuration for the storage backend.
type StorageConfiguration struct {
	Type             string // e.g., "memory" or "postgres"
	ConnectionString string // e.g., Postgres DSN or empty for memory
	AddMockData      bool   // Whether to add mock data on startup
}
