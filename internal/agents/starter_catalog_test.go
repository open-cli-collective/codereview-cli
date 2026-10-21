package agents

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

const starterCatalogDir = "../../starter-agents"

// starterTableRow matches a row of the agent table in starter-agents/README.md:
// | `category:name` | Runs | What it reviews |
var starterTableRow = regexp.MustCompile("^\\| `([a-z0-9-]+:[a-z0-9-]+)` \\| ([^|]+) \\|")

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
	for _, agent := range catalog.Agents {
		if strings.TrimSpace(agent.Prompt) == "" {
			t.Errorf("%s has an empty prompt", agent.ID)
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

	rows := readStarterTable(t)
	var loaded []string
	for _, agent := range catalog.Agents {
		loaded = append(loaded, agent.ID)
		runs, listed := rows[agent.ID]
		if !listed {
			t.Errorf("starter-agents/README.md agent table has no row for %s", agent.ID)
			continue
		}
		if documentedRequired := strings.HasPrefix(runs, "Every PR"); documentedRequired != agent.RequiredOnMatch {
			t.Errorf("README says %s runs %q, but required_on_match is %v", agent.ID, runs, agent.RequiredOnMatch)
		}
	}
	sort.Strings(loaded)
	for id := range rows {
		if _, ok := catalog.Find(id); !ok {
			t.Errorf("starter-agents/README.md lists %s, which the catalog does not load (loaded: %s)", id, strings.Join(loaded, ", "))
		}
	}
}

// readStarterTable returns the agent table rows of starter-agents/README.md,
// keyed by agent ID, with the "Runs" column as the value.
func readStarterTable(t *testing.T) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join(starterCatalogDir, "README.md"))
	if err != nil {
		t.Fatalf("open starter catalog README: %v", err)
	}
	defer f.Close()
	rows := map[string]string{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		match := starterTableRow.FindStringSubmatch(scanner.Text())
		if match == nil {
			continue
		}
		if _, dup := rows[match[1]]; dup {
			t.Errorf("starter-agents/README.md lists %s twice", match[1])
		}
		rows[match[1]] = strings.TrimSpace(match[2])
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read starter catalog README: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("starter-agents/README.md has no agent table rows")
	}
	return rows
}
