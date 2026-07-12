package pipeline

import (
	"errors"
	"fmt"
)

// FailureKind describes whether a live planning failure can be resumed safely.
type FailureKind uint8

const (
	// FailureTransient is an attempt-scoped failure that may succeed on retry.
	FailureTransient FailureKind = iota
	// FailureDurableBlocking preserves resumable task state.
	FailureDurableBlocking
	// FailureTerminal cannot be resumed safely.
	FailureTerminal
)

type classifiedFailure struct {
	kind FailureKind
	err  error
}

func (e *classifiedFailure) Error() string { return e.err.Error() }
func (e *classifiedFailure) Unwrap() error { return e.err }

// ClassifyFailure returns the recovery class for err.
func ClassifyFailure(err error) FailureKind {
	var classified *classifiedFailure
	if errors.As(err, &classified) {
		return classified.kind
	}
	if errors.Is(err, errLLMTaskFailedBlocking) {
		return FailureDurableBlocking
	}
	return FailureTransient
}

// Failure marks err with a recovery class.
func Failure(kind FailureKind, err error) error {
	if err == nil {
		return nil
	}
	if kind > FailureTerminal {
		return fmt.Errorf("pipeline: invalid failure kind %d: %w", kind, err)
	}
	return &classifiedFailure{kind: kind, err: err}
}
