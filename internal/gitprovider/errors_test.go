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
