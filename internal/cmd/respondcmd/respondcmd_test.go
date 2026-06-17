package respondcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/threadreply"
	"github.com/open-cli-collective/codereview-cli/internal/view"
)

const prURL = "https://github.com/open-cli-collective/codereview-cli/pull/29"

var testRef = gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 29}

// stubClassifier returns a canned decision per request in order.
type stubClassifier struct {
	results []threadreply.Result
	calls   int
}

func (s *stubClassifier) ClassifyThreadReply(_ context.Context, _ threadreply.Request) (threadreply.Result, error) {
	result := s.results[s.calls]
	s.calls++
	return result, nil
}

func markerComment(login, body string) gitprovider.ThreadComment {
	return gitprovider.ThreadComment{
		Author: gitprovider.Identity{Login: login},
		Body:   "<!-- codereview:run-id=r1:action=a1:kind=inline_comment:sha=abc123:base=def456 -->\n" + body,
	}
}

func plainComment(login, body string) gitprovider.ThreadComment {
	return gitprovider.ThreadComment{Author: gitprovider.Identity{Login: login}, Body: body}
}

func newFake(t *testing.T) *gitprovider.Fake {
	t.Helper()
	fake := &gitprovider.Fake{}
	fake.SetIdentity(gitprovider.Identity{Login: "review-bot"})
	if err := fake.SetPR(testRef, gitprovider.PR{Ref: testRef, Title: "CR-20", URL: prURL}); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := fake.SetInlineThreads(testRef, []gitprovider.InlineThread{
		{
			ID:       "thread-addressed",
			Resolved: false,
			Path:     "main.go",
			Line:     10,
			Comments: []gitprovider.ThreadComment{
				markerComment("review-bot", "Nil check missing."),
				plainComment("author", "Fixed in latest commit."),
			},
		},
		{
			ID:       "thread-human-only",
			Resolved: false,
			Comments: []gitprovider.ThreadComment{
				plainComment("author", "Unrelated human discussion."),
				plainComment("reviewer-2", "agreed"),
			},
		},
	}); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}
	return fake
}

func newTestCommand(t *testing.T, factory RuntimeFactory) (*cobra.Command, *bytes.Buffer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, testConfig()); err != nil {
		t.Fatalf("Save config: %v", err)
	}
	var out bytes.Buffer
	cmd, opts := root.NewCommandWithOptions(&root.Options{
		ConfigPath: path,
		Stdin:      strings.NewReader(""),
		Stdout:     &out,
		Stderr:     &out,
	})
	RegisterWithFactory(cmd, opts, factory)
	return cmd, &out
}

func testConfig() config.File {
	return config.File{
		DefaultProfile: "home",
		Keyring:        config.KeyringConfig{Backend: "memory"},
		Profiles: map[string]config.Profile{
			"home": {
				Git: config.GitConfig{
					Host:          "github.com",
					AuthMode:      config.GitAuthModePAT,
					CredentialRef: "codereview/home",
				},
				LLM: config.LLMConfig{
					Provider: config.LLMProviderAnthropic,
					Auth:     config.LLMAuthSubscription,
					Adapter:  config.LLMAdapterClaudeCLI,
				},
			},
		},
	}
}

func factoryFor(fake *gitprovider.Fake, classifier threadreply.Classifier) RuntimeFactory {
	return func(_ *cobra.Command, _ *root.Options, _ config.File, _ config.Profile, _ gitprovider.PRRef) (Runtime, error) {
		return Runtime{
			Provider:        fake,
			PostingIdentity: gitprovider.Identity{Login: "review-bot"},
			Classifier:      classifier,
		}, nil
	}
}

func TestRespondLivePostsReplyAndResolves(t *testing.T) {
	fake := newFake(t)
	classifier := &stubClassifier{results: []threadreply.Result{
		{Decision: threadreply.DecisionAcknowledgeAndResolve, Reply: "Thanks, resolving."},
	}}
	cmd, out := newTestCommand(t, factoryFor(fake, classifier))

	if err := root.Execute(cmd, []string{"respond", prURL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	replies := fake.RecordedThreadReplies(testRef)
	if len(replies) != 1 || replies[0].ThreadID != "thread-addressed" || replies[0].Body != "Thanks, resolving." {
		t.Fatalf("replies = %#v, want one reply on thread-addressed", replies)
	}
	resolved := fake.RecordedResolvedThreads(testRef)
	if len(resolved) != 1 || resolved[0] != "thread-addressed" {
		t.Fatalf("resolved = %#v, want thread-addressed resolved", resolved)
	}
	if classifier.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1 (only cr-authored thread with reply)", classifier.calls)
	}
	if got := out.String(); !strings.Contains(got, "Replied: 1") || !strings.Contains(got, "Resolved: 1") {
		t.Fatalf("stdout = %q, want replied/resolved counts", got)
	}
}

func TestRespondDryRunDoesNotPost(t *testing.T) {
	fake := newFake(t)
	classifier := &stubClassifier{results: []threadreply.Result{
		{Decision: threadreply.DecisionAcknowledgeAndResolve, Reply: "Thanks."},
	}}
	cmd, out := newTestCommand(t, factoryFor(fake, classifier))

	if err := root.Execute(cmd, []string{"respond", "--dry-run", prURL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if replies := fake.RecordedThreadReplies(testRef); len(replies) != 0 {
		t.Fatalf("replies = %#v, want none in dry-run", replies)
	}
	if resolved := fake.RecordedResolvedThreads(testRef); len(resolved) != 0 {
		t.Fatalf("resolved = %#v, want none in dry-run", resolved)
	}
	if got := out.String(); !strings.Contains(got, "Mode: dry-run") || !strings.Contains(got, "Resolved: 1") {
		t.Fatalf("stdout = %q, want dry-run plan with resolved count", got)
	}
}

func TestRespondNoResolveThreadsRepliesWithoutResolving(t *testing.T) {
	fake := newFake(t)
	classifier := &stubClassifier{results: []threadreply.Result{
		{Decision: threadreply.DecisionAcknowledgeAndResolve, Reply: "Thanks."},
	}}
	cmd, _ := newTestCommand(t, factoryFor(fake, classifier))

	if err := root.Execute(cmd, []string{"respond", "--no-resolve-threads", prURL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if replies := fake.RecordedThreadReplies(testRef); len(replies) != 1 {
		t.Fatalf("replies = %#v, want one reply", replies)
	}
	if resolved := fake.RecordedResolvedThreads(testRef); len(resolved) != 0 {
		t.Fatalf("resolved = %#v, want no resolutions with --no-resolve-threads", resolved)
	}
}

func TestRespondSkipDecisionDoesNothing(t *testing.T) {
	fake := newFake(t)
	classifier := &stubClassifier{results: []threadreply.Result{{Decision: threadreply.DecisionSkip}}}
	cmd, out := newTestCommand(t, factoryFor(fake, classifier))

	if err := root.Execute(cmd, []string{"respond", prURL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if replies := fake.RecordedThreadReplies(testRef); len(replies) != 0 {
		t.Fatalf("replies = %#v, want none for skip", replies)
	}
	if got := out.String(); !strings.Contains(got, "Skipped: 1") {
		t.Fatalf("stdout = %q, want skipped count", got)
	}
}

func TestRespondJSONOutput(t *testing.T) {
	fake := newFake(t)
	classifier := &stubClassifier{results: []threadreply.Result{
		{Decision: threadreply.DecisionReplyOnly, Reply: "Here is why."},
	}}
	cmd, out := newTestCommand(t, factoryFor(fake, classifier))

	if err := root.Execute(cmd, []string{"respond", "--json", prURL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	var got view.RespondResult
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Considered != 1 || got.Replied != 1 || got.Resolved != 0 {
		t.Fatalf("result counts = %#v, want considered=1 replied=1 resolved=0", got)
	}
	if len(got.Threads) != 1 || got.Threads[0].Decision != string(threadreply.DecisionReplyOnly) {
		t.Fatalf("threads = %#v, want one reply_only thread", got.Threads)
	}
}

func TestRespondNoCandidatesSkipsClassifier(t *testing.T) {
	fake := &gitprovider.Fake{}
	fake.SetIdentity(gitprovider.Identity{Login: "review-bot"})
	if err := fake.SetPR(testRef, gitprovider.PR{Ref: testRef, Title: "t", URL: prURL}); err != nil {
		t.Fatalf("SetPR: %v", err)
	}
	if err := fake.SetInlineThreads(testRef, nil); err != nil {
		t.Fatalf("SetInlineThreads: %v", err)
	}
	classifier := &stubClassifier{}
	cmd, out := newTestCommand(t, factoryFor(fake, classifier))

	if err := root.Execute(cmd, []string{"respond", prURL}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if classifier.calls != 0 {
		t.Fatalf("classifier calls = %d, want 0", classifier.calls)
	}
	if got := out.String(); !strings.Contains(got, "Threads considered: 0") {
		t.Fatalf("stdout = %q, want zero considered", got)
	}
}

func TestRespondRejectsMissingPR(t *testing.T) {
	fake := newFake(t)
	cmd, _ := newTestCommand(t, factoryFor(fake, &stubClassifier{}))
	if err := root.Execute(cmd, []string{"respond"}); err == nil {
		t.Fatal("Execute error = nil, want usage error for missing PR argument")
	}
}
