package identityshadow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

func TestObserverZeroPairRunPublishesPrivateHashBoundArtifactsWithoutCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfigWithInferenceTimeout(t, dir, 3, 12345, "47s")
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000001")
	keyLookups := 0
	factory := &captureFactory{}
	warnings := &strings.Builder{}
	observer := New(Options{
		ConfigPath: configPath, Warnings: warnings,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) {
			keyLookups++
			return "mock-key", nil
		},
		SystemOne: identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
	})
	observer.Observe(context.Background(), emptySnapshot(), attemptDir)

	if keyLookups != 0 || factory.constructions != 0 || len(factory.requests) != 0 {
		t.Fatalf("zero-pair execution accessed credentials/provider: key=%d clients=%d requests=%d", keyLookups, factory.constructions, len(factory.requests))
	}
	if warnings.Len() != 0 {
		t.Fatalf("successful empty diagnostic warned: %s", warnings.String())
	}
	assertPrivateMode(t, attemptDir, 0o700)
	for _, name := range []string{sourceFileName, baselineFileName, comparisonFileName, receiptFileName, manifestFileName} {
		assertPrivateMode(t, filepath.Join(attemptDir, name), 0o600)
	}
	for _, name := range []string{"cohort", "prior_threads"} {
		taskDir := filepath.Join(attemptDir, name)
		assertPrivateMode(t, taskDir, 0o700)
		for _, file := range []string{"fixture.json", "replay.json", "receipts.json", "manifest.json"} {
			assertPrivateMode(t, filepath.Join(taskDir, file), 0o600)
		}
	}
	assertAggregateHashes(t, attemptDir)
	var receipt aggregateReceipt
	readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
	if receipt.Status != "complete" || receipt.ExpectedPairs != 0 || receipt.ProviderCalls != 0 || receipt.ComparisonStatus != "available" {
		t.Fatalf("empty diagnostic receipt = %#v", receipt)
	}
	if receipt.Configuration == nil {
		t.Fatal("aggregate receipt omitted the parsed standalone configuration")
	}
	if receipt.Configuration.Budget.MaxCalls != 3 || receipt.Configuration.Budget.MaxTotalRequestBytes != 12345 ||
		receipt.Configuration.TimeoutText != "10s" || receipt.Configuration.InferenceText != "47s" {
		t.Fatalf("aggregate configuration lost non-default limits/timeouts: %#v", receipt.Configuration)
	}
	canonicalConfig, err := encodeCanonical(*receipt.Configuration)
	if err != nil {
		t.Fatal(err)
	}
	if digest(canonicalConfig) != receipt.ConfigSHA256 {
		t.Fatalf("retained config digest %s does not match config_sha256 %s", digest(canonicalConfig), receipt.ConfigSHA256)
	}
	var comparison Comparison
	readJSON(t, filepath.Join(attemptDir, comparisonFileName), &comparison)
	if comparison.Cohort.ExpectedPairs != 0 || comparison.PriorExact.ExpectedPairs != 0 {
		t.Fatalf("empty comparison counts = %#v", comparison)
	}
}

func TestObserverFailedAttemptReservationDoesNotTouchExistingOrSymlinkTarget(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		symlink bool
	}{
		{name: "existing directory"},
		{name: "symlink target", symlink: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := writeObserverConfig(t, dir, 40, 327680)
			attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000010")
			parentDir := filepath.Dir(attemptDir)
			if err := os.MkdirAll(parentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			var protectedDir string
			if testCase.symlink {
				protectedDir = filepath.Join(dir, "protected")
				if err := os.Mkdir(protectedDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(protectedDir, "sentinel.txt"), []byte("must remain unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(protectedDir, attemptDir); err != nil {
					t.Fatal(err)
				}
			} else {
				protectedDir = attemptDir
				if err := os.Mkdir(attemptDir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(attemptDir, "sentinel.txt"), []byte("must remain unchanged"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			parentBefore := directoryEntries(t, parentDir)
			protectedBefore := directoryEntries(t, protectedDir)
			parentModeBefore := mustStat(t, parentDir).Mode().Perm()
			protectedSentinel := mustRead(t, filepath.Join(protectedDir, "sentinel.txt"))
			keyLookups := 0
			factory := &captureFactory{}
			warnings := &strings.Builder{}
			observer := New(Options{
				ConfigPath: configPath, Warnings: warnings,
				ResolveSystemOneKey: func(context.Context, string, string) (string, error) {
					keyLookups++
					return "mock-key", nil
				},
				SystemOne: identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
			})
			observer.Observe(context.Background(), comparisonSnapshot(), attemptDir)
			if keyLookups != 0 || factory.constructions != 0 || len(factory.requests) != 0 {
				t.Fatalf("reservation failure accessed credentials/provider: key=%d clients=%d requests=%d", keyLookups, factory.constructions, len(factory.requests))
			}
			if !strings.Contains(warnings.String(), "status=artifact_failure") {
				t.Fatalf("missing artifact failure warning: %q", warnings.String())
			}
			if !reflect.DeepEqual(parentBefore, directoryEntries(t, parentDir)) || mustStat(t, parentDir).Mode().Perm() != parentModeBefore {
				t.Fatalf("attempt parent changed after refusing existing destination")
			}
			if !reflect.DeepEqual(protectedBefore, directoryEntries(t, protectedDir)) || !reflect.DeepEqual(protectedSentinel, mustRead(t, filepath.Join(protectedDir, "sentinel.txt"))) {
				t.Fatalf("protected destination changed after refusal")
			}
			if testCase.symlink {
				linkTarget, err := os.Readlink(attemptDir)
				if err != nil || linkTarget != protectedDir {
					t.Fatalf("symlink changed after refusal: target=%q err=%v", linkTarget, err)
				}
			}
		})
	}
}

func TestObserverAggregateBudgetRefusalRetainsFullMissingUniverse(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		maxCalls int
		maxBytes int64
	}{
		{name: "call cap", maxCalls: 2, maxBytes: 327680},
		{name: "request byte cap", maxCalls: 40, maxBytes: 1},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := writeObserverConfig(t, dir, testCase.maxCalls, testCase.maxBytes)
			attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000002")
			keyLookups := 0
			factory := &captureFactory{}
			observer := New(Options{
				ConfigPath: configPath, Warnings: io.Discard,
				ResolveSystemOneKey: func(context.Context, string, string) (string, error) {
					keyLookups++
					return "mock-key", nil
				},
				SystemOne: identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
			})
			observer.Observe(context.Background(), comparisonSnapshot(), attemptDir)

			if keyLookups != 0 || factory.constructions != 0 || len(factory.requests) != 0 {
				t.Fatalf("budget refusal accessed credentials/provider: key=%d clients=%d requests=%d", keyLookups, factory.constructions, len(factory.requests))
			}
			var receipt aggregateReceipt
			readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
			if receipt.Status != "budget_exceeded" || receipt.ExpectedPairs != 20 || receipt.MissingPairs != 20 || receipt.ProviderCalls != 0 {
				t.Fatalf("budget receipt = %#v", receipt)
			}
			var comparison Comparison
			readJSON(t, filepath.Join(attemptDir, comparisonFileName), &comparison)
			for name, section := range map[string]ComparisonSection{"cohort": comparison.Cohort, "prior": comparison.PriorExact} {
				if section.ExpectedPairs != 10 || section.ShadowMissing != 10 || section.UnresolvedPairs != 10 || section.ComparablePairs != 0 ||
					section.ExpectedPairs != section.ComparablePairs+section.UnresolvedPairs {
					t.Fatalf("budget refusal did not retain the full %s comparison denominator: %#v", name, section)
				}
			}
			assertAggregateHashes(t, attemptDir)
		})
	}
}

func TestObserverPriorReservationFailurePreventsKeyAndClientCreation(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000003")
	keyLookups := 0
	factory := &captureFactory{}
	files := identityexperiment.FileDependencies{Mkdir: func(path string, mode os.FileMode) error {
		if filepath.Base(path) == "prior_threads" {
			return errors.New("injected reservation fault")
		}
		return os.Mkdir(path, mode)
	}}
	warnings := &strings.Builder{}
	observer := New(Options{
		ConfigPath: configPath, Warnings: warnings, Files: files,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) {
			keyLookups++
			return "mock-key", nil
		},
		SystemOne: identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
	})
	observer.Observe(context.Background(), comparisonSnapshot(), attemptDir)

	if keyLookups != 0 || factory.constructions != 0 || len(factory.requests) != 0 {
		t.Fatalf("reservation failure accessed credentials/provider: key=%d clients=%d requests=%d", keyLookups, factory.constructions, len(factory.requests))
	}
	if !strings.Contains(warnings.String(), "status=artifact_failure") || strings.Contains(warnings.String(), "injected reservation fault") {
		t.Fatalf("warning is not a sanitized artifact failure: %q", warnings.String())
	}
	if _, err := os.Stat(filepath.Join(attemptDir, "cohort")); err != nil {
		t.Fatalf("successful first task reservation was not retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(attemptDir, receiptFileName)); err != nil {
		t.Fatalf("partial receipt was not retained: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(attemptDir, manifestFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected aggregate manifest after incomplete reservation: %v", err)
	}
}

func TestObserverExecutesBothTasksBeforePublishingAndExcludesPublishTime(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000011")
	start := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	fakeNow := start
	events := []string{}
	factory := &captureFactory{events: &events, fakeNow: &fakeNow, perDecisionAdvance: 2 * time.Second}
	files := identityexperiment.FileDependencies{Rename: func(oldPath, newPath string) error {
		if isTaskPublishPath(newPath) {
			events = append(events, "publish:"+filepath.Base(newPath))
			fakeNow = fakeNow.Add(20 * time.Second)
		}
		return os.Rename(oldPath, newPath)
	}}
	observer := New(Options{
		ConfigPath: configPath, Warnings: io.Discard, Files: files,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) { return "mock-key", nil },
		SystemOne:           identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
		Now:                 func() time.Time { return fakeNow },
	})
	observer.Observe(context.Background(), comparisonSnapshot(), attemptDir)
	lastDecision, firstPublish := -1, len(events)
	for index, event := range events {
		if strings.HasPrefix(event, "decide:") {
			lastDecision = index
		}
		if strings.HasPrefix(event, "publish:") && firstPublish == len(events) {
			firstPublish = index
		}
	}
	if lastDecision < 0 || firstPublish == len(events) || lastDecision >= firstPublish {
		t.Fatalf("task publication began before both task executions finished: events=%v", events)
	}
	if len(factory.requests) != 20 || factory.constructions != 2 {
		t.Fatalf("executed requests=%d clients=%d, want 20/2", len(factory.requests), factory.constructions)
	}
	var receipt aggregateReceipt
	readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
	if receipt.InferenceDurationMS != 40000 {
		t.Fatalf("inference duration included publication time: got %d ms, want 40000 ms", receipt.InferenceDurationMS)
	}
	if receipt.DurationMS <= receipt.InferenceDurationMS {
		t.Fatalf("overall duration %d ms did not include publication delays after %d ms inference", receipt.DurationMS, receipt.InferenceDurationMS)
	}
}

func TestObserverInferenceDeadlineStartsAfterCredentialLookup(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfigWithInferenceTimeout(t, dir, 40, 327680, "47s")
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000015")
	factory := &captureFactory{}
	var lookupCompletedAt, firstDecideAt time.Time
	deadlines := make([]time.Time, 0, 20)
	newClient := func(options systemone.Options) (identityexperiment.SystemOneDecider, error) {
		inner, err := factory.newClient(options)
		if err != nil {
			return nil, err
		}
		return systemOneDeciderFunc(func(ctx context.Context, request systemone.Request) (systemone.Receipt, error) {
			if firstDecideAt.IsZero() {
				firstDecideAt = time.Now()
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Error("provider Decide context has no inference deadline")
			} else {
				deadlines = append(deadlines, deadline)
			}
			return inner.Decide(ctx, request)
		}), nil
	}
	observer := New(Options{
		ConfigPath: configPath, Warnings: io.Discard,
		ResolveSystemOneKey: func(ctx context.Context, _, _ string) (string, error) {
			if _, ok := ctx.Deadline(); ok {
				t.Error("credential resolver received a deadline instead of the original background context")
			}
			if err := ctx.Err(); err != nil {
				t.Errorf("credential resolver received canceled context: %v", err)
			}
			lookupCompletedAt = time.Now()
			return "mock-key", nil
		},
		SystemOne: identityexperiment.SystemOneDependencies{NewClient: newClient},
	})
	observer.Observe(context.Background(), comparisonSnapshot(), attemptDir)

	if factory.constructions != 2 || len(factory.requests) != 20 || len(deadlines) != 20 || lookupCompletedAt.IsZero() || firstDecideAt.IsZero() {
		t.Fatalf("deadline coverage: clients=%d requests=%d deadlines=%d lookup=%s first_decide=%s", factory.constructions, len(factory.requests), len(deadlines), lookupCompletedAt, firstDecideAt)
	}
	deadline := deadlines[0]
	if firstDecideAt.Before(lookupCompletedAt) || deadline.Before(lookupCompletedAt.Add(47*time.Second)) || deadline.After(firstDecideAt.Add(47*time.Second)) {
		t.Fatalf("inference deadline %s is not bounded by lookup completion %s + configured 47s and first Decide %s + configured 47s", deadline, lookupCompletedAt, firstDecideAt)
	}
	for index, observed := range deadlines {
		if !observed.Equal(deadline) {
			t.Fatalf("Decide %d deadline = %s, want shared inference deadline %s", index, observed, deadline)
		}
	}
}

func TestObserverCohortAbstainDoesNotSuppressPriorTask(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000012")
	factory := &captureFactory{abstainFirstCohort: true}
	observer := New(Options{
		ConfigPath: configPath, Warnings: io.Discard,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) { return "mock-key", nil },
		SystemOne:           identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
	})
	observer.Observe(context.Background(), comparisonSnapshot(), attemptDir)
	if len(factory.requests) != 20 || factory.constructions != 2 {
		t.Fatalf("unresolved cohort suppressed prior inference: requests=%d clients=%d", len(factory.requests), factory.constructions)
	}
	var receipt aggregateReceipt
	readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
	if receipt.Status != "unresolved" || receipt.Cohort.AbstainedPairs != 1 || receipt.PriorThreads.ProviderCalls != 10 {
		t.Fatalf("abstain receipt = %#v", receipt)
	}
}

func TestObserverAggregateStatusPreservesContradictoryReplay(t *testing.T) {
	snapshot := comparisonSnapshot()
	snapshot.Findings = append([]review.Finding{}, snapshot.Findings[:3]...)
	snapshot.PriorRoots = append([]reportidentitysnapshot.PriorRoot{}, snapshot.PriorRoots[:2]...)
	snapshot.Baseline.EffectiveOrderedIDs = []review.FindingID{"A", "B", "C"}
	snapshot.Baseline.Dedupe = []reportidentitysnapshot.Dedupe{}
	priorKeys := make([]reportidentitysnapshot.PriorKey, 0, len(snapshot.Baseline.PriorKeys))
	for _, key := range snapshot.Baseline.PriorKeys {
		if key.RecordKind == "finding" && key.RecordID != "A" && key.RecordID != "B" && key.RecordID != "C" {
			continue
		}
		priorKeys = append(priorKeys, key)
	}
	snapshot.Baseline.PriorKeys = priorKeys

	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000016")
	factory := &captureFactory{}
	taskCalls := map[string]int{}
	pairCalls := map[string]int{}
	mutatedContradictoryPair := 0
	newClient := func(options systemone.Options) (identityexperiment.SystemOneDecider, error) {
		inner, err := factory.newClient(options)
		if err != nil {
			return nil, err
		}
		return systemOneDeciderFunc(func(ctx context.Context, request systemone.Request) (systemone.Receipt, error) {
			var state struct {
				Task string `json:"task"`
				Left struct {
					Kind    string `json:"kind"`
					Finding *struct {
						ID string `json:"finding_id"`
					} `json:"finding"`
					Thread *struct {
						ID string `json:"thread_id"`
					} `json:"thread"`
				} `json:"left"`
				Right struct {
					Kind    string `json:"kind"`
					Finding *struct {
						ID string `json:"finding_id"`
					} `json:"finding"`
					Thread *struct {
						ID string `json:"thread_id"`
					} `json:"thread"`
				} `json:"right"`
			}
			if err := json.Unmarshal([]byte(request.State), &state); err != nil {
				t.Fatalf("Decode provider request state: %v", err)
			}
			leftID, rightID := "", ""
			if state.Left.Finding != nil {
				leftID = state.Left.Finding.ID
			}
			if state.Left.Thread != nil {
				leftID = state.Left.Thread.ID
			}
			if state.Right.Finding != nil {
				rightID = state.Right.Finding.ID
			}
			if state.Right.Thread != nil {
				rightID = state.Right.Thread.ID
			}
			if state.Task == "cohort" && (state.Left.Kind != "finding" || state.Right.Kind != "finding") {
				t.Fatalf("cohort request record kinds = %q/%q", state.Left.Kind, state.Right.Kind)
			}
			if state.Task == "prior_threads" && (state.Left.Kind != "finding" || state.Right.Kind != "thread") {
				t.Fatalf("prior request record kinds = %q/%q", state.Left.Kind, state.Right.Kind)
			}
			taskCalls[state.Task]++
			pairCalls[state.Task+"/"+leftID+"/"+rightID]++

			receipt, err := inner.Decide(ctx, request)
			if err != nil {
				return receipt, err
			}
			if state.Task == "cohort" && leftID == "A" && rightID == "C" {
				answer, ok := receipt.Answers["identity"]
				if !ok {
					t.Fatal("capture decider omitted the identity answer")
				}
				answer.Choice = "b"
				answer.Confidence = 0.9
				answer.Probabilities = map[string]float64{"a": 0.05, "b": 0.9, "c": 0.05}
				receipt.Answers["identity"] = answer
				mutatedContradictoryPair++
			}
			return receipt, nil
		}), nil
	}
	observer := New(Options{
		ConfigPath: configPath, Warnings: io.Discard,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) { return "mock-key", nil },
		SystemOne:           identityexperiment.SystemOneDependencies{NewClient: newClient},
	})
	observer.Observe(context.Background(), snapshot, attemptDir)

	if factory.constructions != 2 || len(factory.requests) != 9 || taskCalls["cohort"] != 3 || taskCalls["prior_threads"] != 6 || mutatedContradictoryPair != 1 || len(pairCalls) != 9 {
		t.Fatalf("contradiction run coverage: clients=%d requests=%d tasks=%v unique_pairs=%d mutated=%d", factory.constructions, len(factory.requests), taskCalls, len(pairCalls), mutatedContradictoryPair)
	}
	for pair, calls := range pairCalls {
		if calls != 1 {
			t.Fatalf("provider pair %q called %d times", pair, calls)
		}
	}

	var cohortReplay reportidentity.Result
	readJSON(t, filepath.Join(attemptDir, "cohort", "replay.json"), &cohortReplay)
	var cohortReceipts, priorReceipts identityexperiment.Receipts
	readJSON(t, filepath.Join(attemptDir, "cohort", "receipts.json"), &cohortReceipts)
	readJSON(t, filepath.Join(attemptDir, "prior_threads", "receipts.json"), &priorReceipts)
	if cohortReplay.Status != "unresolved" || len(cohortReplay.Findings) != 3 || cohortReceipts.Status != "unresolved" ||
		cohortReceipts.ExpectedPairs != 3 || cohortReceipts.ProviderCalls != 3 || cohortReceipts.CompletedPairs != 3 ||
		cohortReceipts.MissingPairs != 0 || cohortReceipts.AbstainedPairs != 0 {
		t.Fatalf("contradictory cohort replay/receipts = status:%s findings:%d receipts:%#v", cohortReplay.Status, len(cohortReplay.Findings), cohortReceipts)
	}
	if priorReceipts.Status != "complete" || priorReceipts.ExpectedPairs != 6 || priorReceipts.ProviderCalls != 6 ||
		priorReceipts.CompletedPairs != 6 || priorReceipts.MissingPairs != 0 || priorReceipts.AbstainedPairs != 0 {
		t.Fatalf("complete prior-thread receipts = %#v", priorReceipts)
	}

	var cohortFixture, priorFixture reportidentity.Input
	readJSON(t, filepath.Join(attemptDir, "cohort", "fixture.json"), &cohortFixture)
	readJSON(t, filepath.Join(attemptDir, "prior_threads", "fixture.json"), &priorFixture)
	if len(cohortFixture.Findings) != 3 || !reflect.DeepEqual(cohortFixture.Findings, snapshot.Findings) ||
		len(priorFixture.Findings) != 3 || !reflect.DeepEqual(priorFixture.Findings, snapshot.Findings) {
		t.Fatalf("task fixtures did not retain all original findings: cohort=%d prior=%d", len(cohortFixture.Findings), len(priorFixture.Findings))
	}
	var source sourceArtifact
	readJSON(t, filepath.Join(attemptDir, sourceFileName), &source)
	if len(source.Findings) != 3 || !reflect.DeepEqual(source.Findings, snapshot.Findings) {
		t.Fatalf("source artifact did not retain all original findings: %#v", source.Findings)
	}
	var aggregate aggregateReceipt
	readJSON(t, filepath.Join(attemptDir, receiptFileName), &aggregate)
	if aggregate.Status != "unresolved" || aggregate.Cohort.Status != "unresolved" || aggregate.PriorThreads.Status != "complete" ||
		aggregate.ExpectedPairs != 9 || aggregate.ProviderCalls != 9 || aggregate.CompletedPairs != 9 ||
		aggregate.MissingPairs != 0 || aggregate.AbstainedPairs != 0 {
		t.Fatalf("aggregate status/counts = %#v", aggregate)
	}
	var manifest aggregateManifest
	readJSON(t, filepath.Join(attemptDir, manifestFileName), &manifest)
	if manifest.Status != "unresolved" {
		t.Fatalf("aggregate manifest status = %q, want unresolved", manifest.Status)
	}
	assertAggregateHashes(t, attemptDir)
}

func TestObserverCohortErrorOrCancellationStopsPriorNetworkCalls(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		cancelCall bool
	}{
		{name: "model error"},
		{name: "cancellation", cancelCall: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := t.TempDir()
			configPath := writeObserverConfig(t, dir, 40, 327680)
			attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000013")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			factory := &captureFactory{}
			if testCase.cancelCall {
				factory.cancelAfterFirst = cancel
			} else {
				factory.failTask = "cohort"
			}
			observer := New(Options{
				ConfigPath: configPath, Warnings: io.Discard,
				ResolveSystemOneKey: func(context.Context, string, string) (string, error) { return "mock-key", nil },
				SystemOne:           identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
			})
			observer.Observe(ctx, comparisonSnapshot(), attemptDir)
			if len(factory.requests) != 1 || factory.constructions != 1 || requestTask([]byte(factory.requests[0].State)) != "cohort" {
				t.Fatalf("cohort %s did not stop before prior requests: clients=%d requests=%#v", testCase.name, factory.constructions, factory.requests)
			}
			var receipt aggregateReceipt
			readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
			if receipt.Cohort.ProviderCalls != 1 || receipt.PriorThreads.ProviderCalls != 0 || receipt.PriorThreads.MissingPairs != 10 {
				t.Fatalf("cohort %s task accounting = %#v", testCase.name, receipt)
			}
		})
	}
}

func TestObserverFirstTaskPublishFailureStillPreservesSecondResult(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000014")
	events := []string{}
	factory := &captureFactory{events: &events}
	files := identityexperiment.FileDependencies{Rename: func(oldPath, newPath string) error {
		if isTaskPublishPath(newPath) {
			events = append(events, "publish:"+newPath)
		}
		if strings.HasSuffix(newPath, filepath.Join("cohort", "fixture.json")) {
			return errors.New("injected cohort artifact failure")
		}
		return os.Rename(oldPath, newPath)
	}}
	warnings := &strings.Builder{}
	observer := New(Options{
		ConfigPath: configPath, Warnings: warnings, Files: files,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) { return "mock-key", nil },
		SystemOne:           identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
	})
	observer.Observe(context.Background(), comparisonSnapshot(), attemptDir)
	if len(factory.requests) != 20 || factory.constructions != 2 {
		t.Fatalf("first publisher failure lost an executed result: clients=%d requests=%d", factory.constructions, len(factory.requests))
	}
	var receipt aggregateReceipt
	readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
	if receipt.Status != "artifact_failure" || receipt.TaskReceiptsOnPublishFailure == nil {
		t.Fatalf("failed publication receipt = %#v", receipt)
	}
	retained := receipt.TaskReceiptsOnPublishFailure
	if retained.Cohort.ProviderCalls != 10 || retained.PriorThreads.ProviderCalls != 10 || len(retained.Cohort.Attempts) != 10 || len(retained.PriorThreads.Attempts) != 10 {
		t.Fatalf("aggregate fallback did not preserve both task receipts: cohort=%#v prior=%#v", retained.Cohort, retained.PriorThreads)
	}
	for _, taskReceipts := range []identityexperiment.Receipts{retained.Cohort, retained.PriorThreads} {
		for _, attempt := range taskReceipts.Attempts {
			if attempt.Answer == nil || attempt.Usage == nil {
				t.Fatalf("full task receipt lost answer or usage: %#v", attempt)
			}
		}
	}
	if _, err := os.Stat(filepath.Join(attemptDir, "prior_threads", "manifest.json")); err != nil {
		t.Fatalf("second task payload was not published after first task failure: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(attemptDir, manifestFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("misleading complete aggregate manifest exists: %v", err)
	}
	if !strings.Contains(warnings.String(), "status=artifact_failure") {
		t.Fatalf("missing sanitized artifact failure warning: %q", warnings.String())
	}
}

func TestObserverCancellationAfterSynchronousLookupSendsNoRequests(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000004")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	keyLookups := 0
	factory := &captureFactory{}
	observer := New(Options{
		ConfigPath: configPath, Warnings: io.Discard,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) {
			keyLookups++
			cancel()
			return "mock-key", nil
		},
		SystemOne: identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
	})
	observer.Observe(ctx, comparisonSnapshot(), attemptDir)
	if keyLookups != 1 || factory.constructions != 0 || len(factory.requests) != 0 {
		t.Fatalf("post-lookup cancellation did not stop before client: key=%d clients=%d requests=%d", keyLookups, factory.constructions, len(factory.requests))
	}
	var receipt aggregateReceipt
	readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
	if receipt.Status != "cancelled" || receipt.CredentialLookupDurationMS < 0 || receipt.ProviderCalls != 0 { //nolint:misspell // Stable diagnostic category is contract-defined.
		t.Fatalf("cancelled receipt = %#v", receipt) //nolint:misspell // Stable diagnostic category is contract-defined.
	}
}

func TestObserverCanceledAtEntryDoesNotResolveKeyOrCreateClient(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000005")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	keyLookups := 0
	factory := &captureFactory{}
	observer := New(Options{
		ConfigPath: configPath, Warnings: io.Discard,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) {
			keyLookups++
			return "mock-key", nil
		},
		SystemOne: identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
	})
	observer.Observe(ctx, comparisonSnapshot(), attemptDir)
	if keyLookups != 0 || factory.constructions != 0 || len(factory.requests) != 0 {
		t.Fatalf("entry cancellation accessed credentials/provider: key=%d clients=%d requests=%d", keyLookups, factory.constructions, len(factory.requests))
	}
	var receipt aggregateReceipt
	readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
	if receipt.Status != "cancelled" { //nolint:misspell // Stable diagnostic category is contract-defined.
		t.Fatalf("status = %q, want cancelled", receipt.Status) //nolint:misspell // Stable diagnostic category is contract-defined.
	}
}

func TestObserverBaselineOnlyMutationPreservesEveryRequestAndCallOrder(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	firstDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000006")
	secondDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000007")
	firstSnapshot := comparisonSnapshot()
	secondSnapshot := reportidentitysnapshot.Clone(firstSnapshot)
	changedHash := strings.Repeat("f", 64)
	secondSnapshot.Baseline.PriorKeys[0].KeySHA256 = &changedHash
	changedMetric := int64(123456)
	secondSnapshot.Baseline.Metrics.RunPlanningWallMS = &changedMetric
	firstFactory := &captureFactory{}
	secondFactory := &captureFactory{}
	firstObserver := New(Options{
		ConfigPath: configPath, Warnings: io.Discard,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) { return "mock-key", nil },
		SystemOne:           identityexperiment.SystemOneDependencies{NewClient: firstFactory.newClient},
	})
	secondObserver := New(Options{
		ConfigPath: configPath, Warnings: io.Discard,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) { return "mock-key", nil },
		SystemOne:           identityexperiment.SystemOneDependencies{NewClient: secondFactory.newClient},
	})
	firstObserver.Observe(context.Background(), firstSnapshot, firstDir)
	secondObserver.Observe(context.Background(), secondSnapshot, secondDir)
	if firstFactory.constructions != 2 || secondFactory.constructions != 2 || len(firstFactory.requests) != 20 || len(secondFactory.requests) != 20 {
		t.Fatalf("request totals = %d/%d constructions=%d/%d", len(firstFactory.requests), len(secondFactory.requests), firstFactory.constructions, secondFactory.constructions)
	}
	if !reflect.DeepEqual(firstFactory.requests, secondFactory.requests) {
		t.Fatalf("baseline-only mutation changed provider request bytes/order:\nfirst=%#v\nsecond=%#v", firstFactory.requests, secondFactory.requests)
	}
	for _, request := range firstFactory.requests {
		if strings.Contains(request.State, changedHash) || strings.Contains(request.State, "123456") || strings.Contains(request.State, "accepted_rollup") {
			t.Fatalf("baseline data reached provider state: %s", request.State)
		}
	}
	firstSource := mustRead(t, filepath.Join(firstDir, sourceFileName))
	secondSource := mustRead(t, filepath.Join(secondDir, sourceFileName))
	if !reflect.DeepEqual(firstSource, secondSource) {
		t.Fatal("baseline-only mutation changed source.json")
	}
	if reflect.DeepEqual(mustRead(t, filepath.Join(firstDir, baselineFileName)), mustRead(t, filepath.Join(secondDir, baselineFileName))) {
		t.Fatal("baseline-only mutation did not change baseline.json")
	}
	if reflect.DeepEqual(mustRead(t, filepath.Join(firstDir, comparisonFileName)), mustRead(t, filepath.Join(secondDir, comparisonFileName))) {
		t.Fatal("baseline-only mutation did not change comparison.json")
	}
	var firstReceipt, secondReceipt aggregateReceipt
	readJSON(t, filepath.Join(firstDir, receiptFileName), &firstReceipt)
	readJSON(t, filepath.Join(secondDir, receiptFileName), &secondReceipt)
	if firstReceipt.ConfigSHA256 != secondReceipt.ConfigSHA256 || firstReceipt.SnapshotSHA256 != secondReceipt.SnapshotSHA256 || firstReceipt.SourceSHA256 != secondReceipt.SourceSHA256 || firstReceipt.BaselineSHA256 == secondReceipt.BaselineSHA256 {
		t.Fatalf("receipt hashes did not bind the baseline separately: first=%#v second=%#v", firstReceipt, secondReceipt)
	}
	if firstReceipt.Cohort.ProviderCalls != 10 || firstReceipt.PriorThreads.ProviderCalls != 10 {
		t.Fatalf("task call counts = %#v", firstReceipt)
	}
}

func TestObserverUnknownUsageCostRemainsNullAndCountsUnknownCalls(t *testing.T) {
	dir := t.TempDir()
	configPath := writeObserverConfig(t, dir, 40, 327680)
	attemptDir := observerAttemptDir(t, dir, "00000000-0000-4000-8000-000000000008")
	snapshot := emptySnapshot()
	snapshot.Findings = []review.Finding{comparisonFinding("A"), comparisonFinding("B")}
	snapshot.Baseline.EffectiveOrderedIDs = []review.FindingID{"A", "B"}
	factory := &captureFactory{}
	factory.omitUsage = true
	observer := New(Options{
		ConfigPath: configPath, Warnings: io.Discard,
		ResolveSystemOneKey: func(context.Context, string, string) (string, error) { return "mock-key", nil },
		SystemOne:           identityexperiment.SystemOneDependencies{NewClient: factory.newClient},
	})
	observer.Observe(context.Background(), snapshot, attemptDir)
	var receipt aggregateReceipt
	readJSON(t, filepath.Join(attemptDir, receiptFileName), &receipt)
	if receipt.ProviderCalls != 1 || receipt.UnknownCostCalls != 1 || receipt.EstimatedCostUSD != nil {
		t.Fatalf("unknown usage/cost accounting = %#v", receipt)
	}
	var taskReceipts identityexperiment.Receipts
	readJSON(t, filepath.Join(attemptDir, "cohort", "receipts.json"), &taskReceipts)
	if taskReceipts.UnknownCostCalls != 1 || taskReceipts.EstimatedCostUSD != nil {
		t.Fatalf("task unknown usage/cost accounting = %#v", taskReceipts)
	}
}

type capturedRequest struct {
	Model         string
	ServiceTier   string
	State         string
	RequestSHA256 string
}

type captureFactory struct {
	requests           []capturedRequest
	constructions      int
	omitUsage          bool
	events             *[]string
	failTask           string
	cancelAfterFirst   context.CancelFunc
	abstainFirstCohort bool
	fakeNow            *time.Time
	perDecisionAdvance time.Duration
}

func (factory *captureFactory) newClient(options systemone.Options) (identityexperiment.SystemOneDecider, error) {
	factory.constructions++
	return captureDecider{factory: factory, limits: options.Limits}, nil
}

type captureDecider struct {
	factory *captureFactory
	limits  systemone.Limits
}

type systemOneDeciderFunc func(context.Context, systemone.Request) (systemone.Receipt, error)

func (decider systemOneDeciderFunc) Decide(ctx context.Context, request systemone.Request) (systemone.Receipt, error) {
	return decider(ctx, request)
}

func (decider captureDecider) Decide(ctx context.Context, request systemone.Request) (systemone.Receipt, error) {
	if err := ctx.Err(); err != nil {
		return systemone.Receipt{}, err
	}
	plan, err := systemone.Prepare(request, decider.limits)
	if err != nil {
		return systemone.Receipt{}, err
	}
	state := append([]byte(nil), request.State...)
	task := requestTask(state)
	decider.factory.requests = append(decider.factory.requests, capturedRequest{
		Model: request.Model, ServiceTier: request.ServiceTier,
		State: string(state), RequestSHA256: plan.RequestSHA256,
	})
	if decider.factory.events != nil {
		*decider.factory.events = append(*decider.factory.events, "decide:"+task)
	}
	responseHash := sha256.Sum256([]byte("mock response"))
	answer := systemone.ChoiceAnswer{Choice: "a", Confidence: 0.9, Probabilities: map[string]float64{"a": 0.9, "b": 0.05, "c": 0.05}}
	if decider.factory.abstainFirstCohort && task == "cohort" && len(decider.factory.requests) == 1 {
		answer = systemone.ChoiceAnswer{Choice: "c", Confidence: 0.9, Probabilities: map[string]float64{"a": 0.05, "b": 0.05, "c": 0.9}}
	}
	receipt := systemone.Receipt{
		RequestedModel: request.Model, ReturnedModel: request.ExpectedModel,
		RequestSHA256: plan.RequestSHA256, ResponseSHA256: hex.EncodeToString(responseHash[:]),
		RequestID: "mock-request", Answers: map[string]systemone.ChoiceAnswer{"identity": answer},
		Attempted: true, HTTPStatus: 200,
	}
	if !decider.factory.omitUsage {
		receipt.Usage = &systemone.Usage{InputTokens: 8, OutputTokens: 2}
	}
	if decider.factory.fakeNow != nil {
		*decider.factory.fakeNow = decider.factory.fakeNow.Add(decider.factory.perDecisionAdvance)
	}
	if decider.factory.cancelAfterFirst != nil && len(decider.factory.requests) == 1 {
		decider.factory.cancelAfterFirst()
		decider.factory.cancelAfterFirst = nil
	}
	if decider.factory.failTask == task {
		return receipt, errors.New("mock model failure")
	}
	return receipt, nil
}

func requestTask(state []byte) string {
	var request struct {
		Task string `json:"task"`
	}
	_ = json.Unmarshal(state, &request)
	return request.Task
}

func isTaskPublishPath(path string) bool {
	separator := string(os.PathSeparator)
	return strings.Contains(path, separator+"cohort"+separator) || strings.Contains(path, separator+"prior_threads"+separator)
}

func writeObserverConfig(t *testing.T, dir string, maxCalls int, maxBytes int64) string {
	return writeObserverConfigWithInferenceTimeout(t, dir, maxCalls, maxBytes, "30s")
}

func writeObserverConfigWithInferenceTimeout(t *testing.T, dir string, maxCalls int, maxBytes int64, inferenceTimeout string) string {
	t.Helper()
	content := strings.Replace(validConfigYAML, "max_calls: 40", "max_calls: "+strconvItoa(maxCalls), 1)
	content = strings.Replace(content, "max_total_request_bytes: 327680", "max_total_request_bytes: "+strconvItoa64(maxBytes), 1)
	content = strings.Replace(content, "inference_timeout: 30s", "inference_timeout: "+inferenceTimeout, 1)
	path := filepath.Join(dir, "shadow.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func observerAttemptDir(t *testing.T, dir, id string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "run"), 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "run", "identity-shadow", "attempt-"+id)
}

func emptySnapshot() reportidentitysnapshot.Snapshot {
	return reportidentitysnapshot.Snapshot{
		Source:   reportidentitysnapshot.Source{RunID: "run-zero", CRVersion: "test-version"},
		Findings: []review.Finding{}, PriorRoots: []reportidentitysnapshot.PriorRoot{},
		Baseline: reportidentitysnapshot.Baseline{SchemaVersion: 1, CohortStatus: "fallback_no_dedupe", EffectiveOrderedIDs: []review.FindingID{}, PriorRule: priorRuleV1, PriorKeys: []reportidentitysnapshot.PriorKey{}},
	}
}

func assertPrivateMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if got := info.Mode().Perm(); got != mode {
		t.Fatalf("mode(%s) = %04o, want %04o", path, got, mode)
	}
}

func mustStat(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	return info
}

func directoryEntries(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(path)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", path, err)
	}
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

func assertAggregateHashes(t *testing.T, dir string) {
	t.Helper()
	var manifest struct {
		Files         map[string]string `json:"files"`
		TaskManifests map[string]struct {
			SHA256   string                      `json:"sha256"`
			Manifest identityexperiment.Manifest `json:"manifest"`
		} `json:"task_manifests"`
	}
	readJSON(t, filepath.Join(dir, manifestFileName), &manifest)
	for name, expected := range manifest.Files {
		if actual := fileSHA256(t, filepath.Join(dir, name)); actual != expected {
			t.Fatalf("%s hash = %s, manifest says %s", name, actual, expected)
		}
	}
	for task, nested := range manifest.TaskManifests {
		manifestPath := filepath.Join(dir, task, "manifest.json")
		if fileSHA256(t, manifestPath) != nested.SHA256 {
			t.Fatalf("%s nested manifest hash mismatch", task)
		}
		for file, expected := range map[string]string{
			"fixture.json":  nested.Manifest.Files.FixtureSHA256,
			"replay.json":   nested.Manifest.Files.ReplaySHA256,
			"receipts.json": nested.Manifest.Files.ReceiptsSHA256,
		} {
			if actual := fileSHA256(t, filepath.Join(dir, task, file)); actual != expected {
				t.Fatalf("%s/%s hash = %s, manifest says %s", task, file, actual, expected)
			}
		}
	}
}

func fileSHA256(t *testing.T, path string) string {
	t.Helper()
	hash := sha256.Sum256(mustRead(t, path))
	return hex.EncodeToString(hash[:])
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	// #nosec G304 -- tests read only their own temporary observer artifacts.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return data
}

func readJSON(t *testing.T, path string, target any) {
	t.Helper()
	if err := json.Unmarshal(mustRead(t, path), target); err != nil {
		t.Fatalf("Unmarshal(%s): %v", path, err)
	}
}

func strconvItoa(value int) string { return strconv.FormatInt(int64(value), 10) }

func strconvItoa64(value int64) string { return strconv.FormatInt(value, 10) }
