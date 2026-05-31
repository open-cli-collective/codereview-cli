package gitprovider

import (
	"errors"
	"testing"
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
