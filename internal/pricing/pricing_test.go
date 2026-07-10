package pricing

import (
	"math"
	"testing"
)

func p(v int) *int { return &v }

func TestEstimateUSDKnownModel(t *testing.T) {
	// sonnet: in $3/Mtok, out $15/Mtok, cache read 0.1x in ($0.30), cache write 1.25x in ($3.75).
	// 1M of each token bucket → 3 + 15 + 0.30 + 3.75 = 22.05.
	cost, ok := EstimateUSD("claude-sonnet-4-6", p(1_000_000), p(1_000_000), p(1_000_000), p(1_000_000))
	if !ok {
		t.Fatal("expected ok for a priced model")
	}
	if want := 22.05; math.Abs(cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestEstimateUSDUnknownModelDegrades(t *testing.T) {
	if _, ok := EstimateUSD("some-other-vendor/model-x", p(1000), p(1000), nil, nil); ok {
		t.Fatal("expected ok=false for an unpriced model (any agent's model degrades gracefully)")
	}
}

func TestEstimateUSDNilTokensAreZero(t *testing.T) {
	cost, ok := EstimateUSD("claude-opus-4-8", nil, nil, nil, nil)
	if !ok || cost != 0 {
		t.Fatalf("nil tokens: cost=%v ok=%v, want 0,true", cost, ok)
	}
}
