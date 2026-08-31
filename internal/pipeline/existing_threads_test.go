package pipeline

import (
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

// comment builds one normalized thread comment with the two flags the filter
// reads.
func comment(body string, ours, findingMarker bool) threadcontext.Comment {
	return threadcontext.Comment{
		Body:                      body,
		AuthoredByPostingIdentity: ours,
		HasFindingMarker:          findingMarker,
	}
}

func thread(path string, comments ...threadcontext.Comment) threadcontext.Thread {
	return threadcontext.Thread{
		ID:       gitprovider.ThreadID("t"),
		Anchor:   threadcontext.Anchor{Path: path},
		Comments: comments,
	}
}

// Only a thread this identity opened to report a finding can be one it repeats.
func TestExistingFindingThreadsKeepsOnlyOurOwnFindingThreads(t *testing.T) {
	got := existingFindingThreads([]threadcontext.Thread{
		thread("a.go", comment("ours", true, true)),
		thread("b.go", comment("someone else's", false, true)),
		thread("c.go", comment("ours, but not a finding", true, false)),
	})

	if len(got) != 1 {
		t.Fatalf("kept %d threads, want 1: %+v", len(got), got)
	}
	if got[0].Path != "a.go" {
		t.Fatalf("kept the wrong thread: %+v", got[0])
	}
}

// A thread a human opened and this identity only replied to carries the human's
// text. Treating it as ours would let it suppress a real finding.
func TestExistingFindingThreadsSkipsAThreadWeOnlyRepliedTo(t *testing.T) {
	got := existingFindingThreads([]threadcontext.Thread{
		thread("a.go",
			comment("a human's finding", false, false),
			comment("our reply", true, true),
		),
	})

	if len(got) != 0 {
		t.Fatalf("kept %d threads, want 0: a thread we only replied to was treated as ours: %+v", len(got), got)
	}
}

// The opening comment is the finding; matching against a reply would compare
// against the wrong text.
func TestExistingFindingThreadsCarriesTheOpeningComment(t *testing.T) {
	got := existingFindingThreads([]threadcontext.Thread{
		thread("a.go",
			comment("the finding", true, true),
			comment("fixed in abc123", false, false),
		),
	})

	if len(got) != 1 {
		t.Fatalf("kept %d threads, want 1", len(got))
	}
	if got[0].Body != "the finding" {
		t.Fatalf("body = %q, want the opening comment", got[0].Body)
	}
}

// A thread with no comments carries no finding to compare against.
func TestExistingFindingThreadsSkipsAThreadWithNoComments(t *testing.T) {
	if got := existingFindingThreads([]threadcontext.Thread{thread("a.go")}); len(got) != 0 {
		t.Fatalf("kept %d threads, want 0: %+v", len(got), got)
	}
}
