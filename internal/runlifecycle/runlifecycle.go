// Package runlifecycle owns pure decisions shared by review run lifecycles.
package runlifecycle

import (
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/gitprovider"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
)

// PremiseComparison describes how a stored run premise compares with the
// current pull request premise.
type PremiseComparison struct {
	StoredHeadSHA  string
	CurrentHeadSHA string
	StoredBaseSHA  string
	CurrentBaseSHA string
	Moved          bool
}

// NewestCompatibleIncompleteRun selects the newest resumable run for a base,
// post mode, and artifact marker kind.
func NewestCompatibleIncompleteRun(runs []ledger.Run, baseSHA string, postMode ledger.PostMode, markerKind string, markerMatches func(string, string, string) bool) (ledger.Run, bool) {
	var best ledger.Run
	found := false
	for _, run := range runs {
		if run.BaseSHA != baseSHA || run.PostMode != postMode {
			continue
		}
		if run.Outcome != nil && *run.Outcome != ledger.OutcomeIncomplete {
			continue
		}
		if strings.TrimSpace(run.ArtifactPath) == "" || !markerMatches(run.ArtifactPath, markerKind, run.RunID) {
			continue
		}
		if !found || run.Attempt > best.Attempt || (run.Attempt == best.Attempt && run.StartedAt.After(best.StartedAt)) {
			best = run
			found = true
		}
	}
	return best, found
}

// ComparePremises compares a stored run premise with current head and base
// SHAs and returns the data callers need to render their own message.
func ComparePremises(run ledger.Run, currentHeadSHA, currentBaseSHA string) PremiseComparison {
	return PremiseComparison{
		StoredHeadSHA:  run.SHA,
		CurrentHeadSHA: currentHeadSHA,
		StoredBaseSHA:  run.BaseSHA,
		CurrentBaseSHA: currentBaseSHA,
		Moved:          run.SHA != currentHeadSHA || run.BaseSHA != currentBaseSHA,
	}
}

// PostingKey returns the canonical durable key for a posting identity: login,
// falling back to ID.
func PostingKey(identity gitprovider.Identity) string {
	if strings.TrimSpace(identity.Login) != "" {
		return identity.Login
	}
	return identity.ID
}
