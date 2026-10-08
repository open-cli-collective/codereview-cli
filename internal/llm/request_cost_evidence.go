package llm

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Cost evidence is bounded client telemetry, never provider-reported cost.
const (
	MaxRequestCostAttempts      = 64
	MaxRequestCostEvidenceBytes = 256 * 1024
	RequestCostReservedBytes    = 4 * 1024
	MaxRequestCostGaps          = 16
)

// CostEvidenceGap is a finite allow-listed reason for incomplete evidence.
type CostEvidenceGap string

const (
	CostGapMissing     CostEvidenceGap = "missing_evidence"
	CostGapInvalid     CostEvidenceGap = "invalid_evidence"
	CostGapUsage       CostEvidenceGap = "missing_or_invalid_usage"
	CostGapIdentity    CostEvidenceGap = "missing_or_invalid_identity"
	CostGapResponse    CostEvidenceGap = "unsupported_response"
	CostGapTransport   CostEvidenceGap = "opaque_transport"
	CostGapEndpoint    CostEvidenceGap = "custom_or_unverified_endpoint"
	CostGapDispatch    CostEvidenceGap = "unresolved_dispatch"
	CostGapOverflow    CostEvidenceGap = "overflow"
	CostGapLegacy      CostEvidenceGap = "legacy_history_unknown"
	CostGapInterrupted CostEvidenceGap = "interrupted_generation"
	CostGapCheckpoint  CostEvidenceGap = "checkpoint_unavailable"
	CostGapConflict    CostEvidenceGap = "conflicting_evidence"
	CostGapDuplicate   CostEvidenceGap = "duplicate_identity"
	CostGapScope       CostEvidenceGap = "scope_mismatch"
	CostGapPending     CostEvidenceGap = "pending_generation"
)

// RequestCostProducer opts into collection. Start errors without a stream MUST
// mean no dispatch. Returning both a stream and an error is unsupported and
// remains missing/possibly-dispatched evidence; the direct API never does so.
// Only the concrete direct OpenAI adapter opts in in v1.
type RequestCostProducer interface{ RequestCostEvidenceSource() string }

// RequestCostSource returns only an explicitly supported producer contract.
func RequestCostSource(adapter Adapter) string {
	p, ok := adapter.(RequestCostProducer)
	if ok && p.RequestCostEvidenceSource() == "openai_responses" {
		return "openai_responses"
	}
	return ""
}

// RequestCostEvidence preserves ordered observations, independently of output.
type RequestCostEvidence struct {
	Version           int                  `json:"version"`
	Source            string               `json:"source"`
	Scope             string               `json:"scope"`
	ArtifactScope     string               `json:"artifact_scope"`
	RunID             string               `json:"run_id"`
	TaskID            string               `json:"task_id"`
	AttemptsObserved  uint64               `json:"attempts_observed"`
	AttemptCountExact bool                 `json:"attempt_count_exact"`
	Truncated         bool                 `json:"truncated"`
	DroppedAttempts   uint64               `json:"dropped_attempts"`
	Gaps              []CostEvidenceGap    `json:"gaps"`
	Attempts          []RequestCostAttempt `json:"attempts"`
}

// RequestCostAttempt excludes bodies, URLs, headers and raw errors.
type RequestCostAttempt struct {
	GenerationID         string              `json:"generation_id"`
	Ordinal              uint32              `json:"ordinal"`
	ValidationPhase      string              `json:"validation_phase"`
	AdapterAttempt       uint32              `json:"adapter_attempt"`
	RuntimeID            string              `json:"runtime_id"`
	EndpointKind         string              `json:"endpoint_kind"`
	TransportMode        string              `json:"transport_mode"`
	Dispatch             string              `json:"dispatch"`
	HTTPStatus           *int                `json:"http_status"`
	BodyState            string              `json:"body_state"`
	ProviderResponseID   *string             `json:"provider_response_id"`
	ObservedModel        *string             `json:"observed_model"`
	ObservedServiceTier  *string             `json:"observed_service_tier"`
	ResponseStatus       *string             `json:"response_status"`
	RequestedModel       string              `json:"requested_model"`
	RequestedServiceTier *string             `json:"requested_service_tier"`
	Usage                *OpenAIRequestUsage `json:"usage"`
	Issues               []CostEvidenceGap   `json:"issues"`
}

// OpenAIRequestUsage preserves absence and negative observations without pricing.
type OpenAIRequestUsage struct {
	InputTokens      *int64 `json:"input_tokens"`
	OutputTokens     *int64 `json:"output_tokens"`
	CacheReadTokens  *int64 `json:"cache_read_tokens"`
	CacheWriteTokens *int64 `json:"cache_write_tokens"`
	TotalTokens      *int64 `json:"total_tokens"`
}

func costCopy[T any](p *T) *T {
	if p == nil {
		return nil
	}
	v := *p
	return &v
}

// CloneRequestCostEvidence makes a detached copy, including all nested pointers.
func CloneRequestCostEvidence(e *RequestCostEvidence) *RequestCostEvidence {
	if e == nil {
		return nil
	}
	c := *e
	c.Gaps = append([]CostEvidenceGap{}, e.Gaps...)
	c.Attempts = make([]RequestCostAttempt, len(e.Attempts))
	for i, a := range e.Attempts {
		a.HTTPStatus = costCopy(a.HTTPStatus)
		a.ProviderResponseID = costCopy(a.ProviderResponseID)
		a.ObservedModel = costCopy(a.ObservedModel)
		a.ObservedServiceTier = costCopy(a.ObservedServiceTier)
		a.ResponseStatus = costCopy(a.ResponseStatus)
		a.RequestedServiceTier = costCopy(a.RequestedServiceTier)
		a.Issues = append([]CostEvidenceGap{}, a.Issues...)
		if a.Usage != nil {
			u := *a.Usage
			u.InputTokens = costCopy(u.InputTokens)
			u.OutputTokens = costCopy(u.OutputTokens)
			u.CacheReadTokens = costCopy(u.CacheReadTokens)
			u.CacheWriteTokens = costCopy(u.CacheWriteTokens)
			u.TotalTokens = costCopy(u.TotalTokens)
			a.Usage = &u
		}
		c.Attempts[i] = a
	}
	return &c
}

// SaturatingCostAdd prevents observation/omission counters wrapping to zero.
func SaturatingCostAdd(a, b uint64) uint64 {
	if math.MaxUint64-a < b {
		return math.MaxUint64
	}
	return a + b
}

// SafeCostString accepts bounded UTF-8 without controls, preserving exact bytes.
func SafeCostString(s string, limit int) bool {
	if s == "" || len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validCostGap(g CostEvidenceGap) bool {
	switch g {
	case CostGapMissing, CostGapInvalid, CostGapUsage, CostGapIdentity, CostGapResponse, CostGapTransport,
		CostGapEndpoint, CostGapDispatch, CostGapOverflow, CostGapLegacy, CostGapInterrupted, CostGapCheckpoint,
		CostGapConflict, CostGapDuplicate, CostGapScope, CostGapPending:
		return true
	}
	return false
}

// AddCostGap deduplicates/sorts the finite enum set; arbitrary errors are excluded.
func AddCostGap(gaps []CostEvidenceGap, gap CostEvidenceGap) []CostEvidenceGap {
	if !validCostGap(gap) {
		gap = CostGapInvalid
	}
	if !slices.Contains(gaps, gap) {
		gaps = append(gaps, gap)
	}
	slices.Sort(gaps)
	return gaps
}

// UnknownRequestCostEvidence constructs an explicit bounded unknown value.
func UnknownRequestCostEvidence(gap CostEvidenceGap) *RequestCostEvidence {
	return &RequestCostEvidence{Version: 1, Source: "openai_responses", Scope: "invocation", Gaps: []CostEvidenceGap{gap}, Attempts: []RequestCostAttempt{}}
}

// NormalizeRequestCostEvidence bounds and sanitizes a detached snapshot. It
// never repairs a gap or manufactures usage/identity. The retained prefix wins.
func NormalizeRequestCostEvidence(e *RequestCostEvidence) *RequestCostEvidence {
	if e == nil {
		return nil
	}
	c := *e
	c.Gaps = []CostEvidenceGap{}
	c.Attempts = []RequestCostAttempt{}
	for _, g := range e.Gaps {
		c.Gaps = AddCostGap(c.Gaps, g)
	}
	if c.Version != 1 || c.Source != "openai_responses" || (c.Scope != "invocation" && c.Scope != "task_artifact_history") {
		return UnknownRequestCostEvidence(CostGapInvalid)
	}
	for _, p := range []*string{&c.ArtifactScope, &c.RunID, &c.TaskID} {
		if *p != "" && !SafeCostString(*p, 256) {
			*p = ""
			c.Gaps = AddCostGap(c.Gaps, CostGapScope)
		}
	}
	for i, raw := range e.Attempts {
		if i >= MaxRequestCostAttempts {
			c.DroppedAttempts = SaturatingCostAdd(c.DroppedAttempts, uint64(len(e.Attempts)-i))
			c.Truncated = true
			break
		}
		a := raw
		a.Issues = []CostEvidenceGap{}
		for _, g := range raw.Issues {
			a.Issues = AddCostGap(a.Issues, g)
		}
		for _, p := range []*string{&a.GenerationID, &a.RequestedModel} {
			if *p != "" && !SafeCostString(*p, 256) {
				*p = ""
				a.Issues = AddCostGap(a.Issues, CostGapIdentity)
			}
		}
		for _, field := range []struct {
			p     **string
			limit int
		}{{&a.ProviderResponseID, 256}, {&a.ObservedModel, 256}, {&a.ObservedServiceTier, 64}, {&a.ResponseStatus, 64}, {&a.RequestedServiceTier, 64}} {
			if *field.p != nil && !SafeCostString(**field.p, field.limit) {
				*field.p = nil
				a.Issues = AddCostGap(a.Issues, CostGapIdentity)
			}
		}
		if a.RuntimeID != "openai-api-key" {
			a.RuntimeID = ""
			a.Issues = AddCostGap(a.Issues, CostGapInvalid)
		}
		if a.ValidationPhase != "" && a.ValidationPhase != "initial" && a.ValidationPhase != "correction" {
			a.ValidationPhase = ""
			a.Issues = AddCostGap(a.Issues, CostGapInvalid)
		}
		if a.EndpointKind != "official_global" {
			a.EndpointKind = "custom_or_unverified"
			a.Issues = AddCostGap(a.Issues, CostGapEndpoint)
		}
		if a.TransportMode != "single_send_v1" {
			a.TransportMode = "opaque"
			a.Issues = AddCostGap(a.Issues, CostGapTransport)
			if a.Dispatch != "not_dispatched" {
				c.AttemptCountExact = false
			}
		}
		if a.Dispatch != "not_dispatched" {
			a.Dispatch = "possibly_dispatched"
		}
		switch a.BodyState {
		case "complete", "missing", "read_error", "over_limit", "malformed":
		default:
			a.BodyState = "missing"
			a.Issues = AddCostGap(a.Issues, CostGapInvalid)
		}
		if a.HTTPStatus != nil && (*a.HTTPStatus < 100 || *a.HTTPStatus > 599) {
			a.HTTPStatus = nil
			a.Issues = AddCostGap(a.Issues, CostGapInvalid)
		}
		if a.Dispatch != "not_dispatched" {
			validateObservedCostAttempt(&a)
		}
		c.Attempts = append(c.Attempts, a)
	}
	if c.Truncated || c.DroppedAttempts != 0 {
		c.Truncated = true
		c.Gaps = AddCostGap(c.Gaps, CostGapOverflow)
		c.AttemptCountExact = false
	}
	if c.AttemptsObserved < uint64(len(c.Attempts)) {
		c.AttemptsObserved = uint64(len(c.Attempts))
		c.Gaps = AddCostGap(c.Gaps, CostGapInvalid)
	}
	if c.AttemptsObserved != SaturatingCostAdd(uint64(len(c.Attempts)), c.DroppedAttempts) {
		c.Gaps = AddCostGap(c.Gaps, CostGapMissing)
	}
	if len(c.Gaps) != 0 {
		c.AttemptCountExact = false
	}
	for len(c.Attempts) > 0 {
		b, _ := json.Marshal(c)
		if len(b) <= MaxRequestCostEvidenceBytes-RequestCostReservedBytes {
			break
		}
		c.Attempts = c.Attempts[:len(c.Attempts)-1]
		c.Truncated = true
		c.DroppedAttempts = SaturatingCostAdd(c.DroppedAttempts, 1)
		c.Gaps = AddCostGap(c.Gaps, CostGapOverflow)
		c.AttemptCountExact = false
	}
	return CloneRequestCostEvidence(&c)
}

func validateObservedCostAttempt(a *RequestCostAttempt) {
	if a.BodyState != "complete" || a.HTTPStatus == nil || *a.HTTPStatus < 200 || *a.HTTPStatus > 299 {
		a.Issues = AddCostGap(a.Issues, CostGapDispatch)
	}
	if a.ProviderResponseID == nil || a.ObservedModel == nil || a.ObservedServiceTier == nil {
		a.Issues = AddCostGap(a.Issues, CostGapIdentity)
	}
	if a.ResponseStatus == nil || (*a.ResponseStatus != "completed" && *a.ResponseStatus != "incomplete") {
		a.Issues = AddCostGap(a.Issues, CostGapResponse)
	}
	if a.ObservedServiceTier != nil {
		switch *a.ObservedServiceTier {
		case "default", "standard", "priority", "fast", "flex", "scale":
		default:
			a.Issues = AddCostGap(a.Issues, CostGapIdentity)
		}
	}
	u := a.Usage
	if u == nil || u.InputTokens == nil || u.OutputTokens == nil || u.CacheReadTokens == nil || u.CacheWriteTokens == nil {
		a.Issues = AddCostGap(a.Issues, CostGapUsage)
		return
	}
	in, out, read, write := *u.InputTokens, *u.OutputTokens, *u.CacheReadTokens, *u.CacheWriteTokens
	if in < 0 || out < 0 || read < 0 || write < 0 || read > in || write > in-read {
		a.Issues = AddCostGap(a.Issues, CostGapUsage)
	}
	if u.TotalTokens != nil && (in < 0 || out < 0 || in > math.MaxInt64-out || *u.TotalTokens != in+out) {
		a.Issues = AddCostGap(a.Issues, CostGapUsage)
	}
}

// RequestCostEvidenceBytes supplies fixed-order, map-free v1 digest bytes.
func RequestCostEvidenceBytes(e *RequestCostEvidence) ([]byte, error) {
	return json.Marshal(NormalizeRequestCostEvidence(e))
}

// RequestCostEvidenceDigest binds the full scoped raw evidence, excluding prices.
func RequestCostEvidenceDigest(e *RequestCostEvidence) (string, error) {
	b, err := RequestCostEvidenceBytes(e)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = h.Write([]byte("cr-request-cost-evidence/v1\n"))
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DecodeRequestCostEvidence isolates malformed/unsupported optional telemetry.
func DecodeRequestCostEvidence(data []byte) *RequestCostEvidence {
	if len(data) == 0 || string(data) == "null" {
		return nil
	}
	if len(data) > MaxRequestCostEvidenceBytes {
		return UnknownRequestCostEvidence(CostGapOverflow)
	}
	var e RequestCostEvidence
	if !RequestCostJSONValid(data) || json.Unmarshal(data, &e) != nil {
		return UnknownRequestCostEvidence(CostGapInvalid)
	}
	if len(e.Attempts) > MaxRequestCostAttempts || len(e.Gaps) > MaxRequestCostGaps {
		return UnknownRequestCostEvidence(CostGapOverflow)
	}
	for _, a := range e.Attempts {
		if len(a.Issues) > MaxRequestCostGaps {
			return UnknownRequestCostEvidence(CostGapOverflow)
		}
	}
	return NormalizeRequestCostEvidence(&e)
}

type requestCostCollector struct {
	evidence *RequestCostEvidence
	ordinal  uint64
}

func newRequestCostCollector(adapter Adapter) *requestCostCollector {
	if RequestCostSource(adapter) == "" {
		return nil
	}
	return &requestCostCollector{evidence: &RequestCostEvidence{Version: 1, Source: "openai_responses", Scope: "invocation", AttemptCountExact: true, Gaps: []CostEvidenceGap{}, Attempts: []RequestCostAttempt{}}}
}
func (c *requestCostCollector) snapshot() *RequestCostEvidence {
	if c == nil {
		return nil
	}
	return NormalizeRequestCostEvidence(c.evidence)
}
func (c *requestCostCollector) observe(resp Response, phase string, attempt uint32) {
	if c == nil {
		return
	}
	c.ordinal = SaturatingCostAdd(c.ordinal, 1)
	c.evidence.AttemptsObserved = SaturatingCostAdd(c.evidence.AttemptsObserved, 1)
	e := NormalizeRequestCostEvidence(resp.RequestCostEvidence)
	a := RequestCostAttempt{RuntimeID: "openai-api-key", TransportMode: "opaque", EndpointKind: "custom_or_unverified", Dispatch: "possibly_dispatched", BodyState: "missing", Issues: []CostEvidenceGap{CostGapMissing}}
	if e != nil && len(e.Attempts) == 1 && e.AttemptsObserved == 1 {
		a = e.Attempts[0]
	} else {
		c.evidence.Gaps = AddCostGap(c.evidence.Gaps, CostGapMissing)
		c.evidence.AttemptCountExact = false
	}
	if e != nil {
		for _, g := range e.Gaps {
			c.evidence.Gaps = AddCostGap(c.evidence.Gaps, g)
		}
		c.evidence.AttemptCountExact = c.evidence.AttemptCountExact && e.AttemptCountExact
	}
	a.ValidationPhase = phase
	a.AdapterAttempt = attempt
	if c.ordinal <= math.MaxUint32 {
		a.Ordinal = uint32(c.ordinal)
	} else {
		c.evidence.Gaps = AddCostGap(c.evidence.Gaps, CostGapOverflow)
	}
	if len(c.evidence.Attempts) < MaxRequestCostAttempts && !c.evidence.Truncated {
		c.evidence.Attempts = append(c.evidence.Attempts, a)
	} else {
		c.evidence.Truncated = true
		c.evidence.DroppedAttempts = SaturatingCostAdd(c.evidence.DroppedAttempts, 1)
	}
	c.evidence = NormalizeRequestCostEvidence(c.evidence)
}

func nonDispatchedCostResponse(adapter Adapter, req Request) Response {
	if RequestCostSource(adapter) == "" {
		return Response{}
	}
	attempt := RequestCostAttempt{RuntimeID: "openai-api-key", EndpointKind: "custom_or_unverified", TransportMode: "opaque", Dispatch: "not_dispatched", BodyState: "missing", RequestedModel: req.Model}
	if req.Fast {
		tier := "fast"
		attempt.RequestedServiceTier = &tier
	}
	return Response{RequestCostEvidence: NormalizeRequestCostEvidence(&RequestCostEvidence{Version: 1, Source: "openai_responses", Scope: "invocation", AttemptsObserved: 1, AttemptCountExact: true, Attempts: []RequestCostAttempt{attempt}})}
}

// RequestCostJSONValid rejects duplicate keys and excessive nesting before a
// bounded telemetry envelope is decoded. Unknown unique fields remain allowed.
func RequestCostJSONValid(data []byte) bool {
	if !CostJSONUnicodeValid(data) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if !costJSONValue(d, 0) {
		return false
	}
	_, err := d.Token()
	return err == io.EOF
}
func costJSONValue(d *json.Decoder, depth int) bool {
	if depth > 64 {
		return false
	}
	token, err := d.Token()
	if err != nil {
		return false
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return true
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			t, err := d.Token()
			if err != nil {
				return false
			}
			key, ok := t.(string)
			if !ok {
				return false
			}
			folded := costKeyFold(key)
			if seen[folded] {
				return false
			}
			seen[folded] = true
			if !costJSONValue(d, depth+1) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case '[':
		for d.More() {
			if !costJSONValue(d, depth+1) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	default:
		return false
	}
}

// CostJSONUnicodeValid prevents encoding/json from turning invalid UTF-8 or
// unpaired surrogate escapes into a different, apparently valid identity. It
// does not replace JSON syntax validation and does not unescape provider data.
func CostJSONUnicodeValid(data []byte) bool {
	if !utf8.Valid(data) {
		return false
	}
	inString := false
	for i := 0; i < len(data); i++ {
		if data[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || data[i] != '\\' {
			continue
		}
		i++
		if i >= len(data) {
			return false
		}
		if data[i] != 'u' {
			continue
		}
		if i+4 >= len(data) {
			return false
		}
		value, err := strconv.ParseUint(string(data[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if value >= 0xdc00 && value <= 0xdfff {
			return false
		}
		if value >= 0xd800 && value <= 0xdbff {
			if i+6 >= len(data) || data[i+1] != '\\' || data[i+2] != 'u' {
				return false
			}
			low, err := strconv.ParseUint(string(data[i+3:i+7]), 16, 16)
			if err != nil || low < 0xdc00 || low > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

// encoding/json matches tagged fields with Unicode simple-fold semantics.
// Use the same equivalence classes so alternate casing cannot become a second
// spelling of one field and silently overwrite it during typed decoding.
func costKeyFold(key string) string {
	var out strings.Builder
	for _, r := range key {
		least := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < least {
				least = next
			}
		}
		out.WriteRune(least)
	}
	return out.String()
}
