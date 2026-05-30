// Package exitcode centralizes cr's command-layer process exit taxonomy.
package exitcode

import "errors"

const (
	// Success means the program completed successfully.
	Success = 0
	// Failure is the generic non-usage failure code.
	Failure = 1
	// UsageError means command syntax, flags, or arguments were invalid.
	UsageError = 2
	// AuthConfigError means authentication or configuration prevented execution.
	AuthConfigError = 3
	// UpstreamError means an upstream service or network failure prevented execution.
	UpstreamError = 5
)

type codedError struct {
	code int
	err  error
}

func (e *codedError) Error() string {
	return e.err.Error()
}

func (e *codedError) Unwrap() error {
	return e.err
}

func (e *codedError) ExitCode() int {
	return e.code
}

type exitCoder interface {
	ExitCode() int
}

// With wraps err with an explicit process exit code. Nil stays nil so callers
// can safely write `return exitcode.With(..., err)`.
func With(code int, err error) error {
	if err == nil {
		return nil
	}
	if code == Success {
		code = Failure
	}
	return &codedError{code: code, err: err}
}

// Usage marks err as a usage error.
func Usage(err error) error {
	return With(UsageError, err)
}

// AuthConfig marks err as an authentication or configuration error.
func AuthConfig(err error) error {
	return With(AuthConfigError, err)
}

// Upstream marks err as an upstream or network error.
func Upstream(err error) error {
	return With(UpstreamError, err)
}

// FromError maps an error to cr's process exit taxonomy.
func FromError(err error) int {
	if err == nil {
		return Success
	}

	var coded exitCoder
	if errors.As(err, &coded) {
		switch code := coded.ExitCode(); code {
		case Failure, UsageError, AuthConfigError, UpstreamError:
			return code
		default:
			return Failure
		}
	}

	return Failure
}
