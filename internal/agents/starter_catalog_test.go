package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const starterCatalogDir = "../../starter-agents"

// The starter catalog ships as data, so nothing else compiles or loads it in
// CI. Loading it here keeps every agent valid under the strict profile-source
// parser and keeps the README's agent table in step with what loads.
func TestStarterCatalogLoads(t *testing.T) {
	catalog, err := Load(context.Background(), LoadOptions{ProfileDirs: []string{starterCatalogDir}})
	if err != nil {
		t.Fatalf("Load(%s): %v", starterCatalogDir, err)
	}
	if len(catalog.Agents) == 0 {
		t.Fatal("starter catalog loaded no agents")
	}

	readme, err := os.ReadFile(filepath.Join(starterCatalogDir, "README.md"))
	if err != nil {
		t.Fatalf("read starter catalog README: %v", err)
	}
	for _, agent := range catalog.Agents {
		if strings.TrimSpace(agent.Prompt) == "" {
			t.Errorf("%s has an empty prompt", agent.ID)
		}
		if !strings.Contains(string(readme), "`"+agent.ID+"`") {
			t.Errorf("starter-agents/README.md does not list %s", agent.ID)
		}
	}

	for _, id := range []string{"general:correctness", "general:repo-conventions", "documentation:docs"} {
		agent, ok := catalog.Find(id)
		if !ok {
			t.Fatalf("starter catalog is missing baseline agent %s", id)
		}
		if !agent.RequiredOnMatch {
			t.Errorf("%s must set required_on_match so every matching PR gets it", id)
		}
	}
}
