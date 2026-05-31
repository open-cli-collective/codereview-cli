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

// Is makes both Kind and the wrapped cause errors.Is-matchable.
func (e *ProviderError) Is(target error) bool {
	if e == nil || target == nil {
		return false
	}
	if errors.Is(e.Kind, target) {
		return true
	}
	return errors.Is(e.Err, target)
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
