package runartifact

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMarkerMatchesRequiresValidKindAndRunID(t *testing.T) {
	dir := t.TempDir()
	if err := WriteMarker(dir, KindReview, "run-1"); err != nil {
		t.Fatalf("WriteMarker: %v", err)
	}

	if !MarkerMatches(dir, KindReview, "run-1") {
		t.Fatal("MarkerMatches valid marker = false, want true")
	}
	if MarkerMatches(dir, KindReview, "run-2") {
		t.Fatal("MarkerMatches wrong run = true, want false")
	}
	if MarkerMatches(dir, KindThreadResponse, "run-1") {
		t.Fatal("MarkerMatches wrong kind = true, want false")
	}
}

func TestReadMarkerRejectsMalformedMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(MarkerPath(dir, KindReview), []byte(`{"schema_version":1,"kind":"thread_response","run_id":"run-1"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := ReadMarker(dir, KindReview); !errors.Is(err, ErrMarkerInvalid) {
		t.Fatalf("ReadMarker wrong kind error = %v, want ErrMarkerInvalid", err)
	}

	if err := os.WriteFile(MarkerPath(dir, KindReview), []byte(`{"schema_version":2,"kind":"review","run_id":"run-1"}`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := ReadMarker(dir, KindReview); !errors.Is(err, ErrMarkerInvalid) {
		t.Fatalf("ReadMarker wrong schema error = %v, want ErrMarkerInvalid", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "review-run.json"), []byte(`not json`), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, err := ReadMarker(dir, KindReview); !errors.Is(err, ErrMarkerInvalid) {
		t.Fatalf("ReadMarker malformed JSON error = %v, want ErrMarkerInvalid", err)
	}
}
