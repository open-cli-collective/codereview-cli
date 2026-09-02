// Package pricing derives an approximate USD cost for a model's token usage at
// public list prices. It exists so adapters that do not report a cost (e.g.
// subscription auth) can still surface an estimate instead of "unavailable".
//
// Estimates are intentionally best-effort: a model the table does not know
// returns ok=false so callers leave the cost unavailable rather than render a
// wrong number. cr supports anyone's agents and any model, so unknown models
// degrading gracefully is the point — extend the table to price more models.
package pricing

// TableVersion identifies the public list-price snapshot used by estimates.
const TableVersion = "anthropic-public-2026-09-02"

// rate is the USD list price per 1,000,000 tokens for each billable category.
type rate struct {
	in          float64
	out         float64
	cacheRead   float64
	cacheWrite5 float64
	cacheWrite1 float64
}

// rates maps a concrete model id to its list price. Add entries to price more
// models; absent models simply return no estimate.
var rates = map[string]rate{
	"claude-fable-5-1":          {in: 10, out: 50, cacheRead: 0.25, cacheWrite5: 12.5, cacheWrite1: 20},
	"claude-opus-5":             {in: 5, out: 25, cacheRead: 0.5, cacheWrite5: 6.25, cacheWrite1: 10},
	"claude-opus-4-8":           {in: 5, out: 25, cacheRead: 0.5, cacheWrite5: 6.25, cacheWrite1: 10},
	"claude-sonnet-5":           {in: 2, out: 10, cacheRead: 0.2, cacheWrite5: 2.5, cacheWrite1: 4},
	"claude-sonnet-4-6":         {in: 3, out: 15, cacheRead: 0.3, cacheWrite5: 3.75, cacheWrite1: 6},
	"claude-haiku-4-5":          {in: 1, out: 5, cacheRead: 0.1, cacheWrite5: 1.25, cacheWrite1: 2},
	"claude-haiku-4-5-20251001": {in: 1, out: 5, cacheRead: 0.1, cacheWrite5: 1.25, cacheWrite1: 2},
}

var fastRates = map[string]rate{
	"claude-opus-5":   {in: 10, out: 50, cacheRead: 1, cacheWrite5: 12.5, cacheWrite1: 20},
	"claude-opus-4-8": {in: 10, out: 50, cacheRead: 1, cacheWrite5: 12.5, cacheWrite1: 20},
}

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

// EstimateUsageUSD estimates cost for usage whose billable token categories
// are known. A nonzero cache write with unknown TTL cannot be priced exactly.
func EstimateUsageUSD(model string, usage Usage) (cost float64, ok bool) {
	r, known := rates[model]
	if !known {
		return 0, false
	}
	switch usage.Speed {
	case "standard":
	case "fast":
		var fastKnown bool
		r, fastKnown = fastRates[model]
		if !fastKnown {
			return 0, false
		}
	default:
		return 0, false
	}
	if usage.CacheCreateTotal != nil && deref(usage.CacheCreateTotal) != deref(usage.CacheCreate5m)+deref(usage.CacheCreate1h) {
		return 0, false
	}
	cost = float64(deref(usage.TokensIn))*r.in/perMillion +
		float64(deref(usage.TokensOut))*r.out/perMillion +
		float64(deref(usage.CacheRead))*r.cacheRead/perMillion +
		float64(deref(usage.CacheCreate5m))*r.cacheWrite5/perMillion +
		float64(deref(usage.CacheCreate1h))*r.cacheWrite1/perMillion
	return cost, true
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
