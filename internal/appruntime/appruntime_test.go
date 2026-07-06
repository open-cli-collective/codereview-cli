package appruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestRetentionPolicyFromConfig(t *testing.T) {
	t.Run("omitted max age uses default policy", func(t *testing.T) {
		got := RetentionPolicyFromConfig(config.RetentionConfig{})
		if got.LiveForever || got.LiveMaxAge != 0 || got.DryRunMaxAge != 0 {
			t.Fatalf("retention policy = %#v, want zero-value default policy", got)
		}
	})

	t.Run("zero max age keeps live runs forever", func(t *testing.T) {
		maxAgeDays := 0
		got := RetentionPolicyFromConfig(config.RetentionConfig{MaxAgeDays: &maxAgeDays})
		if !got.LiveForever || got.LiveMaxAge != 0 || got.DryRunMaxAge != 0 {
			t.Fatalf("retention policy = %#v, want live forever", got)
		}
	})

	t.Run("positive max age maps to day duration", func(t *testing.T) {
		maxAgeDays := 30
		got := RetentionPolicyFromConfig(config.RetentionConfig{MaxAgeDays: &maxAgeDays})
		if got.LiveForever || got.LiveMaxAge != 30*24*time.Hour || got.DryRunMaxAge != 0 {
			t.Fatalf("retention policy = %#v, want 30-day live retention", got)
		}
	})
}

func TestResolveRepoRoot(t *testing.T) {
	root, err := ResolveRepoRoot(context.Background())
	if err != nil {
		t.Fatalf("ResolveRepoRoot: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	want := filepath.Clean(filepath.Join(cwd, "..", ".."))
	if root != want {
		t.Fatalf("repo root = %q, want %q", root, want)
	}
}
