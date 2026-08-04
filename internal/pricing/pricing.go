// Package pricing derives an approximate USD cost for a model's token usage at
// public list prices. It exists so adapters that do not report a cost (e.g.
// subscription auth) can still surface an estimate instead of "unavailable".
//
// Estimates are intentionally best-effort: a model the table does not know
// returns ok=false so callers leave the cost unavailable rather than render a
// wrong number. cr supports anyone's agents and any model, so unknown models
// degrading gracefully is the point — extend the table to price more models.
package pricing

// rate is USD list price per 1,000,000 tokens for input and output. Cache
// pricing is derived from the input rate following Anthropic prompt-caching:
// cache reads cost 0.1x input, cache writes cost 1.25x input.
type rate struct {
	in  float64
	out float64
}

// rates maps a concrete model id to its list price. Add entries to price more
// models; absent models simply return no estimate.
var rates = map[string]rate{
	"claude-opus-5":     {in: 5, out: 25},
	"claude-opus-4-8":   {in: 5, out: 25},
	"claude-sonnet-5":   {in: 3, out: 15},
	"claude-sonnet-4-6": {in: 3, out: 15},
	"claude-haiku-4-5":  {in: 1, out: 5},
}

const cacheReadFactor = 0.10
const cacheWriteFactor = 1.25
const perMillion = 1_000_000.0

// EstimateUSD returns an approximate cost for the given token counts at the
// model's public list price. ok is false when the model is not priced, so the
// caller can leave cost unavailable. nil token pointers are treated as zero.
func EstimateUSD(model string, tokensIn, tokensOut, cacheRead, cacheCreate *int) (cost float64, ok bool) {
	r, known := rates[model]
	if !known {
		return 0, false
	}
	cost = float64(deref(tokensIn))*r.in/perMillion +
		float64(deref(tokensOut))*r.out/perMillion +
		float64(deref(cacheRead))*(r.in*cacheReadFactor)/perMillion +
		float64(deref(cacheCreate))*(r.in*cacheWriteFactor)/perMillion
	return cost, true
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
