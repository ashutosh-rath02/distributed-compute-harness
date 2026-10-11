package domain

import "time"

// Role is what an operator-API user may do (manager/roles.go has the
// route-by-route table). Higher roles include everything below them.
type Role string

const (
	RoleViewer   Role = "viewer"   // read-only: lists and gets, never secrets or join/pairing info
	RoleOperator Role = "operator" // viewer + run and cancel work, approve plans, upload files
	RoleAdmin    Role = "admin"    // everything, including users, policy, joining and updates
)

// Rank orders roles; an unknown role ranks below viewer, so it can do nothing.
func (r Role) Rank() int {
	switch r {
	case RoleViewer:
		return 1
	case RoleOperator:
		return 2
	case RoleAdmin:
		return 3
	}
	return 0
}

// Valid reports whether r is one of the three roles.
func (r Role) Valid() bool { return r.Rank() > 0 }

// User is a named operator-API credential. Only a hash of the token is
// stored: the token itself is shown once, when it is made.
type User struct {
	Name string `json:"name"`
	Role Role   `json:"role"`
	// TokenHash is the hex SHA-256 of the user's 256-bit random token.
	TokenHash string `json:"tokenHash"`
	// PrevTokenHash stays valid until PrevExpires after a graceful rotation,
	// so the person can switch tokens without being locked out in between.
	PrevTokenHash string    `json:"prevTokenHash,omitempty"`
	PrevExpires   time.Time `json:"prevExpires,omitempty"`
	// Epoch changes whenever the token is replaced; dashboard sessions carry
	// it, so replacing the token (or removing the user) signs out every
	// browser at once.
	Epoch    string    `json:"epoch"`
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"lastUsed,omitempty"`
	// Expires, if set, is when the user stops working (zero = never).
	Expires time.Time `json:"expires,omitempty"`
}
