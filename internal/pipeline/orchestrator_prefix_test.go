package pipeline

import (
	"strings"
	"testing"
)

// TestOrchestratorStageNamesUsePrefix enforces the contract the headline model
// filter relies on: every orchestrator stage name starts with the shared prefix.
func TestOrchestratorStageNamesUsePrefix(t *testing.T) {
	for _, name := range []string{orchestratorSelectionStage, orchestratorRollupStage} {
		if !strings.HasPrefix(name, orchestratorWorkstreamPrefix) {
			t.Fatalf("%q must start with %q", name, orchestratorWorkstreamPrefix)
		}
	}
}
