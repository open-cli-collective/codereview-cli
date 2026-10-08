package llmlifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/fsatomic"
	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/runlock"
)

const maxCostGenerations = 64

// RequestCostCheckpointBinding binds optional telemetry, never output reuse.
type RequestCostCheckpointBinding struct {
	Version       int    `json:"version"`
	ArtifactScope string `json:"artifact_scope"`
	RunID         string `json:"run_id"`
	TaskID        string `json:"task_id"`
	Sequence      uint64 `json:"sequence"`
	Digest        string `json:"digest"`
}

type costGeneration struct {
	GenerationID     string `json:"generation_id"`
	InputFingerprint string `json:"input_fingerprint"`
	EvidenceDigest   string `json:"evidence_digest"`
}

type pendingCostGeneration struct {
	GenerationID     string `json:"generation_id"`
	InputFingerprint string `json:"input_fingerprint"`
}

// Map-free fixed-order v1 digest projection; only self-digest is excluded.
type costCheckpointProjection struct {
	Version             int                      `json:"version"`
	ArtifactScope       string                   `json:"artifact_scope"`
	RunID               string                   `json:"run_id"`
	TaskID              string                   `json:"task_id"`
	Sequence            uint64                   `json:"sequence"`
	FreshOrigin         bool                     `json:"fresh_origin"`
	GenerationsObserved uint64                   `json:"generations_observed"`
	OmittedGenerations  uint64                   `json:"omitted_generations"`
	Evidence            *llm.RequestCostEvidence `json:"evidence"`
	Generations         []costGeneration         `json:"generations"`
	Pending             *pendingCostGeneration   `json:"pending"`
}

type costCheckpoint struct {
	costCheckpointProjection
	Digest string `json:"digest"`
}

// RequestCostCheckpoint lives outside the resettable task directory but inside
// the caller/run-owned artifact root covered by existing retention/purge.
func (p Paths) RequestCostCheckpoint(taskID string) (string, error) {
	if _, err := p.TaskDir(taskID); err != nil {
		return "", err
	}
	// Keep suffixes and the atomic writer's .tmp inside NAME_MAX even when
	// the existing encoded task directory already uses that component budget.
	identity := sha256.Sum256([]byte(strings.TrimSpace(taskID)))
	return filepath.Join(p.LLMTasksDir, ".request-cost", hex.EncodeToString(identity[:])+".json"), nil
}

func taskCostLock(paths Paths, taskID string) (*runlock.Lock, error) {
	p, err := paths.RequestCostCheckpoint(taskID)
	if err != nil {
		return nil, err
	}
	lock, err := runlock.Acquire(strings.TrimSuffix(p, ".json") + ".lock")
	if err != nil {
		return nil, fmt.Errorf("llmlifecycle: acquire task ownership: %w", err)
	}
	return lock, nil
}

func artifactCostScope(paths Paths) (string, error) {
	p, err := filepath.Abs(paths.LLMTasksDir)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(filepath.Clean(p)))
	return hex.EncodeToString(h[:]), nil
}

func newCostCheckpoint(paths Paths, taskID, runID string) (*costCheckpoint, error) {
	scope, err := artifactCostScope(paths)
	if err != nil {
		return nil, err
	}
	if !llm.SafeCostString(strings.TrimSpace(taskID), 256) || (runID != "" && !llm.SafeCostString(runID, 256)) {
		return nil, errors.New("llmlifecycle: invalid request-cost scope identity")
	}
	e := llm.UnknownRequestCostEvidence(llm.CostGapLegacy)
	e.Scope = "task_artifact_history"
	e.ArtifactScope = scope
	e.TaskID = strings.TrimSpace(taskID)
	e.RunID = runID
	return &costCheckpoint{costCheckpointProjection: costCheckpointProjection{Version: 1, ArtifactScope: scope, TaskID: e.TaskID, RunID: runID, Evidence: e, Generations: []costGeneration{}}}, nil
}

func costCheckpointBytes(c *costCheckpoint) ([]byte, error) {
	return json.Marshal(c.costCheckpointProjection)
}
func checkpointDigest(c *costCheckpoint) (string, error) {
	b, err := costCheckpointBytes(c)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("cr-request-cost-checkpoint/v1\n"))
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}
func costGap(c *costCheckpoint, gap llm.CostEvidenceGap) {
	c.Evidence.Gaps = llm.AddCostGap(c.Evidence.Gaps, gap)
	c.Evidence.AttemptCountExact = false
}
func isCostDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func readCostCheckpoint(paths Paths, taskID, runID string, anyRun bool) (*costCheckpoint, llm.CostEvidenceGap) {
	p, err := paths.RequestCostCheckpoint(taskID)
	if err != nil {
		return nil, llm.CostGapCheckpoint
	}
	f, err := os.Open(p) // #nosec G304 -- path is derived from the caller-owned root.
	if err != nil {
		return nil, llm.CostGapCheckpoint
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, llm.MaxRequestCostEvidenceBytes+1))
	if err != nil {
		return nil, llm.CostGapCheckpoint
	}
	if len(b) > llm.MaxRequestCostEvidenceBytes {
		return nil, llm.CostGapOverflow
	}
	var c costCheckpoint
	if !llm.RequestCostJSONValid(b) || json.Unmarshal(b, &c) != nil || c.Version != 1 || c.Evidence == nil {
		return nil, llm.CostGapCheckpoint
	}
	var rawScope struct {
		RunID *string `json:"run_id"`
	}
	if json.Unmarshal(b, &rawScope) != nil || rawScope.RunID == nil || (*rawScope.RunID != "" && !llm.SafeCostString(*rawScope.RunID, 256)) {
		return nil, llm.CostGapScope
	}
	scope, err := artifactCostScope(paths)
	if err != nil || c.ArtifactScope != scope || c.TaskID != strings.TrimSpace(taskID) || (!anyRun && c.RunID != runID) {
		return nil, llm.CostGapScope
	}
	if len(c.Generations) > maxCostGenerations || len(c.Evidence.Attempts) > llm.MaxRequestCostAttempts || len(c.Evidence.Gaps) > llm.MaxRequestCostGaps {
		return nil, llm.CostGapOverflow
	}
	if c.FreshOrigin || c.Evidence.Version != 1 || c.Evidence.Source != "openai_responses" || c.Evidence.Scope != "task_artifact_history" || c.Evidence.ArtifactScope != c.ArtifactScope || c.Evidence.RunID != c.RunID || c.Evidence.TaskID != c.TaskID {
		return nil, llm.CostGapScope
	}
	digest, err := checkpointDigest(&c)
	if err != nil || digest != c.Digest {
		return nil, llm.CostGapConflict
	}
	c.Evidence = llm.NormalizeRequestCostEvidence(c.Evidence)
	seen := map[string]bool{}
	for _, g := range c.Generations {
		if !llm.SafeCostString(g.GenerationID, 256) || (g.InputFingerprint != "" && !llm.SafeCostString(g.InputFingerprint, 256)) || (g.EvidenceDigest != "" && !isCostDigest(g.EvidenceDigest)) {
			return nil, llm.CostGapInvalid
		}
		if seen[g.GenerationID] {
			costGap(&c, llm.CostGapDuplicate)
		}
		seen[g.GenerationID] = true
	}
	if c.Pending != nil && (!llm.SafeCostString(c.Pending.GenerationID, 256) || !llm.SafeCostString(c.Pending.InputFingerprint, 256)) {
		return nil, llm.CostGapInvalid
	}
	if c.Pending != nil && seen[c.Pending.GenerationID] {
		costGap(&c, llm.CostGapDuplicate)
	}
	if c.GenerationsObserved < uint64(len(c.Generations)) {
		costGap(&c, llm.CostGapInvalid)
	}
	validateCostAttemptIdentities(&c)
	costGap(&c, llm.CostGapLegacy)
	return &c, ""
}

func validateCostAttemptIdentities(c *costCheckpoint) {
	seen := map[string]bool{}
	ids := map[string]string{}
	ordinals := map[string]uint32{}
	for _, a := range c.Evidence.Attempts {
		key := fmt.Sprintf("%s/%d", a.GenerationID, a.Ordinal)
		if a.GenerationID == "" || a.Ordinal == 0 || a.AdapterAttempt == 0 || (a.ValidationPhase != "initial" && a.ValidationPhase != "correction") || seen[key] || a.Ordinal != ordinals[a.GenerationID]+1 {
			costGap(c, llm.CostGapDuplicate)
		}
		seen[key] = true
		ordinals[a.GenerationID] = a.Ordinal
		if a.ProviderResponseID != nil {
			if old, ok := ids[*a.ProviderResponseID]; ok && old != key {
				costGap(c, llm.CostGapDuplicate)
			}
			ids[*a.ProviderResponseID] = key
		}
	}
}
func sameCostAttempt(a, b llm.RequestCostAttempt) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// Admit only new records into the retained prefix, leaving framing/pending
// capacity. A later generation must not evict an earlier observation to fit.
func appendCostAttempt(c *costCheckpoint, a llm.RequestCostAttempt) bool {
	if len(c.Evidence.Attempts) >= llm.MaxRequestCostAttempts {
		return false
	}
	c.Evidence.Attempts = append(c.Evidence.Attempts, a)
	b, _ := costCheckpointBytes(c)
	if len(b) > llm.MaxRequestCostEvidenceBytes-llm.RequestCostReservedBytes {
		c.Evidence.Attempts = c.Evidence.Attempts[:len(c.Evidence.Attempts)-1]
		return false
	}
	return true
}
func appendCostGeneration(c *costCheckpoint, g costGeneration) bool {
	if len(c.Generations) >= maxCostGenerations || c.OmittedGenerations != 0 {
		return false
	}
	c.Generations = append(c.Generations, g)
	b, _ := costCheckpointBytes(c)
	if len(b) > llm.MaxRequestCostEvidenceBytes-llm.RequestCostReservedBytes {
		c.Generations = c.Generations[:len(c.Generations)-1]
		return false
	}
	return true
}

// Repeated snapshots are references, not new charges. A conflicting same-identity
// copy keeps the existing prefix and a sticky ambiguity/loss reason.
func mergeCostObservations(c *costCheckpoint, other *llm.RequestCostEvidence) {
	if other == nil {
		return
	}
	e := llm.NormalizeRequestCostEvidence(other)
	for _, gap := range e.Gaps {
		costGap(c, gap)
	}
	if e.Scope != "task_artifact_history" || e.ArtifactScope != c.ArtifactScope || e.RunID != c.RunID || e.TaskID != c.TaskID {
		costGap(c, llm.CostGapScope)
		return
	}
	index := map[string]llm.RequestCostAttempt{}
	for _, a := range c.Evidence.Attempts {
		index[fmt.Sprintf("%s/%d", a.GenerationID, a.Ordinal)] = a
	}
	blocked := c.Evidence.Truncated
	var omitted uint64
	for _, a := range e.Attempts {
		key := fmt.Sprintf("%s/%d", a.GenerationID, a.Ordinal)
		if prior, ok := index[key]; ok {
			if !sameCostAttempt(prior, a) {
				costGap(c, llm.CostGapConflict)
			}
			continue
		}
		if blocked || !appendCostAttempt(c, a) {
			blocked = true
			c.Evidence.Truncated = true
			omitted = llm.SaturatingCostAdd(omitted, 1)
			costGap(c, llm.CostGapOverflow)
			continue
		}
		index[key] = a
	}
	if omitted > c.Evidence.DroppedAttempts {
		c.Evidence.DroppedAttempts = omitted
	}
	if e.AttemptsObserved > c.Evidence.AttemptsObserved {
		c.Evidence.AttemptsObserved = e.AttemptsObserved
	}
	if e.DroppedAttempts > c.Evidence.DroppedAttempts {
		c.Evidence.DroppedAttempts = e.DroppedAttempts
	}
	if c.Evidence.AttemptsObserved < uint64(len(c.Evidence.Attempts)) {
		c.Evidence.AttemptsObserved = uint64(len(c.Evidence.Attempts))
	}
	c.Evidence.Truncated = c.Evidence.Truncated || e.Truncated
	validateCostAttemptIdentities(c)
	reconcileKnownCostGenerations(c)
}

// Metadata-only observations establish a lower bound on observed generations,
// but cannot recreate missing finalized fingerprints/digests. Keep those missing
// descriptors as omissions with a gap rather than reporting a zero count.
func reconcileKnownCostGenerations(c *costCheckpoint) {
	known := map[string]bool{}
	described := map[string]bool{}
	for _, g := range c.Generations {
		known[g.GenerationID] = true
		described[g.GenerationID] = true
	}
	for _, a := range c.Evidence.Attempts {
		if a.GenerationID != "" {
			known[a.GenerationID] = true
		}
	}
	var missing uint64
	for id := range known {
		if !described[id] {
			missing++
		}
	}
	if uint64(len(known)) > c.GenerationsObserved {
		c.GenerationsObserved = uint64(len(known))
	}
	if missing > 0 {
		costGap(c, llm.CostGapMissing)
		if missing > c.OmittedGenerations {
			c.OmittedGenerations = missing
		}
	}
}

// The same bounded metadata-plus-sidecar reconciliation is used for execution,
// cache load, and reset, so reset cannot erase the last known A/B observations.
func reconcileCostCheckpoint(paths Paths, taskID, runID string, meta *Metadata, anyRun bool) (*costCheckpoint, error) {
	// Reset has no explicit RunID argument. Valid same-root/task metadata is
	// authoritative for its known observations; a foreign-run sidecar must not
	// win and cause that sole metadata prefix to be discarded before deletion.
	if anyRun && meta != nil && meta.requestCostScopeValid && meta.RequestCostEvidence != nil {
		e := meta.RequestCostEvidence
		scope, err := artifactCostScope(paths)
		if err == nil && e.Version == 1 && e.Source == "openai_responses" && e.Scope == "task_artifact_history" && e.ArtifactScope == scope && e.TaskID == strings.TrimSpace(taskID) {
			runID = e.RunID
			anyRun = false
		}
	}
	// With no valid raw scope, retain an independently valid same-root/task
	// sidecar first. A binding is only a reference to a checkpoint: syntactic
	// validity alone cannot authorize replacing an existing checkpoint under
	// another run identity, including with an invented empty/no-run scope.
	c, gap := readCostCheckpoint(paths, taskID, runID, anyRun)
	if c == nil {
		if anyRun && meta != nil && meta.RequestCostCheckpoint != nil {
			b := meta.RequestCostCheckpoint
			scope, err := artifactCostScope(paths)
			if err == nil && b.Version == 1 && b.ArtifactScope == scope && b.TaskID == strings.TrimSpace(taskID) && (b.RunID == "" || llm.SafeCostString(b.RunID, 256)) {
				runID = b.RunID
			}
		}
		var err error
		c, err = newCostCheckpoint(paths, taskID, runID)
		if err != nil {
			return nil, err
		}
		costGap(c, gap)
	}

	if meta != nil {
		if strings.TrimSpace(meta.TaskID) != c.TaskID {
			costGap(c, llm.CostGapScope)
		} else {
			b := meta.RequestCostCheckpoint
			if meta.requestCostScopeValid && b != nil && b.Digest == c.Digest && b.Sequence == c.Sequence && meta.RequestCostEvidence != nil {
				left, _ := llm.RequestCostEvidenceDigest(c.Evidence)
				right, _ := llm.RequestCostEvidenceDigest(meta.RequestCostEvidence)
				if left != right {
					costGap(c, llm.CostGapConflict)
				}
			}
			if meta.requestCostScopeValid {
				mergeCostObservations(c, meta.RequestCostEvidence)
			} else if meta.RequestCostEvidence != nil {
				costGap(c, llm.CostGapScope)
				for _, gap := range meta.RequestCostEvidence.Gaps {
					costGap(c, gap)
				}
			}
			if meta.RequestCostEvidence != nil && (b == nil || b.Version != 1 || b.ArtifactScope != c.ArtifactScope || b.RunID != c.RunID || b.TaskID != c.TaskID || b.Sequence != c.Sequence || b.Digest != c.Digest) {
				costGap(c, llm.CostGapConflict)
			}
		}
	}
	if c.Pending != nil {
		costGap(c, llm.CostGapInterrupted)
	}
	c.Evidence = llm.NormalizeRequestCostEvidence(c.Evidence)
	return c, nil
}

func boundCostCheckpoint(c *costCheckpoint) {
	c.FreshOrigin = false
	costGap(c, llm.CostGapLegacy)
	c.Evidence = llm.NormalizeRequestCostEvidence(c.Evidence)
	if c.Generations == nil {
		c.Generations = []costGeneration{}
	}
	if len(c.Generations) > maxCostGenerations {
		c.OmittedGenerations = llm.SaturatingCostAdd(c.OmittedGenerations, uint64(len(c.Generations)-maxCostGenerations))
		c.Generations = c.Generations[:maxCostGenerations]
		costGap(c, llm.CostGapOverflow)
	}
	for {
		b, _ := costCheckpointBytes(c)
		if len(b) <= llm.MaxRequestCostEvidenceBytes-128 {
			break
		}
		costGap(c, llm.CostGapOverflow)
		if len(c.Evidence.Attempts) > 0 {
			c.Evidence.Attempts = c.Evidence.Attempts[:len(c.Evidence.Attempts)-1]
			c.Evidence.DroppedAttempts = llm.SaturatingCostAdd(c.Evidence.DroppedAttempts, 1)
			c.Evidence.Truncated = true
		} else if len(c.Generations) > 0 {
			c.Generations = c.Generations[:len(c.Generations)-1]
			c.OmittedGenerations = llm.SaturatingCostAdd(c.OmittedGenerations, 1)
		} else {
			break
		}
	}
}

func writeCostCheckpoint(paths Paths, c *costCheckpoint) error {
	boundCostCheckpoint(c)
	if c.Sequence == ^uint64(0) {
		costGap(c, llm.CostGapOverflow)
	} else {
		c.Sequence++
	}
	digest, err := checkpointDigest(c)
	if err != nil {
		return err
	}
	c.Digest = digest
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	if len(b)+1 > llm.MaxRequestCostEvidenceBytes {
		return errors.New("llmlifecycle: request-cost checkpoint exceeds limit")
	}
	p, err := paths.RequestCostCheckpoint(c.TaskID)
	if err != nil {
		return err
	}
	// fsatomic is temp-file/rename without fsync. No power-loss claim is made.
	return fsatomic.WriteFileAtomic(p, append(b, '\n'), 0o600)
}

func costFingerprint(req Request) string {
	if s := strings.TrimSpace(req.InputFingerprint); s != "" {
		return s
	}
	return Fingerprint(adapterName(req.Adapter), req.TaskID, req.Phase, req.Model, req.Effort, req.Prompt, req.DependencyTaskIDs)
}

func beginCostGeneration(req Request, id string) (*costCheckpoint, error) {
	meta, ok, err := ReadMetadata(req.Paths, req.TaskID)
	if err != nil {
		return nil, err
	}
	var observed *Metadata
	if ok {
		observed = &meta
	}
	c, err := reconcileCostCheckpoint(req.Paths, req.TaskID, req.RunID, observed, false)
	if err != nil {
		return nil, err
	}
	fingerprint := costFingerprint(req)
	if !llm.SafeCostString(id, 256) || !llm.SafeCostString(fingerprint, 256) {
		return nil, errors.New("llmlifecycle: invalid cost generation identity")
	}
	for _, g := range c.Generations {
		if g.GenerationID == id {
			return nil, errors.New("llmlifecycle: duplicate cost generation identity")
		}
	}
	for _, a := range c.Evidence.Attempts {
		if a.GenerationID == id {
			return nil, errors.New("llmlifecycle: duplicate cost generation identity")
		}
	}
	if c.Pending != nil {
		if c.Pending.GenerationID == id {
			return nil, errors.New("llmlifecycle: duplicate pending cost generation identity")
		}
		c.OmittedGenerations = llm.SaturatingCostAdd(c.OmittedGenerations, 1)
		costGap(c, llm.CostGapInterrupted)
	}
	c.Pending = &pendingCostGeneration{GenerationID: id, InputFingerprint: fingerprint}
	if err := writeCostCheckpoint(req.Paths, c); err != nil {
		return nil, fmt.Errorf("llmlifecycle: checkpoint before dispatch: %w", err)
	}
	return c, nil
}

func finishCostGeneration(paths Paths, c *costCheckpoint, invocation *llm.RequestCostEvidence) error {
	if c == nil || c.Pending == nil {
		return errors.New("llmlifecycle: missing pending cost generation")
	}
	e := llm.NormalizeRequestCostEvidence(invocation)
	if e == nil || e.Scope != "invocation" {
		e = llm.UnknownRequestCostEvidence(llm.CostGapMissing)
	}
	for _, gap := range e.Gaps {
		costGap(c, gap)
	}
	c.Evidence.AttemptsObserved = llm.SaturatingCostAdd(c.Evidence.AttemptsObserved, e.AttemptsObserved)
	c.Evidence.DroppedAttempts = llm.SaturatingCostAdd(c.Evidence.DroppedAttempts, e.DroppedAttempts)
	// Preserve the invocation's retained prefix even when that invocation was
	// truncated; later attempts must not replace an already full task prefix.
	wasTruncated := c.Evidence.Truncated
	for _, a := range e.Attempts {
		a.GenerationID = c.Pending.GenerationID
		if wasTruncated || !appendCostAttempt(c, a) {
			wasTruncated = true
			c.Evidence.Truncated = true
			c.Evidence.DroppedAttempts = llm.SaturatingCostAdd(c.Evidence.DroppedAttempts, 1)
			costGap(c, llm.CostGapOverflow)
		}
	}
	c.Evidence.Truncated = c.Evidence.Truncated || e.Truncated
	digest, err := llm.RequestCostEvidenceDigest(e)
	if err != nil {
		return err
	}
	c.GenerationsObserved = llm.SaturatingCostAdd(c.GenerationsObserved, 1)
	if !appendCostGeneration(c, costGeneration{GenerationID: c.Pending.GenerationID, InputFingerprint: c.Pending.InputFingerprint, EvidenceDigest: digest}) {
		c.OmittedGenerations = llm.SaturatingCostAdd(c.OmittedGenerations, 1)
		costGap(c, llm.CostGapOverflow)
	}
	c.Pending = nil
	validateCostAttemptIdentities(c)
	return writeCostCheckpoint(paths, c)
}

func checkpointBinding(c *costCheckpoint) *RequestCostCheckpointBinding {
	if c == nil {
		return nil
	}
	return &RequestCostCheckpointBinding{Version: 1, ArtifactScope: c.ArtifactScope, RunID: c.RunID, TaskID: c.TaskID, Sequence: c.Sequence, Digest: c.Digest}
}

func overlayCostMetadata(paths Paths, taskID, runID string, meta *Metadata) {
	c, err := reconcileCostCheckpoint(paths, taskID, runID, meta, false)
	if err != nil {
		meta.RequestCostEvidence = llm.UnknownRequestCostEvidence(llm.CostGapCheckpoint)
		return
	}
	meta.RequestCostEvidence = llm.CloneRequestCostEvidence(c.Evidence)
}

// Both schema-4 readers share this decoder. Optional cost corruption must not
// invalidate valid output/tool fields. The existing whole metadata read remains
// unbounded; the nested cost envelope is bounded before typed nested decoding.
func decodeMetadata(data []byte, meta *Metadata) error {
	type ordinary Metadata
	var raw struct {
		*ordinary
		Cost    json.RawMessage `json:"request_cost_evidence"`
		Binding json.RawMessage `json:"request_cost_checkpoint"`
	}
	raw.ordinary = (*ordinary)(meta)
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	meta.requestCostScopeValid = validRawMetadataCostScope(raw.Cost)
	meta.RequestCostEvidence = llm.DecodeRequestCostEvidence(raw.Cost)
	meta.RequestCostCheckpoint = nil
	if len(raw.Binding) > 0 && string(raw.Binding) != "null" {
		var b RequestCostCheckpointBinding
		if len(raw.Binding) > llm.RequestCostReservedBytes || !llm.RequestCostJSONValid(raw.Binding) || json.Unmarshal(raw.Binding, &b) != nil || b.Version != 1 || !isCostDigest(b.Digest) || !llm.SafeCostString(b.TaskID, 256) || !isCostDigest(b.ArtifactScope) || (b.RunID != "" && !llm.SafeCostString(b.RunID, 256)) {
			if meta.RequestCostEvidence == nil {
				meta.RequestCostEvidence = llm.UnknownRequestCostEvidence(llm.CostGapInvalid)
			} else {
				meta.RequestCostEvidence.Gaps = llm.AddCostGap(meta.RequestCostEvidence.Gaps, llm.CostGapInvalid)
				meta.RequestCostEvidence.AttemptCountExact = false
			}
		} else {
			meta.RequestCostCheckpoint = &b
		}
	}
	if duplicateMetadataCostFields(data) {
		meta.RequestCostEvidence = llm.UnknownRequestCostEvidence(llm.CostGapDuplicate)
		meta.RequestCostCheckpoint = nil
		meta.requestCostScopeValid = false
	}
	return nil
}

func duplicateMetadataCostFields(data []byte) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err = d.Token()
		if err != nil {
			return false
		}
		key, ok := token.(string)
		if !ok {
			return false
		}
		canonical := ""
		if strings.EqualFold(key, "request_cost_evidence") {
			canonical = "request_cost_evidence"
		}
		if strings.EqualFold(key, "request_cost_checkpoint") {
			canonical = "request_cost_checkpoint"
		}
		if canonical != "" {
			if seen[canonical] {
				return true
			}
			seen[canonical] = true
		}
		var ignored json.RawMessage
		if d.Decode(&ignored) != nil {
			return false
		}
	}
	return false
}

// Scope validity is measured on the raw envelope before normalization can omit
// invalid strings. In particular, an omitted invalid RunID is not proof of an
// actual empty/no-run scope. Only this raw fact (or a valid binding) can guide
// reset's scope selection; it never establishes complete accounting history.
func validRawMetadataCostScope(raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > llm.MaxRequestCostEvidenceBytes || !llm.RequestCostJSONValid(raw) {
		return false
	}
	var scope struct {
		Version       int     `json:"version"`
		Source        string  `json:"source"`
		Scope         string  `json:"scope"`
		ArtifactScope string  `json:"artifact_scope"`
		RunID         *string `json:"run_id"`
		TaskID        string  `json:"task_id"`
	}
	if json.Unmarshal(raw, &scope) != nil {
		return false
	}
	return scope.Version == 1 && scope.Source == "openai_responses" && scope.Scope == "task_artifact_history" && isCostDigest(scope.ArtifactScope) && llm.SafeCostString(scope.TaskID, 256) && scope.RunID != nil && (*scope.RunID == "" || llm.SafeCostString(*scope.RunID, 256))
}
