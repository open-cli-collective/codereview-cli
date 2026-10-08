package pricing

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/modelcatalog"
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

func TestEstimateUsageUSDPricesOpus55ReducedCacheReadAndFastMode(t *testing.T) {
	// Opus 5.5 reads cache at 0.05x input, not the usual 0.1x, and fast mode
	// doubles every bucket: 4 + 20 + 0.20 + 5 + 8 = 37.2 standard.
	usage := Usage{
		TokensIn:      p(1_000_000),
		TokensOut:     p(1_000_000),
		CacheRead:     p(1_000_000),
		CacheCreate5m: p(1_000_000),
		CacheCreate1h: p(1_000_000),
		Speed:         "standard",
	}

	standard, ok := EstimateUsageUSD("claude-opus-5-5", usage)
	if !ok {
		t.Fatal("expected standard Opus 5.5 usage to be priced")
	}
	if want := 37.2; math.Abs(standard-want) > 1e-9 {
		t.Fatalf("standard cost = %v, want %v", standard, want)
	}

	usage.Speed = "fast"
	fast, ok := EstimateUsageUSD("claude-opus-5-5", usage)
	if !ok {
		t.Fatal("expected fast Opus 5.5 usage to be priced")
	}
	if want := 74.4; math.Abs(fast-want) > 1e-9 {
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

func TestEstimateUsageUSDForUsesSelectedCatalogRatesAndBasis(t *testing.T) {
	catalog := customPricingCatalog(t)
	cost, ok := EstimateUsageUSDFor(catalog, "pricing-test-model", Usage{
		TokensIn:      p(1_000_000),
		TokensOut:     p(1_000_000),
		CacheRead:     p(1_000_000),
		CacheCreate5m: p(1_000_000),
		CacheCreate1h: p(1_000_000),
		Speed:         "standard",
	})
	if !ok {
		t.Fatal("expected selected catalog rate to be usable")
	}
	if want := 26.7; math.Abs(cost-want) > 1e-9 {
		t.Fatalf("cost = %v, want %v from selected catalog", cost, want)
	}
	if want := "pricing-test-revision/pricing"; EstimateBasis(catalog) != want {
		t.Fatalf("EstimateBasis = %q, want %q", EstimateBasis(catalog), want)
	}
}

func TestEstimateUsageUSDForLeavesUnknownRatesAndContextBandsUnavailable(t *testing.T) {
	catalog, err := modelcatalog.LoadBundled()
	if err != nil {
		t.Fatalf("LoadBundled: %v", err)
	}
	if _, ok := EstimateUsageUSDFor(catalog, "gpt-5.5", Usage{CacheCreate5m: p(1), Speed: "standard"}); ok {
		t.Fatal("unknown cache-write rate should remain unavailable")
	}
	if _, ok := EstimateUsageUSDFor(catalog, "gpt-6.1-sol", Usage{TokensIn: p(1_000_000), Speed: "standard"}); ok {
		t.Fatal("context-banded rate should remain unavailable without context metadata")
	}
}

func customPricingCatalog(t *testing.T) *modelcatalog.Catalog {
	t.Helper()
	return customPricingCatalogForSchema(t, 1, false)
}

func customPricingCatalogForSchema(t *testing.T, schema int, requestBound bool) *modelcatalog.Catalog {
	t.Helper()
	catalog, err := modelcatalog.LoadPath(customPricingCatalogPathForSchema(t, schema, requestBound))
	if err != nil {
		t.Fatalf("LoadPath: %v", err)
	}
	return catalog
}

func customPricingCatalogPathForSchema(t *testing.T, schema int, requestBound bool) string {
	t.Helper()
	_, testFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	source := t.TempDir()
	dataDir := filepath.Join(filepath.Dir(testFile), "..", "modelcatalog", "data")
	if schema == 1 {
		dataDir = filepath.Join(filepath.Dir(testFile), "..", "modelcatalog", "testdata", "v1")
	}
	for _, name := range []string{"manifest.json", "runtimes.csv", "models.csv", "defaults.csv", "pricing.csv"} {
		body, err := os.ReadFile(filepath.Join(dataDir, name)) // #nosec G304 -- dataDir is the repository's bundled fixture.
		if err != nil {
			t.Fatalf("read catalog %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(source, name), body, 0o600); err != nil { // #nosec G703 -- source is under t.TempDir.
			t.Fatalf("write catalog %s: %v", name, err)
		}
	}
	appendCatalogRow(t, source, "models.csv", "openai-api-key,pricing-test-model,low,,,,https://example.invalid,2026-10-03\n")
	priceRow := "pricing-test-model,standard,all,7,11,0.7,,3,5,https://example.invalid,2026-10-03"
	if schema == 2 {
		if requestBound {
			priceRow += ",openai-api-key,default,0,,whole_request"
		} else {
			priceRow += ",,,,,"
		}
	}
	appendCatalogRow(t, source, "pricing.csv", priceRow+"\n")
	manifestPath := filepath.Join(source, "manifest.json")
	manifestBody, err := os.ReadFile(manifestPath) // #nosec G304 -- manifestPath is under t.TempDir.
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest modelcatalog.Manifest
	if err := json.Unmarshal(manifestBody, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	manifest.Revision = "pricing-test-revision"
	for _, name := range []string{"models.csv", "pricing.csv"} {
		body, err := os.ReadFile(filepath.Join(source, name)) // #nosec G304 -- source is under t.TempDir.
		if err != nil {
			t.Fatalf("read changed %s: %v", name, err)
		}
		digest := sha256.Sum256(body)
		manifest.Files[name] = fmt.Sprintf("%x", digest)
	}
	manifestBody, err = json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	if err := os.WriteFile(manifestPath, append(manifestBody, '\n'), 0o600); err != nil { // #nosec G703 -- manifestPath is under t.TempDir.
		t.Fatalf("write manifest: %v", err)
	}
	return source
}

func appendCatalogRow(t *testing.T, dir, name, row string) {
	t.Helper()
	path := filepath.Join(dir, name)
	body, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600) // #nosec G304 -- path is under t.TempDir.
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer body.Close()
	if _, err := body.WriteString(row); err != nil {
		t.Fatalf("append %s: %v", name, err)
	}
}

func TestLegacyEstimateRejectsRequestBoundAllAndPreservesV1(t *testing.T) {
	for _, tc := range []struct {
		schema int
		bound  bool
	}{{1, false}, {2, false}, {2, true}} {
		catalog := customPricingCatalogForSchema(t, tc.schema, tc.bound)
		if _, ok := catalog.PriceFor("pricing-test-model", "standard"); ok == tc.bound {
			t.Fatalf("schema=%d bound=%t PriceFor availability=%t", tc.schema, tc.bound, ok)
		}
		for _, usage := range []Usage{{Speed: "standard"}, {TokensIn: p(1_000_000), Speed: "standard"}} {
			cost, ok := EstimateUsageUSDFor(catalog, "pricing-test-model", usage)
			if ok == tc.bound {
				t.Fatalf("schema=%d bound=%t legacy availability=%t", tc.schema, tc.bound, ok)
			}
			if ok && cost != float64(deref(usage.TokensIn))*7/perMillion {
				t.Fatal("legacy price changed")
			}
		}
		if tc.bound {
			if _, reason := catalog.PriceForRequest("openai-api-key", "pricing-test-model", "default", 1); reason != "" {
				t.Fatalf("request-bound all row unavailable to exact selector: %q", reason)
			}
		}
	}
}

func TestLegacyEstimateStillRejectsEveryOpenAIContextBand(t *testing.T) {
	catalog, err := modelcatalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	for _, price := range catalog.Pricing() {
		if price.ContextBand == "all" {
			continue
		}
		if _, ok := EstimateUsageUSDFor(catalog, price.ModelID, Usage{TokensIn: p(1), Speed: price.Speed}); ok {
			t.Fatalf("legacy estimator activated for %s/%s", price.ModelID, price.Speed)
		}
	}
}

func TestPaddedV2HeadersCannotDemoteRequestBoundAllToLegacy(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		indexes               []int
		quoted, finiteMaximum bool
	}{
		{name: "all five", indexes: []int{11, 12, 13, 14, 15}},
		{name: "all five quoted", indexes: []int{11, 12, 13, 14, 15}, quoted: true},
		{name: "partial", indexes: []int{11, 12}},
		{name: "optional maximum only", indexes: []int{14}, finiteMaximum: true},
		{name: "optional maximum quoted", indexes: []int{14}, quoted: true, finiteMaximum: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := customPricingCatalogPathForSchema(t, 2, true)
			catalog, err := modelcatalog.LoadPath(source)
			if err != nil {
				t.Fatal(err)
			}
			usage := Usage{TokensIn: p(1_000_000), Speed: "standard"}
			if _, ok := catalog.PriceFor("pricing-test-model", "standard"); ok {
				t.Fatal("canonical bound all row entered PriceFor")
			}
			if _, ok := EstimateUsageUSDFor(catalog, "pricing-test-model", usage); ok {
				t.Fatal("canonical bound all row entered legacy estimator")
			}
			sourceRoot, err := os.OpenRoot(source)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sourceRoot.Close() })
			body, err := sourceRoot.ReadFile("pricing.csv")
			if err != nil {
				t.Fatal(err)
			}
			parts := strings.SplitN(string(body), "\n", 2)
			headers := strings.Split(parts[0], ",")
			for _, index := range tc.indexes {
				headers[index] += " "
				if tc.quoted {
					headers[index] = "\"" + headers[index] + "\""
				}
			}
			if tc.finiteMaximum {
				// A populated invalid finite bound on all must not become unbounded
				// merely because its header key was padded.
				parts[1] = strings.Replace(parts[1], ",openai-api-key,default,0,,whole_request", ",openai-api-key,default,0,100,whole_request", 1)
			}
			body = []byte(strings.Join(headers, ",") + "\n" + parts[1])
			if err := sourceRoot.WriteFile("pricing.csv", body, 0o600); err != nil {
				t.Fatal(err)
			}
			manifest := catalog.Manifest()
			manifest.Files["pricing.csv"] = fmt.Sprintf("%x", sha256.Sum256(body))
			manifestBody, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := sourceRoot.WriteFile("manifest.json", manifestBody, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := sourceRoot.Close(); err != nil {
				t.Fatal(err)
			}
			malformed, err := modelcatalog.LoadPath(source)
			if err == nil {
				_, priceOK := malformed.PriceFor("pricing-test-model", "standard")
				_, estimateOK := EstimateUsageUSDFor(malformed, "pricing-test-model", usage)
				t.Fatalf("padded header accepted: PriceFor=%t legacy estimate=%t", priceOK, estimateOK)
			}
			if !strings.Contains(err.Error(), "header") {
				t.Fatalf("error = %v, want exact header rejection", err)
			}
		})
	}
}
