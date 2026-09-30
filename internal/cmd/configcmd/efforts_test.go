package configcmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

func TestEffortCommandCannotUndoConcurrentMigration(t *testing.T) {
	cfg := config.Normalize(testConfig())
	runtimeName := cfg.Profiles["home"].LLMRuntime
	cfg.LLMRuntimes[runtimeName] = config.LLMConfig{Provider: config.LLMProviderOpenAI,
		Auth: config.LLMAuthSubscription, Adapter: config.LLMAdapterCodexCLI}
	path := saveTestConfig(t, cfg)
	previousSave := saveConfigFile
	t.Cleanup(func() { saveConfigFile = previousSave })
	saveConfigFile = func(path string, draft config.File) error {
		// Interleave migration after the command loads its draft, before its save.
		latest, err := config.Load(path)
		if err != nil {
			return err
		}
		upgraded, _ := config.UpgradeReviewDefaults(latest, runtimeName)
		if err := config.Save(path, upgraded); err != nil {
			return err
		}
		return config.Save(path, draft)
	}
	cmd, _ := newTestCommand(path)
	err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "efforts", "set", "small", "high"})
	if !errors.Is(err, config.ErrChanged) {
		t.Fatalf("stale command must fail safely: %v", err)
	}
	saveConfigFile = previousSave
	cmd, _ = newTestCommand(path)
	if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "efforts", "set", "small", "high"}); err != nil {
		t.Fatal(err)
	}
	latest, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	llm := latest.LLMRuntimes[runtimeName]
	if llm.DefaultsVersion != 1 || llm.ModelMap["medium"] != "gpt-6.1-sol" || llm.EffortMap["small"] != "high" {
		t.Fatalf("retry must preserve migration and customization: %#v", llm)
	}
}

func TestEffortCommandsPreserveModelsAndSharedRuntime(t *testing.T) {
	cfg := config.Normalize(testConfig())
	home := cfg.Profiles["home"]
	llm := config.LLMConfig{Provider: config.LLMProviderOpenAI, Auth: config.LLMAuthSubscription,
		Adapter: config.LLMAdapterCodexCLI, ModelMap: config.ModelMap{"small": "gpt-6-luna"},
		MaxEffort: config.EffortMap{"large": "medium"}}
	cfg.LLMRuntimes[home.LLMRuntime] = llm
	home.LLM = llm
	cfg.Profiles["home"] = home
	cfg.Profiles["shared"] = home
	path := saveTestConfig(t, cfg)
	for _, action := range []string{"set", "unset"} {
		args := []string{"--profile", "home", "config", "llm", "efforts", action, "small"}
		if action == "set" {
			args = append(args, "max")
		}
		cmd, _ := newTestCommand(path)
		if err := root.Execute(cmd, args); err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		loaded, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		want := ""
		if action == "set" {
			want = "max"
		}
		for _, name := range []string{"home", "shared"} {
			got := loaded.Profiles[name].LLM
			if got.EffortMap["small"] != want || got.ModelMap["small"] != "gpt-6-luna" || got.MaxEffort["large"] != "medium" {
				t.Fatalf("%s after %s: %#v", name, action, got)
			}
		}
		cmd, out := newTestCommand(path)
		if err := root.Execute(cmd, []string{"--profile", "home", "config", "llm", "efforts", "list", "--json"}); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `"effort_map"`) || !strings.Contains(out.String(), `"max_effort"`) {
			t.Fatalf("list: %s", out)
		}
	}
	for _, args := range [][]string{{"small", "ultra"}, {"huge", "max"}, {"small", ""}} {
		cmd, _ := newTestCommand(path)
		if err := root.Execute(cmd, append([]string{"--profile", "home", "config", "llm", "efforts", "set"}, args...)); err == nil {
			t.Fatalf("accepted invalid setting: %v", args)
		}
	}
}
