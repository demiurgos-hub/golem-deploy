package apitypes

// ErrorResponse is the body of every non-2xx response.
type ErrorResponse struct {
	Error ErrorBody `json:"error"`
}

// ErrorBody describes a failed request. Message is safe to show to users and
// never contains internal details; RequestID matches the X-Request-Id
// response header and the daemon log line that holds those details.
type ErrorBody struct {
	Code      ErrorCode `json:"code"`
	Message   string    `json:"message"`
	RequestID string    `json:"request_id"`
	// Field names the offending request field for validation_failed errors.
	Field string `json:"field,omitempty"`
}

// ErrorCode is a stable, snake_case machine-readable error identifier.
// Clients should switch on the code, not on the message or HTTP status.
type ErrorCode string

// Error codes. The HTTP status each one is used with is noted in parentheses.
const (
	// ErrorCodeBadRequest: malformed JSON, unknown fields, trailing data, bad query
	// or path parameters (400).
	ErrorCodeBadRequest ErrorCode = "bad_request"
	// ErrorCodeValidationFailed: a field is well-formed JSON but violates a rule;
	// ErrorBody.Field names it (400).
	ErrorCodeValidationFailed ErrorCode = "validation_failed"
	// ErrorCodeInvalidCurrentPassword: the current password supplied to a password
	// change or API token creation is wrong (400).
	ErrorCodeInvalidCurrentPassword ErrorCode = "invalid_current_password"
	// ErrorCodeUnauthenticated: missing, malformed, expired or revoked credentials,
	// or the account behind them is gone or unapproved (401). Clients should
	// drop the stored token and show the login page.
	ErrorCodeUnauthenticated ErrorCode = "unauthenticated"
	// ErrorCodeInvalidCredentials: login failed. The same body is returned for an
	// unknown user, a wrong password and an unapproved account (401).
	ErrorCodeInvalidCredentials ErrorCode = "invalid_credentials"
	// ErrorCodeForbidden: authenticated, but the identity's role is too low, or
	// the resource is reserved to admins (e.g. the log of a restricted job). API
	// tokens never exceed the developer role (403).
	ErrorCodeForbidden ErrorCode = "forbidden"
	// ErrorCodeSessionRequired: the route needs a session JWT and was called with an
	// API token (403).
	ErrorCodeSessionRequired ErrorCode = "session_required"
	// ErrorCodeRegistrationDisabled: self-registration is turned off (403).
	ErrorCodeRegistrationDisabled ErrorCode = "registration_disabled"
	// ErrorCodeNotFound: unknown route or resource (404).
	ErrorCodeNotFound ErrorCode = "not_found"
	// ErrorCodeMethodNotAllowed: the route exists with another method; the Allow
	// header lists the valid ones (405).
	ErrorCodeMethodNotAllowed ErrorCode = "method_not_allowed"
	// ErrorCodeUsernameTaken: an account with that username exists (409).
	ErrorCodeUsernameTaken ErrorCode = "username_taken"
	// ErrorCodeNameTaken: a project or server with that name exists (409).
	ErrorCodeNameTaken ErrorCode = "name_taken"
	// ErrorCodePortInUse: the requested host port is already assigned (409).
	ErrorCodePortInUse ErrorCode = "port_in_use"
	// ErrorCodeLastAdmin: the change would leave no approved admin account (409).
	ErrorCodeLastAdmin ErrorCode = "last_admin"
	// ErrorCodeCannotModifySelf: an admin tried to delete, demote or unapprove their
	// own account (409).
	ErrorCodeCannotModifySelf ErrorCode = "cannot_modify_self"
	// ErrorCodeJobInProgress: another job is queued or running for the same subject
	// (409).
	ErrorCodeJobInProgress ErrorCode = "job_in_progress"
	// ErrorCodeJobNotActive: the job already finished and cannot be cancelled (409).
	ErrorCodeJobNotActive ErrorCode = "job_not_active"
	// ErrorCodeSchemaMismatch: the backup's schema fingerprint differs from
	// the fingerprint of the server's build, so the server would not load
	// it; repeat the restore with force to do it anyway (409).
	ErrorCodeSchemaMismatch ErrorCode = "schema_mismatch"
	// ErrorCodeConflict: any other state conflict, e.g. deleting something that is
	// still in use or the API token limit (409).
	ErrorCodeConflict ErrorCode = "conflict"
	// ErrorCodeBadHost: the Host header names neither an IP address, localhost
	// nor a configured public host name (--public-host). Protects against DNS
	// rebinding (421).
	ErrorCodeBadHost ErrorCode = "bad_host"
	// ErrorCodeRequestTooLarge: the request body exceeds the route's limit (413).
	ErrorCodeRequestTooLarge ErrorCode = "request_too_large"
	// ErrorCodeUnsupportedMediaType: a JSON route got a body that is not
	// application/json (415).
	ErrorCodeUnsupportedMediaType ErrorCode = "unsupported_media_type"
	// ErrorCodeRateLimited: too many attempts or too many concurrent requests (e.g.
	// log streams); the Retry-After header gives the wait in seconds (429).
	ErrorCodeRateLimited ErrorCode = "rate_limited"
	// ErrorCodeInsufficientStorage: an upload would exceed the static store's
	// size quota or file count cap, or leave too little free disk space on
	// the data directory's file system (507).
	ErrorCodeInsufficientStorage ErrorCode = "insufficient_storage"
	// ErrorCodeInternal: unexpected server-side failure; details are in the daemon
	// log under the request id (500).
	ErrorCodeInternal ErrorCode = "internal_error"
	// ErrorCodeNotImplemented: the route is part of the contract but not built yet
	// (501).
	ErrorCodeNotImplemented ErrorCode = "not_implemented"
	// ErrorCodeUnavailable: a dependency (e.g. Docker) is unreachable, the daemon
	// is shutting down, or the server is too busy checking passwords; retry after
	// the Retry-After header when present (503).
	ErrorCodeUnavailable ErrorCode = "service_unavailable"
)
