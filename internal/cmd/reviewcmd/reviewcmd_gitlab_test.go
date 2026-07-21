package reviewcmd

import (
	"context"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/exitcode"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func gitLabTestConfig() config.File {
	cfg := testConfig()
	home := cfg.Profiles["home"]
	home.Git.Provider = config.GitProviderGitLab
	home.Git.Host = "gitlab.example.com"
	cfg.Profiles["home"] = home
	cfg.RepositoryProfiles = nil
	return cfg
}

func TestReviewAcceptsGitLabMergeRequestURL(t *testing.T) {
	runner := &fakeRunner{result: testPipelineResult(false)}
	cmd, _ := newTestCommand(t, gitLabTestConfig(), fakeFactory(runner))

	err := root.Execute(cmd, []string{"--profile", "home", "review", "https://gitlab.example.com/group/subgroup/project/-/merge_requests/29", "--dry-run"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(runner.requests) != 1 {
		t.Fatalf("runner calls = %d, want 1", len(runner.requests))
	}
	want := gitprovider.PRRef{Host: "gitlab.example.com", Owner: "group/subgroup", Repo: "project", Number: 29}
	if runner.requests[0].PRRef != want {
		t.Fatalf("PRRef = %#v, want %#v", runner.requests[0].PRRef, want)
	}
}

func TestReviewRejectsGitLabURLForGitHubProviderProfile(t *testing.T) {
	cfg := gitLabTestConfig()
	home := cfg.Profiles["home"]
	home.Git.Provider = config.GitProviderGitHub
	cfg.Profiles["home"] = home
	cmd, _ := newTestCommand(t, cfg, func(context.Context, app.OpenRequest) (app.Runtime, error) {
		t.Fatal("runtime factory should not be called on provider mismatch")
		return app.Runtime{}, nil
	})

	err := root.Execute(cmd, []string{"--profile", "home", "review", "https://gitlab.example.com/group/project/-/merge_requests/29", "--dry-run"})
	if err == nil {
		t.Fatal("Execute error = nil, want provider mismatch")
	}
	if got := exitcode.FromError(err); got != exitcode.UsageError {
		t.Fatalf("exit code = %d, want usage", got)
	}
	if !strings.Contains(err.Error(), `PR URL is a gitlab URL but the configured git provider is "github"`) {
		t.Fatalf("error = %v, want provider mismatch detail", err)
	}
}

func TestReviewRejectsGitHubURLForGitLabProviderProfile(t *testing.T) {
	cmd, _ := newTestCommand(t, gitLabTestConfig(), func(context.Context, app.OpenRequest) (app.Runtime, error) {
		t.Fatal("runtime factory should not be called on provider mismatch")
		return app.Runtime{}, nil
	})

	err := root.Execute(cmd, []string{"--profile", "home", "review", "https://gitlab.example.com/group/project/pull/29", "--dry-run"})
	if err == nil {
		t.Fatal("Execute error = nil, want provider mismatch")
	}
	if !strings.Contains(err.Error(), `PR URL is a github URL but the configured git provider is "gitlab"`) {
		t.Fatalf("error = %v, want provider mismatch detail", err)
	}
}
