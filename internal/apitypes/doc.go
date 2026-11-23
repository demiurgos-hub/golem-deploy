// Package apitypes defines the JSON wire types of the Golem Deploy HTTP API
// (/api/v1). It is the contract shared by the daemon, the web client and
// docs/API.md: frontend/src/api/types.gen.ts is generated from this package
// with tygo (scripts/gen-types.sh), so keep it free of behaviour and of any
// dependency other than the standard library.
//
// Conventions:
//   - Timestamps are RFC 3339 strings in UTC (time.Time on the Go side).
//   - IDs are integers unless documented otherwise.
//   - Every non-2xx response body is an ErrorResponse.
//   - Successful responses are the resource itself; collections are wrapped in
//     an object with an "items" array so fields can be added later.
//   - Request bodies are decoded strictly: unknown fields and trailing data are
//     rejected with 400 bad_request.
//   - Optional request fields are pointers with omitempty; an absent field
//     leaves the stored value unchanged in PATCH requests.
//   - No type in this package ever carries a stored secret back to a client.
package apitypes
