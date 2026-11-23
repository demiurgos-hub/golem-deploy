package apitypes

import "time"

// Role is an account role. Admins can do everything; developers can use and
// operate projects and servers but not administer accounts, settings or
// destructive host-level resources. API tokens always act as developer.
type Role string

// Roles.
const (
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
)

// LoginRequest is the body of POST /api/v1/auth/login.
type LoginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// RegisterRequest is the body of POST /api/v1/auth/register.
// Username: ^[a-z0-9][a-z0-9._-]{2,31}$ after lower-casing.
// Password: 12 to 72 bytes (UTF-8).
type RegisterRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// SessionResponse is returned by login, refresh and password change. Send the
// token as "Authorization: Bearer <token>". Refresh before ExpiresAt; after
// SessionExpiresAt (the absolute session lifetime) a fresh login is required.
type SessionResponse struct {
	Token            string    `json:"token"`
	ExpiresAt        time.Time `json:"expires_at"`
	SessionExpiresAt time.Time `json:"session_expires_at"`
	Account          Account   `json:"account"`
}

// Account is an account as seen by clients. It never contains the password
// hash.
type Account struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Role      Role      `json:"role"`
	Approved  bool      `json:"approved"`
	CreatedAt time.Time `json:"created_at"`
}

// AccountList is the body of GET /api/v1/accounts.
type AccountList struct {
	Items []Account `json:"items"`
}

// ChangePasswordRequest is the body of POST /api/v1/account/password. On
// success every existing session of the account is invalidated and a new
// SessionResponse is returned.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// CreateAccountRequest is the body of POST /api/v1/accounts (admin).
type CreateAccountRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     Role   `json:"role"`
	Approved bool   `json:"approved"`
}

// UpdateAccountRequest is the body of PATCH /api/v1/accounts/{id} (admin).
// Absent fields are left unchanged. Changing the role or un-approving an
// account ends all of its sessions.
type UpdateAccountRequest struct {
	Role     *Role `json:"role,omitempty"`
	Approved *bool `json:"approved,omitempty"`
}

// APIToken describes a personal API token. The secret is only ever returned
// once, in CreateAPITokenResponse.
type APIToken struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
	// Prefix is the first characters of the secret ("gdp_" + 8 hex chars), for
	// recognising a token without revealing it.
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at" tstype:"string | null,required"`
	ExpiresAt  *time.Time `json:"expires_at" tstype:"string | null,required"`
}

// APITokenList is the body of GET /api/v1/account/tokens.
type APITokenList struct {
	Items []APIToken `json:"items"`
}

// CreateAPITokenRequest is the body of POST /api/v1/account/tokens.
// Name: 1 to 64 characters. ExpiresAt, when set, must be in the future.
type CreateAPITokenRequest struct {
	Name      string     `json:"name"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// CreateAPITokenResponse is returned once when a token is created. Secret is
// the bearer value ("gdp_..."); it cannot be retrieved again.
type CreateAPITokenResponse struct {
	Token  APIToken `json:"token"`
	Secret string   `json:"secret"`
}
