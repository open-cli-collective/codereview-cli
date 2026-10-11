package modelcatalog

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

var requestPricingColumns = []string{"runtime_id", "observed_service_tiers", "input_tokens_min_inclusive", "input_tokens_max_exclusive", "rate_application"}

// RequestPricingBinding describes verified whole-request list-price semantics.
// Nil bindings are legacy/display-only. Within a complete binding, minimum is
// required and a nil maximum means unbounded, not unknown.
type RequestPricingBinding struct {
	RuntimeID               string   `json:"runtime_id"`
	ObservedServiceTiers    []string `json:"observed_service_tiers"`
	InputTokensMinInclusive *int64   `json:"input_tokens_min_inclusive"`
	InputTokensMaxExclusive *int64   `json:"input_tokens_max_exclusive"`
	RateApplication         string   `json:"rate_application"`
}

// UnavailableReason explains why no request price could be selected.
type UnavailableReason string

// Request pricing unavailability reasons returned by PriceForRequest.
const (
	UnavailableCatalog        UnavailableReason = "catalog_unavailable"
	UnavailableInput          UnavailableReason = "invalid_input_tokens"
	UnavailableIdentity       UnavailableReason = "missing_observed_identity"
	UnavailableRuntime        UnavailableReason = "unsupported_runtime"
	UnavailablePrice          UnavailableReason = "no_matching_request_price"
	UnavailableAmbiguousPrice UnavailableReason = "ambiguous_request_price"
)

// PriceSelection is a detached row and its exact immutable catalog identity.
// It is not an estimate receipt: endpoint scope, request evidence and complete
// dispatch history must be established independently before any cost is used.
type PriceSelection struct {
	Price                Price  `json:"price"`
	CatalogSchemaVersion int    `json:"catalog_schema_version"`
	CatalogRevision      string `json:"catalog_revision"`
	CatalogDigest        string `json:"catalog_digest"`
	ObservedServiceTier  string `json:"observed_service_tier"`
}

// PriceForRequest selects one absolute-rate row for inclusive input tokens on
// one observed request. Caller must independently establish official global
// endpoint scope and complete request/history evidence. This function does not
// activate an estimator, validate context-window capacity, or normalize aliases,
// service tiers, token categories, requested speed, or aggregate usage.
func (c *Catalog) PriceForRequest(runtimeID, observedModel, observedServiceTier string, inputTokensTotal int64) (PriceSelection, UnavailableReason) {
	if c == nil {
		return PriceSelection{}, UnavailableCatalog
	}
	if inputTokensTotal < 0 {
		return PriceSelection{}, UnavailableInput
	}
	if runtimeID == "" || observedModel == "" || observedServiceTier == "" {
		return PriceSelection{}, UnavailableIdentity
	}
	runtime, ok := c.RuntimeByID(runtimeID)
	if !ok || !supportsRequestPricing(runtime) {
		return PriceSelection{}, UnavailableRuntime
	}
	var selected Price
	matches := 0
	for _, price := range c.pricing {
		binding := price.RequestPricing
		if price.ModelID != observedModel || binding == nil || binding.RuntimeID != runtimeID || binding.RateApplication != "whole_request" || !contains(binding.ObservedServiceTiers, observedServiceTier) {
			continue
		}
		if binding.InputTokensMinInclusive == nil || inputTokensTotal < *binding.InputTokensMinInclusive || (binding.InputTokensMaxExclusive != nil && inputTokensTotal >= *binding.InputTokensMaxExclusive) {
			continue
		}
		selected = price
		matches++
	}
	if matches == 0 {
		return PriceSelection{}, UnavailablePrice
	}
	if matches != 1 {
		return PriceSelection{}, UnavailableAmbiguousPrice
	}
	return PriceSelection{
		Price: clonePrice(selected), CatalogSchemaVersion: c.manifest.SchemaVersion,
		CatalogRevision: c.Revision(), CatalogDigest: c.Digest(), ObservedServiceTier: observedServiceTier,
	}, ""
}

func supportsRequestPricing(runtime Runtime) bool {
	return runtime.ID == "openai-api-key" && runtime.Provider == "openai" && runtime.Auth == "api_key" && runtime.Harness == "openai_api" && runtime.Adapter == "openai_api"
}

func absentRequestCell(value string) bool { return value == "" || strings.EqualFold(value, "null") }

func parseRequestPricing(row map[string]string, speed string) (*RequestPricingBinding, error) {
	absent := 0
	for _, column := range requestPricingColumns {
		if absentRequestCell(row[column]) {
			absent++
		}
	}
	if absent == len(requestPricingColumns) {
		return nil, nil
	}
	for _, column := range []string{"runtime_id", "observed_service_tiers", "input_tokens_min_inclusive", "rate_application"} {
		if absentRequestCell(row[column]) {
			return nil, fmt.Errorf("incomplete request pricing: %s is required", column)
		}
	}
	if row["rate_application"] != "whole_request" {
		return nil, fmt.Errorf("request rate_application %q is unsupported", row["rate_application"])
	}
	minimum, err := parseInputBound(row["input_tokens_min_inclusive"])
	if err != nil {
		return nil, fmt.Errorf("input_tokens_min_inclusive: %w", err)
	}
	binding := &RequestPricingBinding{RuntimeID: row["runtime_id"], InputTokensMinInclusive: &minimum, RateApplication: "whole_request"}
	if !absentRequestCell(row["input_tokens_max_exclusive"]) {
		maximum, err := parseInputBound(row["input_tokens_max_exclusive"])
		if err != nil {
			return nil, fmt.Errorf("input_tokens_max_exclusive: %w", err)
		}
		if maximum <= minimum {
			return nil, fmt.Errorf("input_tokens_max_exclusive must exceed minimum")
		}
		binding.InputTokensMaxExclusive = &maximum
	}
	seen := map[string]bool{}
	for _, tier := range strings.Split(row["observed_service_tiers"], "|") {
		if seen[tier] || !tierMatchesSpeed(tier, speed) {
			return nil, fmt.Errorf("observed service tier %q is duplicate or incompatible with speed %q", tier, speed)
		}
		seen[tier] = true
		binding.ObservedServiceTiers = append(binding.ObservedServiceTiers, tier)
	}
	return binding, nil
}

func parseInputBound(raw string) (int64, error) {
	if raw == "" {
		return 0, fmt.Errorf("empty input bound")
	}
	for _, digit := range raw {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("input bound %q must contain only decimal digits", raw)
		}
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("input bound %q exceeds int64", raw)
	}
	return value, nil
}

func tierMatchesSpeed(tier, speed string) bool {
	switch speed {
	case "standard":
		return tier == "default"
	case "fast":
		return tier == "fast" || tier == "priority"
	case "ultrafast":
		return tier == "ultrafast"
	default:
		return false
	}
}

func validateRequestPricing(runtimes map[string]Runtime, models map[string]map[string]Model, prices []Price) error {
	groups := map[string][]Price{}
	identities := map[string][]Price{}
	for _, price := range prices {
		key := price.ModelID + "\x00" + price.Speed
		groups[key] = append(groups[key], price)
		binding := price.RequestPricing
		if binding == nil {
			continue
		}
		runtime, exists := runtimes[binding.RuntimeID]
		if !exists || !supportsRequestPricing(runtime) {
			return fmt.Errorf("model catalog: price %q has unsupported request runtime %q", key, binding.RuntimeID)
		}
		if _, exists := models[binding.RuntimeID][price.ModelID]; !exists {
			return fmt.Errorf("model catalog: request price %q model is not offered by runtime %q", key, binding.RuntimeID)
		}
		for _, tier := range binding.ObservedServiceTiers {
			identity := binding.RuntimeID + "\x00" + price.ModelID + "\x00" + tier
			identities[identity] = append(identities[identity], price)
		}
	}
	for key, group := range groups {
		enabled := 0
		for _, price := range group {
			if price.RequestPricing != nil {
				enabled++
			}
		}
		if enabled == 0 {
			continue
		}
		if enabled != len(group) {
			return fmt.Errorf("model catalog: request price group %q is partially enabled", key)
		}
		if len(group) == 1 && group[0].ContextBand == "all" {
			binding := group[0].RequestPricing
			if *binding.InputTokensMinInclusive != 0 || binding.InputTokensMaxExclusive != nil {
				return fmt.Errorf("model catalog: request price group %q all must span [0,unbounded)", key)
			}
			continue
		}
		if len(group) != 2 {
			return fmt.Errorf("model catalog: request price group %q needs one all row or a short/long pair", key)
		}
		var short, long *RequestPricingBinding
		for _, price := range group {
			switch price.ContextBand {
			case "short":
				short = price.RequestPricing
			case "long":
				long = price.RequestPricing
			}
		}
		if short == nil || long == nil || short.RuntimeID != long.RuntimeID || !sameTierSet(short.ObservedServiceTiers, long.ObservedServiceTiers) || *short.InputTokensMinInclusive != 0 || short.InputTokensMaxExclusive == nil || *short.InputTokensMaxExclusive <= 0 || long.InputTokensMaxExclusive != nil || *long.InputTokensMinInclusive != *short.InputTokensMaxExclusive {
			return fmt.Errorf("model catalog: request price group %q needs matching tiers and contiguous [0,B),[B,unbounded) bounds", key)
		}
	}
	for identity, group := range identities {
		sort.Slice(group, func(i, j int) bool {
			return *group[i].RequestPricing.InputTokensMinInclusive < *group[j].RequestPricing.InputTokensMinInclusive
		})
		for i := 1; i < len(group); i++ {
			previous := group[i-1].RequestPricing
			if previous.InputTokensMaxExclusive == nil || *group[i].RequestPricing.InputTokensMinInclusive < *previous.InputTokensMaxExclusive {
				return fmt.Errorf("model catalog: overlapping request price identity %q", identity)
			}
		}
	}
	return nil
}

func sameTierSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, value := range a {
		if !contains(b, value) {
			return false
		}
	}
	return true
}

func cloneRequestPricing(value *RequestPricingBinding) *RequestPricingBinding {
	if value == nil {
		return nil
	}
	clone := *value
	clone.ObservedServiceTiers = append([]string(nil), value.ObservedServiceTiers...)
	clone.InputTokensMinInclusive = cloneInputBound(value.InputTokensMinInclusive)
	clone.InputTokensMaxExclusive = cloneInputBound(value.InputTokensMaxExclusive)
	return &clone
}

func cloneInputBound(value *int64) *int64 {
	if value == nil {
		return nil
	}
	clone := *value
	return &clone
}
