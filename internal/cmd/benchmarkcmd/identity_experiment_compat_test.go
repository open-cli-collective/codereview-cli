package benchmarkcmd

import (
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
	"github.com/open-cli-collective/codereview-cli/internal/review"
)

func inferenceFindingRecord(finding review.Finding) reportidentityinference.Record {
	return reportidentityinference.Record{
		Kind: "finding",
		Finding: &reportidentityinference.Finding{
			ID: string(finding.ID), Severity: string(finding.Severity), FilePath: finding.FilePath,
			Anchor: reportidentityinference.Anchor{Kind: string(finding.Anchor.Kind), Side: string(finding.Anchor.Side), Line: finding.Anchor.Line},
			Body:   finding.Body, Anchoring: string(finding.Anchoring),
		},
	}
}

func inferenceThreadRecord(thread reportidentity.Thread) reportidentityinference.Record {
	return reportidentityinference.Record{
		Kind: "thread",
		Thread: &reportidentityinference.Thread{
			ID: thread.ID, Eligible: thread.Eligible, FilePath: thread.FilePath, Body: thread.Body,
		},
	}
}
