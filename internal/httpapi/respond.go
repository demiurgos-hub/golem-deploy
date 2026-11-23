package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/demiurgos-hub/golem-deploy/internal/apitypes"
)

// apiError is an error with a client-facing status, code and message. cause
// is logged with the request id and never sent to the client.
type apiError struct {
	status  int
	code    apitypes.ErrorCode
	message string
	field   string
	header  http.Header
	cause   error
}

func (e *apiError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.code, e.message, e.cause)
	}
	return string(e.code) + ": " + e.message
}

func (e *apiError) Unwrap() error { return e.cause }

func newError(status int, code apitypes.ErrorCode, message string) *apiError {
	return &apiError{status: status, code: code, message: message}
}

func badRequest(message string) *apiError {
	return newError(http.StatusBadRequest, apitypes.ErrorCodeBadRequest, message)
}

func invalidField(field, message string) *apiError {
	e := newError(http.StatusBadRequest, apitypes.ErrorCodeValidationFailed, message)
	e.field = field
	return e
}

func notFound(what string) *apiError {
	return newError(http.StatusNotFound, apitypes.ErrorCodeNotFound, what+" not found")
}

func conflict(code apitypes.ErrorCode, message string) *apiError {
	return newError(http.StatusConflict, code, message)
}

var (
	errUnauthenticated = newError(http.StatusUnauthorized, apitypes.ErrorCodeUnauthenticated, "authentication required")
	errForbidden       = newError(http.StatusForbidden, apitypes.ErrorCodeForbidden, "your role does not allow this action")
	errSessionRequired = newError(http.StatusForbidden, apitypes.ErrorCodeSessionRequired, "this action requires a login session; API tokens cannot use it")
	errNotImplemented  = newError(http.StatusNotImplemented, apitypes.ErrorCodeNotImplemented, "this endpoint is not implemented yet")
	errUnavailable     = newError(http.StatusServiceUnavailable, apitypes.ErrorCodeUnavailable, "service temporarily unavailable")
)

// internal wraps an unexpected error as a 500 whose details stay in the log.
func internal(cause error) *apiError {
	e := newError(http.StatusInternalServerError, apitypes.ErrorCodeInternal, "internal server error")
	e.cause = cause
	return e
}

// writeJSON writes v with status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(v)
}

// writeError writes err as the error envelope. Anything that is not an
// *apiError becomes internal_error. Internal causes are logged, never sent.
func (s *Server) writeError(w http.ResponseWriter, r *http.Request, err error) {
	var ae *apiError
	if !errors.As(err, &ae) {
		ae = internal(err)
	}
	reqID := requestIDFrom(r.Context())
	switch {
	case ae.status >= 500 && ae.cause != nil:
		s.logger.Error("request failed", "request_id", reqID, "method", r.Method, "path", r.URL.Path, "code", ae.code, "err", ae.cause.Error())
	case ae.cause != nil:
		s.logger.Info("request rejected", "request_id", reqID, "code", ae.code, "err", ae.cause.Error())
	}
	for k, vs := range ae.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	writeJSON(w, ae.status, apitypes.ErrorResponse{Error: apitypes.ErrorBody{
		Code:      ae.code,
		Message:   ae.message,
		RequestID: reqID,
		Field:     ae.field,
	}})
}

// decodeJSON strictly decodes a JSON request body into dst: the media type
// must be application/json, unknown fields and trailing data are rejected and
// the route's body limit applies.
func decodeJSON(r *http.Request, dst any) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return newError(http.StatusUnsupportedMediaType, apitypes.ErrorCodeUnsupportedMediaType, "request body must be application/json")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return decodeError(err)
		}
		return badRequest("request body must contain a single JSON value")
	}
	return nil
}

func decodeError(err error) error {
	var mbe *http.MaxBytesError
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &mbe):
		return newError(http.StatusRequestEntityTooLarge, apitypes.ErrorCodeRequestTooLarge,
			fmt.Sprintf("request body exceeds the %d byte limit", mbe.Limit))
	case errors.Is(err, io.EOF):
		return badRequest("request body is required")
	case errors.As(err, &syn), errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("request body is not valid JSON")
	case errors.As(err, &typ):
		e := badRequest("request field has the wrong type")
		e.field = clip(typ.Field, 64)
		return e
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		e := badRequest("request body contains an unknown field")
		e.field = clip(name, 64)
		return e
	default:
		return badRequest("request body could not be decoded")
	}
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
