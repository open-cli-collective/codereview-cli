package gitprovider

import (
	"errors"
	"fmt"
)

// Typed provider errors. Concrete adapters map host-specific failures into
// these sentinels so callers can classify outcomes without knowing wire details.
var (
	ErrAuth       = errors.New("gitprovider: authentication failed")
	ErrPermission = errors.New("gitprovider: permission denied")
	ErrNotFound   = errors.New("gitprovider: target not found")
	ErrRetryable  = errors.New("gitprovider: retryable upstream error")
	ErrConflict   = errors.New("gitprovider: already exists")
	ErrStaleSHA   = errors.New("gitprovider: pinned SHA is no longer current")
	// ErrDiffTooLarge indicates the host declined to return a diff because it
	// exceeds the host API's size limit. GitHub, for example, responds with
	// HTTP 406 for pull request diffs beyond its line cap instead of returning
	// a truncated diff.
	ErrDiffTooLarge = errors.New("gitprovider: diff too large for host API")
)

// ProviderError annotates a typed provider failure with the operation that
// produced it and, when available, a more specific wrapped cause.
type ProviderError struct {
	Op   Operation
	Kind error
	Err  error
}

func (e *ProviderError) Error() string {
	switch {
	case e == nil:
		return "<nil>"
	case e.Kind == nil && e.Op != "" && e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Op, e.Err)
	case e.Kind == nil && e.Err != nil:
		return e.Err.Error()
	case e.Kind == nil && e.Op != "":
		return fmt.Sprintf("%s: provider error", e.Op)
	case e.Kind == nil:
		return "gitprovider: provider error"
	case e.Op != "" && e.Err != nil:
		return fmt.Sprintf("%s: %v: %v", e.Op, e.Kind, e.Err)
	case e.Op != "":
		return fmt.Sprintf("%s: %v", e.Op, e.Kind)
	case e.Err != nil:
		return fmt.Sprintf("%v: %v", e.Kind, e.Err)
	default:
		return e.Kind.Error()
	}
}

// Is makes Kind errors.Is-matchable. Unwrap exposes Err to the standard chain.
func (e *ProviderError) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	return errors.Is(e.Kind, target)
}

func (e *ProviderError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// WrapError wraps err with a typed provider error kind and operation.
func WrapError(kind error, op Operation, err error) error {
	if kind == nil {
		return err
	}
	return &ProviderError{Op: op, Kind: kind, Err: err}
}
