package pipeline

import (
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/threadcontext"
)

func thread(path string, crAuthored, resolved bool, bodies ...string) threadcontext.Thread {
	comments := make([]threadcontext.Comment, 0, len(bodies))
	for _, body := range bodies {
		comments = append(comments, threadcontext.Comment{Body: body})
	}
	return threadcontext.Thread{
		ID:       gitprovider.ThreadID("t"),
		Resolved: resolved,
		Anchor:   threadcontext.Anchor{Path: path},
		Comments: comments,
		Status:   threadcontext.Status{CRAuthoredFinding: crAuthored},
	}
}

// A human quoting the same code is not this review repeating itself, so only
// threads this identity opened to report a finding can suppress one.
func TestExistingFindingThreadsKeepsOnlyOurFindingThreads(t *testing.T) {
	got := existingFindingThreads([]threadcontext.Thread{
		thread("a.go", true, true, "ours, resolved"),
		thread("b.go", true, false, "ours, open"),
		thread("c.go", false, true, "someone else's"),
	})

	if len(got) != 2 {
		t.Fatalf("kept %d threads, want 2: %+v", len(got), got)
	}
	for _, e := range got {
		if e.Path == "c.go" {
			t.Fatalf("a thread this identity did not author was kept: %+v", e)
		}
	}
	// Resolution is carried through rather than filtered on: an open thread
	// means the finding has been said too.
	if !got[0].Resolved || got[1].Resolved {
		t.Fatalf("resolution was not carried through: %+v", got)
	}
}

// The opening comment is the finding; the rest is the conversation about it,
// and matching against a reply would compare against the wrong text.
func TestExistingFindingThreadsCarriesTheOpeningComment(t *testing.T) {
	got := existingFindingThreads([]threadcontext.Thread{
		thread("a.go", true, true, "the finding", "fixed in abc123", "thanks"),
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
	if got := existingFindingThreads([]threadcontext.Thread{thread("a.go", true, true)}); len(got) != 0 {
		t.Fatalf("kept %d threads, want 0: %+v", len(got), got)
	}
}
