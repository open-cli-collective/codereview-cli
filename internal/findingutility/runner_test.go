package findingutility

import (
	"context"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
)

func TestRunAdvisoryDisabledDoesNoWorkOrIOTouch(t *testing.T) {
	calledFactory := false
	calledResolver := false
	calledNow := false
	calledID := false
	options := Options{
		NewAdapter:   func(Invocation) (llm.Adapter, error) { calledFactory = true; return nil, nil },
		ResolveModel: func(string) (string, error) { calledResolver = true; return "", nil },
		Now:          func() time.Time { calledNow = true; return time.Time{} },
		NewAttemptID: func() string { calledID = true; return "id" },
	}
	outcome := RunAdvisory(context.Background(), options, testSnapshot())
	if outcome.AuditStatus != "" || len(outcome.Records) != 0 || len(outcome.Warnings) != 0 || outcome.AuditPath != "" {
		t.Fatalf("disabled outcome = %#v", outcome)
	}
	if calledFactory || calledResolver || calledNow || calledID {
		t.Fatal("disabled utility consumed an injected dependency")
	}
}

func TestRunAdvisoryUnconfiguredFixtureRetainsEveryFinding(t *testing.T) {
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	calledFactory := false
	calledResolver := false
	warnings := 0
	profile.BoundProfile.Digest = ""
	outcome := RunAdvisory(context.Background(), Options{
		Profile: profile,
		NewAdapter: func(Invocation) (llm.Adapter, error) {
			calledFactory = true
			return nil, nil
		},
		ResolveModel: func(string) (string, error) {
			calledResolver = true
			return "fixture:utility-v1", nil
		},
		Warn: func(Warning) { warnings++ },
	}, testSnapshot())
	if outcome.AuditStatus != AuditStatusDegraded || len(outcome.Records) != 2 || len(outcome.Warnings) != 1 {
		t.Fatalf("unconfigured outcome = %#v", outcome)
	}
	for _, record := range outcome.Records {
		if record.EffectiveDecision != DispositionKeep || record.ProposedDecision != DispositionKeep || record.EvaluatorStatus != EvaluatorSkippedConfig {
			t.Fatalf("unconfigured record = %#v", record)
		}
	}
	if calledFactory || calledResolver {
		t.Fatal("unconfigured utility invoked future adapter/model dependencies")
	}
	if warnings != 1 {
		t.Fatalf("warning callback count = %d, want 1", warnings)
	}
}

func TestRunAdvisoryInvalidInputRetainsRawFindings(t *testing.T) {
	profile, err := LoadFixtureProfile()
	if err != nil {
		t.Fatal(err)
	}
	snapshot := testSnapshot()
	snapshot.PR.Ref.Number = 0
	outcome := RunAdvisory(context.Background(), Options{Profile: profile}, snapshot)
	if len(outcome.Records) != len(snapshot.Findings) || outcome.AuditStatus != AuditStatusDegraded {
		t.Fatalf("invalid input outcome = %#v", outcome)
	}
	for _, record := range outcome.Records {
		if record.EffectiveDecision != DispositionKeep || !hasRecordReason(record, ReasonInvalidState) {
			t.Fatalf("invalid input record = %#v", record)
		}
	}
}
