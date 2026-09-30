package configcmd

import (
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

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
