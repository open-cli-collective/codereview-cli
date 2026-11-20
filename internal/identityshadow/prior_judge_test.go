package identityshadow

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/openaidecisions"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/review"
	"github.com/open-cli-collective/codereview-cli/internal/reviewplan"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

func TestPriorJudgeProviderWiring(t *testing.T) {
	const systemOneResponse = `{"model":"decision-models/mammoth-test-2026-11","answers":{"identity":{"type":"choice","choice":"a","confidence":0.95,"probabilities":{"a":0.95,"b":0.04,"c":0.01}}},"usage":{"input_tokens":120,"output_tokens":3}}`
	const decisionsResponse = `{"model":"decision-models/mammoth-test-2026-11","answers":[{"type":"choice","name":"identity","choice":"a","confidence":0.95,"probabilities":[{"value":"a","probability":0.95},{"value":"b","probability":0.04},{"value":"c","probability":0.01}]}],"usage":{"input_tokens":120,"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},"output_tokens":3,"total_tokens":123}}`
	const key = "synthetic-judge-key"
	decisionsWithoutCacheUsage := strings.Replace(decisionsResponse, `"input_tokens_details":{"cached_tokens":0,"cache_write_tokens":0},`, "", 1)
	for _, test := range []struct {
		name, kind, scheme, auth, response string
		statusCode                         int
		costUnknown                        bool
	}{
		{"Jev", "system_one", "bearer", "Bearer " + key, systemOneResponse, http.StatusOK, false},
		{"Mammoth", "system_one", "api-key", "Api-Key " + key, systemOneResponse, http.StatusOK, false},
		{"Decisions", "openai_decisions", "", "Bearer " + key, decisionsResponse, http.StatusOK, false},
		{"Decisions unknown cost", "openai_decisions", "", "Bearer " + key, decisionsWithoutCacheUsage, http.StatusOK, true},
		{"Jev rate limited", "system_one", "bearer", "Bearer " + key, key, http.StatusTooManyRequests, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != test.auth {
					t.Error("judge did not use expected POST authentication")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				body, err := io.ReadAll(r.Body)
				if err != nil || !strings.Contains(string(body), "scheduler") || strings.Contains(string(body), "pagination") || strings.Contains(string(body), "identity-baseten") {
					t.Error("unexpected reports or credential reference sent to inference")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.statusCode)
				_, _ = io.WriteString(w, test.response)
			}))
			defer server.Close()
			dir := t.TempDir()
			config := strings.Replace(validConfigYAML, "https://inference.example.test/v1/systemone", server.URL, 1)
			config = strings.Replace(config, "kind: system_one", "kind: "+test.kind, 1)
			authLine := ""
			if test.scheme != "" {
				authLine = "  auth_scheme: " + test.scheme + "\n"
			}
			config = strings.Replace(config, "  auth_scheme: api-key\n", authLine, 1)
			config += "prices: {input_per_million: 1.0, output_per_million: 2.0}\n"
			output := filepath.Join(dir, "output")
			input := judgeFixture(t)
			report, err := RunPriorJudge(t.Context(), JudgeOptions{
				ConfigPath: writeJudgeTestConfig(t, dir, config),
				ExactKey:   reviewplan.PriorFindingIdentityKey,
				ResolveKey: func(_ context.Context, kind string, ref Credential) (string, error) {
					if kind != test.kind || ref.Store != "local-os" || ref.Name != "codereview/identity-baseten" {
						t.Errorf("unexpected credential resolution: %q %#v", kind, ref)
					}
					return key, nil
				},
				SystemOne: identityexperiment.SystemOneDependencies{NewClient: func(o systemone.Options) (identityexperiment.SystemOneDecider, error) {
					o.HTTPClient = server.Client()
					return systemone.New(o)
				}},
				NewDecisionsClient: func(o openaidecisions.Options) (identityexperiment.OpenAIDecisionsDecider, error) {
					o.HTTPClient = server.Client()
					return openaidecisions.New(o)
				},
			}, input, output)
			if err != nil || report.ExactMatches != 1 || report.SelectedPairs != 1 ||
				report.Receipts.ExpectedPairs != 6 || report.Receipts.ProviderCalls != 1 {
				t.Fatalf("unexpected judge report: %+v, %v", report, err)
			}
			if test.costUnknown && (report.Receipts.EstimatedCostUSD != nil || report.Receipts.UnknownCostCalls != 1) {
				t.Fatalf("expected unknown call cost: %+v", report.Receipts)
			}
			if !test.costUnknown && (report.Receipts.EstimatedCostUSD == nil || math.Abs(*report.Receipts.EstimatedCostUSD-0.000126) > 1e-12) {
				t.Fatalf("unexpected known call cost: %+v", report.Receipts)
			}
			if test.statusCode == http.StatusOK {
				if report.Status != "complete" || report.Receipts.KnownInputTokens != 120 ||
					len(report.Suggestions) != 1 || report.Suggestions[0] != (Suggestion{FindingID: "second", ThreadID: "prior", Confidence: .95}) {
					t.Fatalf("unexpected successful report: %+v", report)
				}
			} else if report.Status != "unavailable" || len(report.Suggestions) != 0 || report.Receipts.FailedPairs != 1 {
				t.Fatalf("unexpected failed report: %+v", report)
			}
			for _, filename := range []string{"input.json", "suggestions.json", "receipts.json", "replay.json", "manifest.json"} {
				path := filepath.Join(output, filename)
				info, statErr := os.Stat(path)
				if statErr != nil || info.Mode().Perm() != 0o600 {
					t.Fatalf("non-private or missing artifact %s: %v", filename, statErr)
				}
				// #nosec G304 -- test-owned output directory.
				data, readErr := os.ReadFile(path)
				if readErr != nil || strings.Contains(string(data), key) {
					t.Fatalf("unreadable artifact or key leaked in %s: %v", filename, readErr)
				}
			}
		})
	}
}

func TestPriorJudgeExceedsBudgetBeforeCredentialLookup(t *testing.T) {
	dir := t.TempDir()
	config := strings.Replace(validConfigYAML, "max_calls: 40", "max_calls: 0", 1)
	report, err := RunPriorJudge(t.Context(), JudgeOptions{
		ConfigPath: writeJudgeTestConfig(t, dir, config),
		ExactKey:   reviewplan.PriorFindingIdentityKey,
		ResolveKey: func(context.Context, string, Credential) (string, error) {
			t.Fatal("judge requested a credential without call budget")
			return "", nil
		},
	}, judgeFixture(t), filepath.Join(dir, "output"))
	if err != nil || report.Status != "unavailable" || len(report.Suggestions) != 0 ||
		report.SelectedPairs != 1 || report.Receipts.ProviderCalls != 0 || len(report.Receipts.PreflightErrors) != 1 ||
		report.Receipts.PreflightErrors[0].ErrorCode != "budget_exceeded" {
		t.Fatalf("unexpected budget response: %+v, %v", report, err)
	}
}

func TestPriorJudgeWithholdsAmbiguousSuggestion(t *testing.T) {
	for _, test := range []struct {
		choice     reportidentity.Choice
		confidence float64
		status     string
		count      int
	}{
		{reportidentity.ChoiceSame, .98, "partial", 0},
		{reportidentity.ChoiceSame, .75, "partial", 0},
		{reportidentity.ChoiceDifferent, .75, "partial", 0},
		{reportidentity.ChoiceAbstain, 1, "partial", 0},
		{reportidentity.ChoiceDifferent, .85, "complete", 1},
	} {
		t.Run(fmt.Sprintf("%s-%.2f", test.choice, test.confidence), func(t *testing.T) {
			suggestions, status := collectSuggestions([]identityexperiment.Attempt{
				{LeftID: "finding", RightID: "one", Answer: &identityexperiment.Answer{Choice: reportidentity.ChoiceSame, Confidence: .95}},
				{LeftID: "finding", RightID: "two", Answer: &identityexperiment.Answer{Choice: test.choice, Confidence: test.confidence}},
			}, .90, map[string]string{"one": "native-thread-one", "two": "native-thread-two"})
			if status != test.status || len(suggestions) != test.count || (test.count == 1 && suggestions[0].ThreadID != "native-thread-one") {
				t.Fatalf("unexpected suggestion outcome: %s %+v", status, suggestions)
			}
		})
	}
}

func writeJudgeTestConfig(t *testing.T, dir, contents string) string {
	t.Helper()
	path := filepath.Join(dir, "judge.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func judgeFixture(t *testing.T) []byte {
	t.Helper()
	first, second := comparisonFinding("first"), comparisonFinding("second")
	first.Body = "Preserve the pagination cursor."
	second.Body = "The scheduler repeats delivery attempts after the lease expires."
	data, err := json.Marshal(reportidentity.Input{
		SchemaVersion: reportidentity.SchemaVersion,
		Task:          reportidentity.TaskPriorThread,
		Findings:      []review.Finding{first, second},
		Threads: []reportidentity.Thread{
			{ID: "exact", Eligible: true, FilePath: first.FilePath, Body: first.Body},
			{ID: "prior", Eligible: true, FilePath: "src/worker.go", Body: "The lease expires, then the scheduler repeats delivery attempts."},
			{ID: "unrelated", Eligible: true, FilePath: "src/math.go", Body: "Round fractional cents consistently."},
			{ID: "excluded", Eligible: false, FilePath: second.FilePath, Body: second.Body},
		},
		Decisions: []reportidentity.Decision{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
