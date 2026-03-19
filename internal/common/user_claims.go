package common

// UserClaims holds the relevant user information extracted from the ID token.
type UserClaims struct {
	Subject           string `json:"sub"`
	Email             string `json:"email,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	Name              string `json:"name,omitempty"`
}
