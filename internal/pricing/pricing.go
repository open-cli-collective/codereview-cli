// Package pricing derives an approximate USD cost from one model catalog.
// A catalog row is only estimated when it has one context-independent rate;
// context-banded rows remain visible through `cr catalog show` but are not
// applied without reliable context metadata.
package pricing

import "github.com/open-cli-collective/codereview-cli/internal/modelcatalog"

// TableVersion identifies the bundled public list-price snapshot used by the
// compatibility wrapper. Catalog-aware callers should use EstimateBasis.
const TableVersion = "model-catalog"

const perMillion = 1_000_000.0

// Usage contains the token categories and observed execution speed needed to
// estimate a workstream at public list prices.
type Usage struct {
	TokensIn         *int
	TokensOut        *int
	CacheRead        *int
	CacheCreate5m    *int
	CacheCreate1h    *int
	CacheCreateTotal *int
	Speed            string
}

// EstimateUsageUSD uses the bundled catalog for callers that have no command
// snapshot. Command paths should use EstimateUsageUSDFor.
func EstimateUsageUSD(model string, usage Usage) (float64, bool) {
	catalog, err := modelcatalog.LoadBundled()
	if err != nil {
		return 0, false
	}
	return EstimateUsageUSDFor(catalog, model, usage)
}

// EstimateUsageUSDFor estimates usage against one immutable catalog snapshot.
// Context-banded rows are intentionally unavailable because the current usage
// record does not identify the context band or provider token normalization.
// Request-bound rows remain unavailable even when their context band is all.
func EstimateUsageUSDFor(catalog *modelcatalog.Catalog, model string, usage Usage) (cost float64, ok bool) {
	if catalog == nil {
		return 0, false
	}
	prices := catalog.PricesFor(model, usage.Speed)
	if len(prices) != 1 || prices[0].ContextBand != "all" || prices[0].RequestPricing != nil {
		return 0, false
	}
	price := prices[0]
	if usage.CacheCreateTotal != nil && deref(usage.CacheCreateTotal) != deref(usage.CacheCreate5m)+deref(usage.CacheCreate1h) {
		return 0, false
	}
	if nonzeroWithoutRate(usage.TokensIn, price.Input) || nonzeroWithoutRate(usage.TokensOut, price.Output) || nonzeroWithoutRate(usage.CacheRead, price.CacheRead) || nonzeroWithoutRate(usage.CacheCreate5m, price.CacheWrite5m) || nonzeroWithoutRate(usage.CacheCreate1h, price.CacheWrite1h) {
		return 0, false
	}
	cost = float64(deref(usage.TokensIn))*value(price.Input)/perMillion +
		float64(deref(usage.TokensOut))*value(price.Output)/perMillion +
		float64(deref(usage.CacheRead))*value(price.CacheRead)/perMillion +
		float64(deref(usage.CacheCreate5m))*value(price.CacheWrite5m)/perMillion +
		float64(deref(usage.CacheCreate1h))*value(price.CacheWrite1h)/perMillion
	return cost, true
}

// EstimateBasis identifies the catalog revision used for a computed estimate.
func EstimateBasis(catalog *modelcatalog.Catalog) string {
	if catalog == nil {
		return ""
	}
	return catalog.Revision() + "/pricing"
}

func nonzeroWithoutRate(tokens *int, rate *float64) bool {
	return deref(tokens) != 0 && rate == nil
}

func value(rate *float64) float64 {
	if rate == nil {
		return 0
	}
	return *rate
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
