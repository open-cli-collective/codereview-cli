package runartifact

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/prref"
	"github.com/open-cli-collective/codereview-cli/internal/statepaths"
)

func TestForRunUsesCompactPath(t *testing.T) {
	const (
		headSHA         = "0123456789abcdef0123456789abcdef01234567"
		baseSHA         = "fedcba9876543210fedcba9876543210fedcba98"
		profile         = "signalft-reviewer-profile"
		postingIdentity = "signalft-reviewer-bot"
		agentID         = "frontend:react-correctness"
		runID           = "123e4567-e89b-12d3-a456-426614174000"
	)
	ref := gitprovider.PRRef{Host: "github.com", Owner: "SignalFT", Repo: "signal-adminapp-frontend", Number: 123}
	pr := gitprovider.PR{
		Ref:  ref,
		Head: gitprovider.PRBranchRef{SHA: headSHA},
		Base: gitprovider.PRBranchRef{SHA: baseSHA},
	}
	layout := statepaths.NewLayout(filepath.FromSlash("C:/Users/Konstantin/AppData/Local/cr/data"), "")

	paths, err := ForRun(layout, ref, pr, profile, postingIdentity, runID)
	if err != nil {
		t.Fatalf("ForRun: %v", err)
	}
	prKey, err := statepaths.PRKey(ref.Host, ref.Owner, ref.Repo, ref.Number)
	if err != nil {
		t.Fatalf("PRKey: %v", err)
	}
	scope, err := statepaths.ResumeScope(profile, postingIdentity)
	if err != nil {
		t.Fatalf("ResumeScope: %v", err)
	}
	wantScopeHash := statepaths.KeyHash(prKey, headSHA, baseSHA, profile, postingIdentity)
	runsRoot := filepath.Join(layout.DataRoot, "runs")
	components := func(dir string) []string {
		rel, err := filepath.Rel(runsRoot, dir)
		if err != nil {
			t.Fatalf("Rel(%q): %v", dir, err)
		}
		return strings.Split(rel, string(filepath.Separator))
	}
	gotComponents := components(paths.Dir)
	if len(gotComponents) != 5 {
		t.Fatalf("path components below runs = %d, want 5: %q", len(gotComponents), paths.Dir)
	}
	if gotComponents[0] != prKey {
		t.Fatalf("PR key component = %q, want readable key %q", gotComponents[0], prKey)
	}
	if gotComponents[1] != prref.ShortSHA(headSHA) || len(gotComponents[1]) != 12 {
		t.Fatalf("head component = %q, want 12-char %q", gotComponents[1], prref.ShortSHA(headSHA))
	}
	if gotComponents[2] != prref.ShortSHA(baseSHA) || len(gotComponents[2]) != 12 {
		t.Fatalf("base component = %q, want 12-char %q", gotComponents[2], prref.ShortSHA(baseSHA))
	}
	if gotComponents[3] != wantScopeHash || len(gotComponents[3]) != 12 {
		t.Fatalf("scope component = %q, want 12-char tuple hash %q", gotComponents[3], wantScopeHash)
	}
	if gotComponents[4] != "run-"+runID {
		t.Fatalf("run component = %q, want full UUID %q", gotComponents[4], "run-"+runID)
	}

	legacyDir := filepath.Join(layout.DataRoot, "runs", prKey, headSHA, baseSHA, scope, "run-"+statepaths.Encode(runID))
	legacyReviewerRepo := filepath.Join(legacyDir, "workbench", "reviewers", statepaths.Encode(agentID), "repo")
	compactReviewerRepo := filepath.Join(paths.WorkbenchDir, "reviewers", statepaths.Encode(agentID), "repo")
	if len(legacyReviewerRepo) <= 260 || len(compactReviewerRepo) >= 260 {
		t.Fatalf("reviewer repo path lengths = legacy %d, compact %d; want legacy > 260 and compact < 260", len(legacyReviewerRepo), len(compactReviewerRepo))
	}

	otherProfile, err := ForRun(layout, ref, pr, "other-profile", postingIdentity, runID)
	if err != nil {
		t.Fatalf("ForRun with other profile: %v", err)
	}
	if components(otherProfile.Dir)[3] == gotComponents[3] {
		t.Fatal("scope component did not change when profile changed")
	}
	otherIdentity, err := ForRun(layout, ref, pr, profile, "other-posting-identity", runID)
	if err != nil {
		t.Fatalf("ForRun with other posting identity: %v", err)
	}
	if components(otherIdentity.Dir)[3] == gotComponents[3] {
		t.Fatal("scope component did not change when posting identity changed")
	}

	for _, test := range []struct {
		name            string
		profile         string
		postingIdentity string
	}{
		{name: "blank profile", profile: "", postingIdentity: postingIdentity},
		{name: "blank posting identity", profile: profile, postingIdentity: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ForRun(layout, ref, pr, test.profile, test.postingIdentity, runID); err == nil {
				t.Fatal("ForRun accepted blank resume scope value")
			}
		})
	}
}

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
