package pricing

import (
	"math"
	"testing"
)

func p(v int) *int { return &v }

func TestEstimateUSDKnownModel(t *testing.T) {
	// sonnet: in $3/Mtok, out $15/Mtok, cache read 0.1x in ($0.30), cache write 1.25x in ($3.75).
	// 1M of each token bucket → 3 + 15 + 0.30 + 3.75 = 22.05.
	cost, ok := EstimateUsageUSD("claude-sonnet-4-6", Usage{
		TokensIn: p(1_000_000), TokensOut: p(1_000_000), CacheRead: p(1_000_000), CacheCreate5m: p(1_000_000), Speed: "standard",
	})
	if !ok {
		t.Fatal("expected ok for a priced model")
	}
	if want := 22.05; math.Abs(cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v", cost, want)
	}
}

func TestEstimateUSDUnknownModelDegrades(t *testing.T) {
	if _, ok := EstimateUsageUSD("some-other-vendor/model-x", Usage{TokensIn: p(1000), TokensOut: p(1000)}); ok {
		t.Fatal("expected ok=false for an unpriced model (any agent's model degrades gracefully)")
	}
}

func TestEstimateUsageUSDRejectsMissingSpeed(t *testing.T) {
	if _, ok := EstimateUsageUSD("claude-opus-5", Usage{TokensIn: p(1_000_000)}); ok {
		t.Fatal("expected usage without an observed speed tier to leave cost unavailable")
	}
}

func TestEstimateUSDNilTokensAreZero(t *testing.T) {
	cost, ok := EstimateUsageUSD("claude-opus-4-8", Usage{Speed: "standard"})
	if !ok || cost != 0 {
		t.Fatalf("nil tokens: cost=%v ok=%v, want 0,true", cost, ok)
	}
}

func TestEstimateUSDCurrentClaudeRates(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		input     int
		output    int
		cacheRead int
		want      float64
	}{
		{
			name:   "sonnet 5 permanent price",
			model:  "claude-sonnet-5",
			input:  1_000_000,
			output: 1_000_000,
			want:   12,
		},
		{
			name:      "fable 5.1 exceptional cache read price",
			model:     "claude-fable-5-1",
			cacheRead: 1_000_000,
			want:      0.25,
		},
		{
			name:   "canonical dated haiku id",
			model:  "claude-haiku-4-5-20251001",
			input:  1_000_000,
			output: 1_000_000,
			want:   6,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cost, ok := EstimateUsageUSD(tt.model, Usage{TokensIn: p(tt.input), TokensOut: p(tt.output), CacheRead: p(tt.cacheRead), Speed: "standard"})
			if !ok {
				t.Fatal("expected current Claude model to be priced")
			}
			if math.Abs(cost-tt.want) > 1e-9 {
				t.Fatalf("cost = %v, want %v", cost, tt.want)
			}
		})
	}
}

func TestEstimateUsageUSDPricesCacheTTLAndObservedSpeed(t *testing.T) {
	usage := Usage{
		TokensIn:      p(1_000_000),
		TokensOut:     p(1_000_000),
		CacheRead:     p(1_000_000),
		CacheCreate5m: p(1_000_000),
		CacheCreate1h: p(1_000_000),
		Speed:         "standard",
	}

	standard, ok := EstimateUsageUSD("claude-opus-5", usage)
	if !ok {
		t.Fatal("expected standard Opus usage to be priced")
	}
	if want := 46.75; math.Abs(standard-want) > 1e-9 {
		t.Fatalf("standard cost = %v, want %v", standard, want)
	}

	usage.Speed = "fast"
	fast, ok := EstimateUsageUSD("claude-opus-5", usage)
	if !ok {
		t.Fatal("expected fast Opus usage to be priced")
	}
	if want := 93.5; math.Abs(fast-want) > 1e-9 {
		t.Fatalf("fast cost = %v, want %v", fast, want)
	}
}

func TestEstimateUsageUSDPricesEachSonnetBucketIndependently(t *testing.T) {
	tests := []struct {
		name  string
		usage Usage
		want  float64
	}{
		{name: "input", usage: Usage{TokensIn: p(1_000_000), Speed: "standard"}, want: 2},
		{name: "output", usage: Usage{TokensOut: p(1_000_000), Speed: "standard"}, want: 10},
		{name: "cache read", usage: Usage{CacheRead: p(1_000_000), Speed: "standard"}, want: 0.2},
		{name: "five minute cache write", usage: Usage{CacheCreate5m: p(1_000_000), Speed: "standard"}, want: 2.5},
		{name: "one hour cache write", usage: Usage{CacheCreate1h: p(1_000_000), Speed: "standard"}, want: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cost, ok := EstimateUsageUSD("claude-sonnet-5", tt.usage)
			if !ok || math.Abs(cost-tt.want) > 1e-9 {
				t.Fatalf("cost = %v, ok = %v, want %v,true", cost, ok, tt.want)
			}
		})
	}
}

func TestEstimateUsageUSDRejectsUnknownCacheCreateTTL(t *testing.T) {
	usage := Usage{
		TokensIn:         p(1_000_000),
		CacheCreateTotal: p(1),
		Speed:            "standard",
	}
	if _, ok := EstimateUsageUSD("claude-sonnet-5", usage); ok {
		t.Fatal("expected unknown nonzero cache-create TTL to leave cost unavailable")
	}
}

func TestEstimateUsageUSDRejectsMismatchedCacheCreateTotal(t *testing.T) {
	usage := Usage{
		CacheCreateTotal: p(10),
		CacheCreate5m:    p(4),
		CacheCreate1h:    p(5),
		Speed:            "standard",
	}
	if _, ok := EstimateUsageUSD("claude-sonnet-5", usage); ok {
		t.Fatal("expected mismatched cache-create total and TTL buckets to leave cost unavailable")
	}
}

func TestEstimateUsageUSDAllowsZeroUnknownCacheCreate(t *testing.T) {
	usage := Usage{
		TokensIn:         p(1_000_000),
		CacheCreateTotal: p(0),
		Speed:            "standard",
	}
	cost, ok := EstimateUsageUSD("claude-sonnet-5", usage)
	if !ok || math.Abs(cost-2) > 1e-9 {
		t.Fatalf("cost = %v, ok = %v, want 2,true", cost, ok)
	}
}

func TestEstimateUsageUSDRejectsMixedOrUnknownSpeed(t *testing.T) {
	for _, speed := range []string{"mixed", "unknown"} {
		t.Run(speed, func(t *testing.T) {
			if _, ok := EstimateUsageUSD("claude-opus-5", Usage{TokensIn: p(1_000_000), Speed: speed}); ok {
				t.Fatalf("speed %q should leave cost unavailable", speed)
			}
		})
	}
}
