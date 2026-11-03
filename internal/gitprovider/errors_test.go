package gitprovider

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

func TestProviderErrorMatchesKindAndCause(t *testing.T) {
	err := WrapError(ErrConflict, OperationPostIssueComment, ErrRetryable)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("errors.Is(conflict wrapper, ErrConflict) = false, want true")
	}
	if !errors.Is(err, ErrRetryable) {
		t.Fatalf("errors.Is(conflict wrapper, ErrRetryable) = false, want true")
	}

	bare := WrapError(ErrConflict, OperationPostIssueComment, nil)
	if !errors.Is(bare, ErrConflict) {
		t.Fatalf("errors.Is(bare conflict, ErrConflict) = false, want true")
	}
	if errors.Is(bare, ErrRetryable) {
		t.Fatalf("errors.Is(bare conflict, ErrRetryable) = true, want false")
	}

	var providerErr *ProviderError
	if !errors.As(err, &providerErr) {
		t.Fatalf("errors.As(conflict wrapper, *ProviderError) = false, want true")
	}
	if providerErr.Op != OperationPostIssueComment {
		t.Fatalf("ProviderError.Op = %q, want %q", providerErr.Op, OperationPostIssueComment)
	}
}

func TestWrapErrorNilKindReturnsOriginalError(t *testing.T) {
	raw := errors.New("raw")
	if got := WrapError(nil, OperationGetPR, raw); !errors.Is(got, raw) {
		t.Fatalf("WrapError(nil, op, raw) = %v, want original error", got)
	}
	if got := WrapError(nil, OperationGetPR, nil); got != nil {
		t.Fatalf("WrapError(nil, op, nil) = %v, want nil", got)
	}
}

func TestProviderErrorZeroValueDoesNotPanic(t *testing.T) {
	err := (&ProviderError{}).Error()
	if err == "" {
		t.Fatal("zero ProviderError Error() = empty string, want non-empty")
	}
	withOp := (&ProviderError{Op: OperationGetPR}).Error()
	if withOp == "" {
		t.Fatal("ProviderError with op Error() = empty string, want non-empty")
	}
}

func TestRetryAfterErrorKeepsClassificationAndHint(t *testing.T) {
	inner := WrapError(ErrRetryable, OperationGetPR, errors.New("github: status 429"))
	wrapped := fmt.Errorf("outer: %w", &RetryAfterError{After: 7 * time.Second, Err: inner})

	if !errors.Is(wrapped, ErrRetryable) {
		t.Fatalf("errors.Is(ErrRetryable) = false for %v", wrapped)
	}
	var providerErr *ProviderError
	if !errors.As(wrapped, &providerErr) || providerErr.Op != OperationGetPR {
		t.Fatalf("errors.As(ProviderError) = %#v", providerErr)
	}
	if hint, ok := RetryAfterHint(wrapped); !ok || hint != 7*time.Second {
		t.Fatalf("RetryAfterHint = %s, %t", hint, ok)
	}
	if wrapped.Error() != "outer: "+inner.Error() {
		t.Fatalf("Error() = %q", wrapped)
	}
	if _, ok := RetryAfterHint(inner); ok {
		t.Fatal("RetryAfterHint found a hint on an unhinted error")
	}
}
