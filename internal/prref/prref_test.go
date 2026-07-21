package prref

import (
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
)

func TestParseGitHubPullURL(t *testing.T) {
	ref, err := ParseGitHubPullURL("https://github.com/open-cli-collective/codereview-cli/pull/510")
	if err != nil {
		t.Fatalf("ParseGitHubPullURL error = %v", err)
	}
	want := gitprovider.PRRef{Host: "github.com", Owner: "open-cli-collective", Repo: "codereview-cli", Number: 510}
	if ref != want {
		t.Fatalf("ref = %+v, want %+v", ref, want)
	}
}

func TestParseGitLabMergeRequestURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want gitprovider.PRRef
	}{
		{
			name: "canonical",
			raw:  "https://gitlab.com/group/project/-/merge_requests/42",
			want: gitprovider.PRRef{Host: "gitlab.com", Owner: "group", Repo: "project", Number: 42},
		},
		{
			name: "nested namespace",
			raw:  "https://gitlab.example.com/group/subgroup/project/-/merge_requests/7",
			want: gitprovider.PRRef{Host: "gitlab.example.com", Owner: "group/subgroup", Repo: "project", Number: 7},
		},
		{
			name: "legacy without dash separator",
			raw:  "https://gitlab.example.com/group/project/merge_requests/9",
			want: gitprovider.PRRef{Host: "gitlab.example.com", Owner: "group", Repo: "project", Number: 9},
		},
		{
			name: "query and fragment ignored",
			raw:  "https://gitlab.com/group/project/-/merge_requests/42?tab=diffs#note_1",
			want: gitprovider.PRRef{Host: "gitlab.com", Owner: "group", Repo: "project", Number: 42},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := ParseGitLabMergeRequestURL(tt.raw)
			if err != nil {
				t.Fatalf("ParseGitLabMergeRequestURL(%q) error = %v", tt.raw, err)
			}
			if ref != tt.want {
				t.Fatalf("ref = %+v, want %+v", ref, tt.want)
			}
		})
	}
}

func TestParseGitLabMergeRequestURLRejectsInvalid(t *testing.T) {
	tests := []string{
		"https://gitlab.com/project/-/merge_requests/42",
		"https://gitlab.com/group/project/-/merge_requests/0",
		"https://gitlab.com/group/project/-/merge_requests/abc",
		"https://gitlab.com/group/project/-/merge_requests/42/diffs",
		"https://gitlab.com/group/project/-/issues/42",
		"http://gitlab.com/group/project/-/merge_requests/42",
		"https://gitlab.com/group/../project/-/merge_requests/42",
		"https://github.com/owner/repo/pull/12",
	}
	for _, raw := range tests {
		if _, err := ParseGitLabMergeRequestURL(raw); err == nil {
			t.Errorf("ParseGitLabMergeRequestURL(%q) = nil error, want error", raw)
		}
	}
}

func TestParsePullURLDetectsProvider(t *testing.T) {
	ref, provider, err := ParsePullURL("https://github.com/owner/repo/pull/12")
	if err != nil || provider != ProviderGitHub {
		t.Fatalf("ParsePullURL github = (%+v, %q, %v), want github", ref, provider, err)
	}
	ref, provider, err = ParsePullURL("https://gitlab.com/group/sub/repo/-/merge_requests/12")
	if err != nil || provider != ProviderGitLab {
		t.Fatalf("ParsePullURL gitlab = (%+v, %q, %v), want gitlab", ref, provider, err)
	}
	if ref.Owner != "group/sub" {
		t.Fatalf("owner = %q, want group/sub", ref.Owner)
	}
	if _, _, err := ParsePullURL("https://example.com/not/a/pr"); err == nil {
		t.Fatal("ParsePullURL invalid = nil error, want error")
	}
}

func TestSameHost(t *testing.T) {
	if !SameHost("https://GitLab.example.com/", "gitlab.example.com") {
		t.Fatal("SameHost normalized comparison failed")
	}
	if SameHost("gitlab.com", "github.com") {
		t.Fatal("SameHost distinct hosts matched")
	}
}
