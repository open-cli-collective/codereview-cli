package reviewplan

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/review"
)

// postedBody is what Build renders into a comment, which is what a later run
// reads back from the host when it lists threads.
func postedBody(t *testing.T, req Request) string {
	t.Helper()
	plan, err := Build(req)
	if err != nil {
		t.Fatal(err)
	}
	inline := actionsOfKind(plan.Actions, ActionKindInlineComment)
	if len(inline) != 1 {
		t.Fatalf("want one inline comment to read a body from, got %d", len(inline))
	}
	return inline[0].InlineComment.Body
}

func inlineCount(t *testing.T, req Request) int {
	t.Helper()
	plan, err := Build(req)
	if err != nil {
		t.Fatal(err)
	}
	return len(actionsOfKind(plan.Actions, ActionKindInlineComment))
}

// A finding fixed by a later commit is still in the cumulative diff, so it
// anchors again and posts again. Repeating it against text that no longer
// exists teaches a reader to skim.
func TestBuildDoesNotRepeatAFindingAlreadyRaised(t *testing.T) {
	first := baseRequest()
	body := postedBody(t, first)

	second := baseRequest()
	second.ExistingThreads = []ExistingThread{{Path: "main.go", Body: body}}

	if got := inlineCount(t, second); got != 0 {
		t.Fatalf("posted %d inline comments for a finding already raised, want 0", got)
	}
}

// Suppressed from the thread, not from the review: a reviewer that still
// believes the finding stays on record.
func TestBuildKeepsARepeatedFindingInTheRollup(t *testing.T) {
	req := baseRequest()
	body := postedBody(t, baseRequest())
	req.ExistingThreads = []ExistingThread{{Path: "main.go", Body: body}}

	plan, err := Build(req)
	if err != nil {
		t.Fatal(err)
	}
	submits := actionsOfKind(plan.Actions, ActionKindSubmitReview)
	if len(submits) == 0 {
		t.Fatal("no submit-review action was planned")
	}
	if !strings.Contains(submits[0].SubmitReview.Body, "finding body") {
		t.Fatalf("the review body does not carry the suppressed finding:\n%s", submits[0].SubmitReview.Body)
	}
}

// The repeats worth suppressing are the ones a fix moved, so the line cannot be
// part of what identifies a finding.
func TestBuildSuppressesARepeatWhoseLineMoved(t *testing.T) {
	req := baseRequest()
	body := postedBody(t, baseRequest())
	req.Findings = []review.Finding{finding("f-1", "main.go",
		review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 14})}
	req.ExistingThreads = []ExistingThread{{Path: "main.go", Body: body}}

	if got := inlineCount(t, req); got != 0 {
		t.Fatalf("posted %d inline comments for a finding that only moved, want 0", got)
	}
}

// Markers carry a run id that differs every run, so leaving them in the
// comparison would make every one fail and suppress nothing.
func TestBuildSuppressesARepeatDespiteADifferentRunMarker(t *testing.T) {
	req := baseRequest()
	// What the host actually stores: the planned body with the run's marker
	// prepended at post time. The run id differs on every run.
	stale := "<!-- codereview:skip --> <!-- codereview:run-id=" +
		"11111111-2222-3333-4444-555555555555:action=inline_comment-001 -->\n\n" +
		postedBody(t, baseRequest())
	req.ExistingThreads = []ExistingThread{{Path: "main.go", Body: stale}}

	if got := inlineCount(t, req); got != 0 {
		t.Fatalf("posted %d inline comments; a differing run marker defeated the match", got)
	}
}

// Suppressing a near-match would lose real findings, so equality is exact once
// formatting is folded.
func TestBuildStillPostsADifferentFindingOnTheSameFile(t *testing.T) {
	req := baseRequest()
	other := "<!-- codereview:skip -->\n\nsomething else entirely\n\n" + inlineFooter
	req.ExistingThreads = []ExistingThread{{Path: "main.go", Body: other}}

	if got := inlineCount(t, req); got != 1 {
		t.Fatalf("posted %d inline comments, want 1: a different finding must still be raised", got)
	}
}

// The same words about a different file are a different claim.
func TestBuildStillPostsTheSameTextAboutAnotherFile(t *testing.T) {
	req := baseRequest()
	body := postedBody(t, baseRequest())
	req.ExistingThreads = []ExistingThread{{Path: "other.go", Body: body}}

	if got := inlineCount(t, req); got != 1 {
		t.Fatalf("posted %d inline comments, want 1: another file is another claim", got)
	}
}

// A thread carrying no text of its own cannot establish that anything was said.
func TestBuildIgnoresAnEmptyExistingThread(t *testing.T) {
	req := baseRequest()
	req.ExistingThreads = []ExistingThread{
		{Path: "main.go", Body: "  \n\n"},
		{Path: "main.go", Body: "<!-- codereview:skip -->"},
	}

	if got := inlineCount(t, req); got != 1 {
		t.Fatalf("posted %d inline comments, want 1: an empty thread suppressed a finding", got)
	}
}

// Formatting alone must not make the same text look new.
func TestNormalizeFindingTextFoldsFormatting(t *testing.T) {
	a := normalizeFindingText("Finding   body\n\nhere")
	b := normalizeFindingText("finding body here")
	if a != b {
		t.Fatalf("normalized %q and %q differently", a, b)
	}
}

// On a host without native file-level comments every file-anchored finding
// takes the fallback path, and such a finding re-anchors to the first hunk on
// each run, so it is the class most in need of suppression. The rendered header
// interpolates the file path, so stripping only the literal prefix would leave
// the path in the text and nothing would ever match.
func TestBuildSuppressesARepeatedFileLevelFinding(t *testing.T) {
	fallback := func() Request {
		req := baseRequest()
		req.ProviderCaps.NativeFileLevelComments = false
		req.Findings = []review.Finding{finding("f-1", "main.go",
			review.Anchor{Kind: review.AnchorKindFile})}
		return req
	}

	body := postedBody(t, fallback())
	if !strings.Contains(body, fileLevelFallbackPrefix+"main.go") {
		t.Fatalf("this test is not exercising the fallback path: %q", body)
	}

	req := fallback()
	req.ExistingThreads = []ExistingThread{{Path: "main.go", Body: body}}
	if got := inlineCount(t, req); got != 0 {
		t.Fatalf("posted %d inline comments for a repeated file-level finding, want 0", got)
	}
}

// A finding may quote an HTML, XML, or JSX comment out of the diff. Dropping it
// would take real text out of the comparison while leaving it in the posted
// body, so two findings differing only inside a quoted comment would collapse.
func TestBuildKeepsTextQuotedInsideANonMarkerComment(t *testing.T) {
	quoting := func(quoted string) Request {
		req := baseRequest()
		f := finding("f-1", "main.go",
			review.Anchor{Kind: review.AnchorKindLine, Side: review.DiffSideRight, Line: 12})
		f.Body = "this line is wrong: <!-- " + quoted + " -->"
		req.Findings = []review.Finding{f}
		return req
	}

	req := quoting("beta")
	req.ExistingThreads = []ExistingThread{{Path: "main.go", Body: postedBody(t, quoting("alpha"))}}

	if got := inlineCount(t, req); got != 1 {
		t.Fatalf("posted %d inline comments, want 1: two findings differing only "+
			"inside a quoted comment were treated as the same", got)
	}
}

// The marker's run id differs on every run, so it must not reach the key even
// when the span is unterminated.
func TestNormalizeFindingTextDropsAnUnterminatedMarker(t *testing.T) {
	if got := normalizeFindingText("before " + markerPrefix + "run-id=abc"); got != "before" {
		t.Fatalf("normalized = %q, want %q", got, "before")
	}
}

// An unterminated comment that is not a marker is ordinary quoted text.
func TestNormalizeFindingTextKeepsAnUnterminatedNonMarkerComment(t *testing.T) {
	if got := normalizeFindingText("before <!-- never closed"); !strings.Contains(got, "never closed") {
		t.Fatalf("normalized = %q, want the quoted text kept", got)
	}
}
