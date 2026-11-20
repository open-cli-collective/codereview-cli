package identityshadow

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentity"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentitysnapshot"
)

// JudgeOptions configures the same prior-thread judge for captured and saved inputs.
type JudgeOptions struct {
	ConfigPath         string
	ResolveKey         func(context.Context, string, Credential) (string, error)
	SystemOne          identityexperiment.SystemOneDependencies
	NewDecisionsClient identityexperiment.OpenAIDecisionsClientFactory
	ExactKey           func(string, string) (string, bool)
	Warnings           io.Writer
}

// Suggestion points at a possible match and carries no posting instructions.
type Suggestion struct {
	FindingID  string  `json:"finding_id"`
	ThreadID   string  `json:"thread_id"`
	Confidence float64 `json:"confidence"`
}

// JudgeReport contains advisory suggestions and bounded accounting.
type JudgeReport struct {
	SchemaVersion int                         `json:"schema_version"`
	InputSHA256   string                      `json:"input_sha256"`
	Status        string                      `json:"status"`
	ExactMatches  int                         `json:"exact_matches"`
	SelectedPairs int                         `json:"selected_pairs"`
	ElapsedMS     int64                       `json:"elapsed_ms"`
	Receipts      identityexperiment.Receipts `json:"receipts"`
	Suggestions   []Suggestion                `json:"suggestions"`
}

// MaxJudgeInputBytes allows a replay fixture and the captured planner keys.
const MaxJudgeInputBytes = reportidentity.MaxFixtureBytes + 64*1024

type capturedJudgeInput struct {
	SchemaVersion   int               `json:"schema_version"`
	Fixture         json.RawMessage   `json:"fixture"`
	PlannerKeys     map[string]string `json:"planner_keys"`
	OriginalThreads map[string]string `json:"original_threads"`
}

func decodeJudgeInput(data []byte) (reportidentity.Input, capturedJudgeInput, error) {
	if len(data) > MaxJudgeInputBytes {
		return reportidentity.Input{}, capturedJudgeInput{}, errors.New("judge: input exceeds maximum size")
	}
	fixture, err := reportidentity.Decode(data)
	if err == nil {
		return fixture, capturedJudgeInput{Fixture: data}, nil
	}
	var captured capturedJudgeInput
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&captured); err != nil || captured.SchemaVersion != 1 || captured.PlannerKeys == nil || captured.OriginalThreads == nil {
		return reportidentity.Input{}, capturedJudgeInput{}, errors.New("judge: invalid captured fixture")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return reportidentity.Input{}, capturedJudgeInput{}, errors.New("judge: captured fixture must contain one JSON object")
	}
	fixture, err = reportidentity.Decode(captured.Fixture)
	if err != nil {
		return fixture, captured, err
	}
	if len(captured.OriginalThreads) != len(fixture.Threads) {
		return fixture, captured, errors.New("judge: invalid original thread mapping")
	}
	knownIDs := make(map[string]bool, len(fixture.Findings)+len(fixture.Threads))
	for _, thread := range fixture.Threads {
		original := captured.OriginalThreads[thread.ID]
		if original == "" || deterministicThreadID(original) != thread.ID {
			return fixture, captured, errors.New("judge: invalid original thread mapping")
		}
		knownIDs[thread.ID] = true
	}
	for _, finding := range fixture.Findings {
		knownIDs[string(finding.ID)] = true
	}
	for id, key := range captured.PlannerKeys {
		decoded, err := hex.DecodeString(key)
		if !knownIDs[id] || err != nil || len(decoded) != 32 {
			return fixture, captured, errors.New("judge: invalid captured planner key")
		}
	}
	return fixture, captured, nil
}

// RunPriorJudge judges a retained prior_threads input. It has no access to Git.
func RunPriorJudge(ctx context.Context, options JudgeOptions, input []byte, outputDir string) (JudgeReport, error) {
	started := time.Now()
	if ctx == nil {
		ctx = context.Background()
	}
	configBytes, err := readConfigBytes(options.ConfigPath)
	if err != nil {
		return JudgeReport{}, err
	}
	cfg, err := parseIdentityConfig(configBytes, true)
	if err != nil {
		return JudgeReport{}, err
	}
	fixture, source, err := decodeJudgeInput(input)
	if err != nil {
		return JudgeReport{}, err
	}
	if fixture.Task != reportidentity.TaskPriorThread || len(fixture.Decisions) != 0 {
		return JudgeReport{}, errors.New("judge: expected a prior_threads fixture without decisions")
	}
	routing := routePriorPairs(fixture, source.PlannerKeys, options.ExactKey)
	runCfg := runnerConfiguration(cfg, reportidentity.TaskPriorThread, digest(configBytes))
	runCfg.SuitePath = "judge/prior-threads"
	backend := identityexperiment.SystemOneBackend(runCfg, options.SystemOne)
	if cfg.Provider.Kind == "openai_decisions" {
		runCfg.CredentialKey = "openai_api_key"
		backend = identityexperiment.OpenAIDecisionsBackend(runCfg, options.NewDecisionsClient)
	}
	withinCallBudget := routing.count <= cfg.Budget.MaxCalls
	prepared, err := identityexperiment.PrepareSelected(source.Fixture, runCfg, backend, func(pair reportidentity.Pair) bool {
		return withinCallBudget && routing.selected[pair.LeftID][pair.RightID]
	})
	if err != nil {
		return JudgeReport{}, err
	}
	if !withinCallBudget || prepared.TotalRequestBytes() > cfg.Budget.MaxTotalRequestBytes {
		prepared = prepared.WithPreflightError("budget_exceeded")
	}
	publisher, err := identityexperiment.ReserveOutput(outputDir, identityexperiment.FileDependencies{})
	if err != nil {
		return JudgeReport{}, err
	}
	if err := atomicWritePrivate(outputDir, "input.json", input, identityexperiment.FileDependencies{}); err != nil {
		return JudgeReport{}, err
	}
	inferenceCtx, cancel := context.WithTimeout(ctx, cfg.InferenceLimit)
	defer cancel()
	result := identityexperiment.Execute(inferenceCtx, prepared, identityexperiment.Dependencies{
		ResolveAPIKey: func(ctx context.Context) (string, error) {
			if options.ResolveKey == nil {
				return "", errors.New("judge: credential resolver unavailable")
			}
			return options.ResolveKey(ctx, cfg.Provider.Kind, cfg.Provider.Credential)
		},
	}, backend)
	if _, err := publisher.Publish(result); err != nil {
		return JudgeReport{}, err
	}
	report := JudgeReport{
		SchemaVersion: 1, InputSHA256: digest(input), Status: "complete", ExactMatches: routing.exact, SelectedPairs: routing.count,
		ElapsedMS: time.Since(started).Milliseconds(), Receipts: result.Receipts, Suggestions: []Suggestion{},
	}
	if result.Receipts.Status == "error" {
		report.Status = "unavailable"
	} else {
		report.Suggestions, report.Status = collectSuggestions(result.Receipts.Attempts, cfg.MinimumConfidence, source.OriginalThreads)
	}
	data, err := encodeCanonical(report)
	if err != nil {
		return JudgeReport{}, err
	}
	if err := atomicWritePrivate(outputDir, "suggestions.json", data, identityexperiment.FileDependencies{}); err != nil {
		return JudgeReport{}, err
	}
	return report, nil
}

type priorRouting struct {
	exact    int
	count    int
	selected map[string]map[string]bool
}

func routePriorPairs(input reportidentity.Input, keys map[string]string, exactKey func(string, string) (string, bool)) priorRouting {
	out := priorRouting{selected: make(map[string]map[string]bool)}
	keyFor := func(id, path, body string) string {
		if keys != nil {
			return keys[id]
		}
		if exactKey == nil {
			return ""
		}
		key, _ := exactKey(path, body)
		return key
	}
	threadTerms := make(map[string]map[string]struct{})
	existing := make(map[string]bool)
	for _, thread := range input.Threads {
		if !thread.Eligible {
			continue
		}
		key := keyFor(thread.ID, thread.FilePath, thread.Body)
		if key != "" {
			existing[key] = true
		}
		threadTerms[thread.ID] = matchingTerms(thread.Body)
	}
	for _, finding := range input.Findings {
		id := string(finding.ID)
		key := keyFor(id, finding.FilePath, finding.Body)
		if key != "" && existing[key] {
			out.exact++
			continue
		}
		terms := matchingTerms(finding.Body)
		for _, thread := range input.Threads {
			if !thread.Eligible || (thread.FilePath != finding.FilePath && !termOverlap(terms, threadTerms[thread.ID])) {
				continue
			}
			if out.selected[id] == nil {
				out.selected[id] = make(map[string]bool)
			}
			out.selected[id][thread.ID] = true
			out.count++
		}
	}
	return out
}

func matchingTerms(body string) map[string]struct{} {
	terms := make(map[string]struct{})
	for _, word := range strings.FieldsFunc(strings.ToLower(body), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}) {
		if len(word) < 4 || strings.Contains(" after also before both could every first from have into more only other same should than that their them then there these this those when where which while will with would ", " "+word+" ") {
			continue
		}
		terms[word] = struct{}{}
	}
	return terms
}

func termOverlap(a, b map[string]struct{}) bool {
	shared := 0
	for token := range a {
		if _, ok := b[token]; ok {
			shared++
		}
	}
	return shared >= 3 && shared*100 >= 13*(len(a)+len(b)-shared)
}

func collectSuggestions(attempts []identityexperiment.Attempt, minimum float64, originalThreads map[string]string) ([]Suggestion, string) {
	var positives []Suggestion
	blocked := make(map[string]bool)
	positiveCount := make(map[string]int)
	status := "complete"
	for _, attempt := range attempts {
		if attempt.Answer == nil || attempt.ErrorCode != "" || attempt.Answer.Choice == reportidentity.ChoiceAbstain ||
			(attempt.Answer.Choice == reportidentity.ChoiceSame && attempt.Answer.Confidence < minimum) ||
			(attempt.Answer.Choice == reportidentity.ChoiceDifferent && attempt.Answer.Confidence < 0.80) {
			blocked[attempt.LeftID] = true
			status = "partial"
			continue
		}
		if attempt.Answer.Choice == reportidentity.ChoiceSame {
			threadID := attempt.RightID
			if originalThreads != nil {
				threadID = originalThreads[threadID]
				if threadID == "" {
					blocked[attempt.LeftID] = true
					status = "partial"
					continue
				}
			}
			positiveCount[attempt.LeftID]++
			positives = append(positives, Suggestion{FindingID: attempt.LeftID, ThreadID: threadID, Confidence: attempt.Answer.Confidence})
		}
	}
	out := make([]Suggestion, 0, len(positives))
	for _, suggestion := range positives {
		if blocked[suggestion.FindingID] || positiveCount[suggestion.FindingID] != 1 {
			status = "partial"
			continue
		}
		out = append(out, suggestion)
	}
	return out, status
}

type priorJudgeObserver struct{ options JudgeOptions }

// NewPriorJudgeObserver runs the shared judge on an authoritative review's copy.
func NewPriorJudgeObserver(options JudgeOptions) Observer {
	return priorJudgeObserver{options: options}
}

func (o priorJudgeObserver) Observe(ctx context.Context, snapshot reportidentitysnapshot.Snapshot, attemptDir string) {
	_, input, mapping, err := buildTaskInputs(snapshot)
	if err != nil {
		o.print("invalid_input", attemptDir, 0)
		return
	}
	reverse := make(map[string]string, len(mapping))
	for original, replay := range mapping {
		reverse[replay] = original
	}
	keys := make(map[string]string, len(snapshot.Baseline.PriorKeys))
	for _, key := range snapshot.Baseline.PriorKeys {
		id := key.RecordID
		switch key.RecordKind {
		case "finding":
		case "thread":
			id = mapping[id]
		default:
			continue
		}
		if id != "" && key.KeySHA256 != nil {
			keys[id] = *key.KeySHA256
		}
	}
	if err := secureExistingOrCreateDirectory(filepath.Dir(attemptDir), lstatFunc(identityexperiment.FileDependencies{}), mkdirFunc(identityexperiment.FileDependencies{}), chmodFunc(identityexperiment.FileDependencies{})); err != nil {
		o.print("artifact_failure", attemptDir, 0)
		return
	}
	raw, err := encodeCanonical(input)
	if err != nil {
		o.print("invalid_input", attemptDir, 0)
		return
	}
	captured, err := encodeCanonical(capturedJudgeInput{SchemaVersion: 1, Fixture: raw, PlannerKeys: keys, OriginalThreads: reverse})
	if err != nil {
		o.print("invalid_input", attemptDir, 0)
		return
	}
	report, err := RunPriorJudge(ctx, o.options, captured, attemptDir)
	if err != nil {
		o.print("unavailable", attemptDir, 0)
		return
	}
	o.print(report.Status, attemptDir, len(report.Suggestions))
}

func (o priorJudgeObserver) print(status, path string, count int) {
	if o.options.Warnings != nil {
		_, _ = fmt.Fprintf(o.options.Warnings, "cr judge prior-threads status=%s suggestions=%d artifacts=%q\n", status, count, path)
	}
}
