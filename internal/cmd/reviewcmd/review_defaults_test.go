package reviewcmd

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/open-cli-collective/codereview-cli/internal/app"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/cmdtest"
	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestReviewAutomaticallyUpgradesCodexBeforeRuntimeAndOnlyOnce(t *testing.T) {
	for _, flags := range [][]string{nil, {"--dry-run"}, {"--no-post"}, {"--retry-posts"}} {
		t.Run(strings.Join(flags, " "), func(t *testing.T) {
			cfg := config.Normalize(testConfig())
			home := cfg.Profiles["home"]
			cfg.LLMRuntimes[home.LLMRuntime] = config.LLMConfig{
				Provider: config.LLMProviderOpenAI, Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterCodexCLI,
				ModelMap: config.ModelMap{"small": "old-small", "medium": "old-medium"}, MaxEffort: config.EffortMap{"small": "low"},
			}
			cfg.Profiles["shared"] = home
			path := filepath.Join(t.TempDir(), "config.yml")
			if err := config.Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			// #nosec G304 -- config and backup paths are controlled by t.TempDir.
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			wantUpgrade := len(flags) == 0
			for attempt := 0; attempt < 2; attempt++ {
				var out, errOut *bytes.Buffer
				opts := &root.Options{ConfigPath: path, Quiet: true}
				called := false
				stop := errors.New("stop before review/network activity")
				var cmd *cobra.Command
				cmd, out, errOut = cmdtest.New(opts, func(cmd *cobra.Command, opts *root.Options) {
					RegisterWithFactory(cmd, opts, func(_ context.Context, req app.OpenRequest) (app.Runtime, error) {
						called = true
						if wantUpgrade {
							wantEffort := "max"
							if attempt == 1 {
								wantEffort = "high"
							}
							if req.Profile.LLM.EffortMap["small"] != wantEffort || req.Profile.LLM.ModelMap["medium"] != "gpt-6.1-sol" {
								t.Fatalf("runtime opened with stale settings: %#v", req.Profile.LLM)
							}
							if attempt == 0 && !strings.Contains(errOut.String(), "Updated Codex review settings") {
								t.Fatal("runtime opened before upgrade notice")
							}
						}
						return app.Runtime{}, stop
					})
				})
				args := append([]string{"review", "https://github.com/open-cli-collective/codereview-cli/pull/29"}, flags...)
				if err := root.Execute(cmd, args); !errors.Is(err, stop) || !called {
					t.Fatalf("Execute: %v, runtime called=%v", err, called)
				}
				noticed := strings.Contains(errOut.String(), "Updated Codex review settings")
				if noticed != (wantUpgrade && attempt == 0) || strings.Contains(out.String(), "Updated Codex") {
					t.Fatalf("notice count/output: stdout=%q stderr=%q", out.String(), errOut.String())
				}
				loaded, err := config.Load(path)
				if err != nil {
					t.Fatal(err)
				}
				if wantUpgrade {
					if loaded.Profiles["shared"].LLM.DefaultsVersion != config.CurrentReviewDefaultsVersion {
						t.Fatal("shared runtime upgrade was not saved")
					}
					// #nosec G304 -- config and backup paths are controlled by t.TempDir.
					backup, err := os.ReadFile(path + ".before-review-defaults-v1")
					if err != nil || !bytes.Equal(backup, before) {
						t.Fatalf("original config backup: %v", err)
					}
					llm := loaded.LLMRuntimes[home.LLMRuntime]
					llm.EffortMap["small"] = "high"
					loaded.LLMRuntimes[home.LLMRuntime] = llm
					if err := config.Save(path, loaded); err != nil {
						t.Fatal(err)
					}
				} else {
					// #nosec G304 -- config and backup paths are controlled by t.TempDir.
					after, err := os.ReadFile(path)
					if err != nil || !bytes.Equal(before, after) {
						t.Fatal("dry-run or recovery changed config")
					}
				}
			}
		})
	}
}

func TestConcurrentReviewUpgradesDoNotLoseRuntimeUpdates(t *testing.T) {
	cfg := config.Normalize(testConfig())
	cfg.LLMRuntimes["first"] = config.LLMConfig{Provider: config.LLMProviderOpenAI,
		Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterCodexCLI}
	cfg.LLMRuntimes["second"] = cfg.LLMRuntimes["first"]
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 4)
	changes := make(chan bool, 4)
	for _, name := range []string{"first", "second", "first", "second"} {
		go func() {
			_, changed, err := upgradeReviewDefaults(context.Background(), path, name)
			changes <- changed
			errs <- err
		}()
	}
	count := 0
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		if <-changes {
			count++
		}
	}
	loaded, err := config.Load(path)
	if err != nil || count != 2 || loaded.LLMRuntimes["first"].DefaultsVersion != 1 || loaded.LLMRuntimes["second"].DefaultsVersion != 1 {
		t.Fatalf("concurrent upgrade: %v, changes=%d, runtimes=%#v", err, count, loaded.LLMRuntimes)
	}
}

func TestFailedDefaultsSavePreservesOriginalConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, testConfig()); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- path is controlled by t.TempDir.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := saveReviewDefaults(path, config.File{}); err == nil {
		t.Fatal("invalid config save succeeded")
	}
	// #nosec G304 -- path is controlled by t.TempDir.
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed upgrade changed the original config")
	}
}

func TestMigrationAndOrdinaryConfigEditsRejectStaleDrafts(t *testing.T) {
	cfg := config.Normalize(testConfig())
	cfg.LLMRuntimes["codex"] = config.LLMConfig{Provider: config.LLMProviderOpenAI,
		Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterCodexCLI}
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := config.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	staleEdit, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := upgradeReviewDefaults(context.Background(), path, "codex"); err != nil {
		t.Fatal(err)
	}
	staleEdit.Data.KeepWorkbench = true
	if err := config.Save(path, staleEdit); !errors.Is(err, config.ErrChanged) {
		t.Fatalf("stale edit must not undo migration: %v", err)
	}
	latest, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	staleMigration := latest
	latest.Data.KeepWorkbench = true
	if err := config.Save(path, latest); err != nil {
		t.Fatal(err)
	}
	if err := saveReviewDefaults(path, staleMigration); !errors.Is(err, config.ErrChanged) {
		t.Fatalf("stale migration must not erase ordinary edit: %v", err)
	}
	final, err := config.Load(path)
	if err != nil || !final.Data.KeepWorkbench || final.LLMRuntimes["codex"].DefaultsVersion != 1 {
		t.Fatalf("migration and ordinary edit not preserved: %#v, %v", final, err)
	}
}
