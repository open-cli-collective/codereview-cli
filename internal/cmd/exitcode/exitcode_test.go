package exitcode

import (
	"errors"
	"testing"
)

func TestFromError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "nil", err: nil, want: Success},
		{name: "generic", err: errors.New("boom"), want: Failure},
		{name: "usage", err: Usage(errors.New("bad args")), want: UsageError},
		{name: "auth config", err: AuthConfig(errors.New("missing token")), want: AuthConfigError},
		{name: "upstream", err: Upstream(errors.New("rate limited")), want: UpstreamError},
		{name: "success code with error becomes failure", err: With(Success, errors.New("bad success")), want: Failure},
		{name: "out of range coded error", err: With(99, errors.New("bad code")), want: Failure},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FromError(tt.err); got != tt.want {
				t.Fatalf("FromError(%v) = %d, want %d", tt.err, got, tt.want)
			}
		})
	}
}

func TestCodedErrorUnwraps(t *testing.T) {
	base := errors.New("wrapped")
	err := Usage(base)
	if !errors.Is(err, base) {
		t.Fatalf("Usage error must unwrap base error")
	}
	if err.Error() != "wrapped" {
		t.Fatalf("Usage error text = %q, want wrapped", err.Error())
	}
}
