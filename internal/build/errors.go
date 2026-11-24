package build

import (
	"errors"
	"fmt"
)

// UserError is a build failure whose message is safe to show to users: it
// names the repository file or setting at fault and never contains host
// paths, command output or internal error text.
type UserError struct {
	Msg string
	Err error
}

func (e *UserError) Error() string { return e.Msg }

func (e *UserError) Unwrap() error { return e.Err }

func userErr(format string, a ...any) error {
	return &UserError{Msg: fmt.Sprintf(format, a...)}
}

// SafeMessage returns the user-safe message of err when it is (or wraps) a
// *UserError, and fallback otherwise.
func SafeMessage(err error, fallback string) string {
	var ue *UserError
	if errors.As(err, &ue) && ue.Msg != "" {
		return ue.Msg
	}
	return fallback
}
