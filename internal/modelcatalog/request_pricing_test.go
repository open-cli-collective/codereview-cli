package modelcatalog

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func requestCatalogFiles(t *testing.T) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range requiredFiles {
		body, err := bundledFS.ReadFile("data/" + name)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = body
	}
	return files
}

func pricingRecords(t *testing.T, files map[string][]byte) [][]string {
	t.Helper()
	rows, err := csv.NewReader(bytes.NewReader(files["pricing.csv"])).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func replacePricingRecords(t *testing.T, files map[string][]byte, rows [][]string) {
	t.Helper()
	var body bytes.Buffer
	writer := csv.NewWriter(&body)
	if err := writer.WriteAll(rows); err != nil {
		t.Fatal(err)
	}
	files["pricing.csv"] = body.Bytes()
	manifest := readManifestBytes(t, files["manifest.json"])
	if manifest.Files != nil {
		manifest.Files["pricing.csv"] = digest(files["pricing.csv"])
	}
	files["manifest.json"] = marshalManifest(t, manifest)
}

func setPricingCell(t *testing.T, rows [][]string, model, speed, band, column, value string) {
	t.Helper()
	index := -1
	for i, name := range rows[0] {
		if name == column {
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("missing column %q", column)
	}
	for _, row := range rows[1:] {
		if row[0] == model && row[1] == speed && row[2] == band {
			row[index] = value
			return
		}
	}
	t.Fatalf("missing row %s/%s/%s", model, speed, band)
}

func requireRequestCatalog(t *testing.T, files map[string][]byte) *Catalog {
	t.Helper()
	catalog, err := loadFiles(files, Source{Kind: "local"})
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestRequestPricingSchemaCompatibility(t *testing.T) {
	v1 := catalogFiles(t, filepath.Join("testdata", "v1"))
	legacy := requireRequestCatalog(t, v1)
	if legacy.Digest() != "7e4512bcc46d396e800e8f71d4ef3ba5ca97108411b3ad36d6b920ef8af3f162" {
		t.Fatalf("unchanged v1 digest = %s", legacy.Digest())
	}
	for _, price := range legacy.Pricing() {
		if price.RequestPricing != nil {
			t.Fatal("unchanged v1 snapshot produced a request binding")
		}
	}
	if _, reason := legacy.PriceForRequest("openai-api-key", "gpt-6.1-sol", "default", 1); reason != UnavailablePrice {
		t.Fatalf("v1 request selection = %q", reason)
	}
	if price, ok := legacy.PriceFor("claude-sonnet-4-6", "standard"); !ok || *price.Input != 3 {
		t.Fatal("legacy Claude row changed")
	}
	for _, tc := range []struct {
		name    string
		files   map[string][]byte
		version int
	}{
		{"v1 header with v2", v1, 2}, {"v2 header with v1", requestCatalogFiles(t), 1},
		{"future", requestCatalogFiles(t), 3}, {"zero", requestCatalogFiles(t), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := copyBytes(tc.files)
			manifest := readManifestBytes(t, files["manifest.json"])
			manifest.SchemaVersion = tc.version
			files["manifest.json"] = marshalManifest(t, manifest)
			if _, err := loadFiles(files, Source{}); err == nil {
				t.Fatal("accepted mismatched schema/header")
			}
		})
	}
}

func TestBundledRequestPricingScopeAndThresholdEdges(t *testing.T) {
	catalog := requireRequestCatalog(t, requestCatalogFiles(t))
	wantRows := map[string]int{"gpt-6.1-sol": 4, "gpt-6-astra": 6, "gpt-6-sol": 4, "gpt-6-luna": 4, "gpt-5.6-sol": 2, "gpt-5.6-terra": 2, "gpt-5.6-luna": 2}
	gotRows := map[string]int{}
	for _, price := range catalog.Pricing() {
		if price.RequestPricing == nil {
			if wantRows[price.ModelID] != 0 {
				t.Fatalf("missing reviewed binding for %#v", price)
			}
			continue
		}
		gotRows[price.ModelID]++
		if price.ContextBand != "short" {
			continue
		}
		for _, tier := range price.RequestPricing.ObservedServiceTiers {
			for _, input := range []int64{0, 271999, 272000, 272001, math.MaxInt64} {
				selection, reason := catalog.PriceForRequest("openai-api-key", price.ModelID, tier, input)
				band := "short"
				if input >= 272001 {
					band = "long"
				}
				var expected Price
				for _, row := range catalog.PricesFor(price.ModelID, price.Speed) {
					if row.ContextBand == band {
						expected = row
					}
				}
				if reason != "" || !reflect.DeepEqual(selection.Price, expected) || selection.ObservedServiceTier != tier {
					t.Fatalf("%s/%s/%s input=%d got %#v, %q; want literal %s row", price.ModelID, price.Speed, tier, input, selection, reason, band)
				}
				if selection.CatalogDigest != catalog.Digest() || selection.CatalogRevision != catalog.Revision() || selection.CatalogSchemaVersion != 2 {
					t.Fatal("selection lost catalog identity")
				}
			}
		}
	}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("bound rows = %#v, want %#v", gotRows, wantRows)
	}
	// The data migration may add provenance but must not change any absolute rates.
	legacy := requireRequestCatalog(t, catalogFiles(t, filepath.Join("testdata", "v1")))
	old := legacy.Pricing()
	if len(catalog.Pricing()) != len(old) {
		t.Fatal("data migration changed row count")
	}
	for i, current := range catalog.Pricing() {
		current.RequestPricing = nil
		current.SourceURL, current.VerifiedDate = old[i].SourceURL, old[i].VerifiedDate
		if !reflect.DeepEqual(current, old[i]) {
			t.Fatalf("migration changed rate identity or rates: %#v vs %#v", current, old[i])
		}
	}
}

func TestPriceForRequestRejectsUnverifiedIdentity(t *testing.T) {
	catalog := requireRequestCatalog(t, requestCatalogFiles(t))
	if _, reason := (*Catalog)(nil).PriceForRequest("openai-api-key", "gpt-6.1-sol", "default", 1); reason != UnavailableCatalog {
		t.Fatalf("nil catalog reason = %q", reason)
	}
	for _, tc := range []struct {
		runtime, model, tier string
		input                int64
	}{
		{"openai-api-key", "gpt-6.1-sol", "default", -1}, {"", "gpt-6.1-sol", "default", 1},
		{"openai-subscription-codex-cli", "gpt-6.1-sol", "default", 1}, {"anthropic-api-key", "gpt-6.1-sol", "default", 1},
		{"openai-api-key", "gpt-6.1-sol-20261008", "default", 1}, {"openai-api-key", "gpt-6.1-sol-custom", "default", 1},
		{"openai-api-key", "", "default", 1}, {"openai-api-key", "gpt-5.4", "default", 1},
		{"openai-api-key", "gpt-5.5", "default", 1}, {"openai-api-key", "gpt-5.6-terra", "fast", 1},
	} {
		if _, reason := catalog.PriceForRequest(tc.runtime, tc.model, tc.tier, tc.input); reason == "" {
			t.Fatalf("accepted unsupported identity %#v", tc)
		}
	}
	for _, tier := range []string{"", "auto", "standard", "Default", "FAST", "priority ", "mixed", "unknown", "flex", "batch", "scale"} {
		if _, reason := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", tier, 1); reason == "" {
			t.Fatalf("accepted raw tier %q", tier)
		}
	}
	for _, price := range catalog.pricing {
		if price.ModelID == "gpt-6.1-sol" && price.Speed == "standard" && price.ContextBand == "short" {
			catalog.pricing = append(catalog.pricing, price)
			break
		}
	}
	if _, reason := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", "default", 1); reason != UnavailableAmbiguousPrice {
		t.Fatalf("duplicate reason = %q", reason)
	}
}

func TestRequestPricingRejectsMalformedCells(t *testing.T) {
	cases := []struct{ column, value string }{
		{"runtime_id", ""}, {"runtime_id", "missing"}, {"runtime_id", "openai-subscription-codex-cli"},
		{"observed_service_tiers", ""}, {"observed_service_tiers", "default|default"}, {"observed_service_tiers", "default|"},
		{"observed_service_tiers", "|default"}, {"observed_service_tiers", "default||priority"}, {"observed_service_tiers", "default| priority"},
		{"observed_service_tiers", "Default"}, {"observed_service_tiers", "auto"}, {"observed_service_tiers", "standard"},
		{"observed_service_tiers", "priority"}, {"observed_service_tiers", "mixed"}, {"observed_service_tiers", "flex"},
		{"input_tokens_min_inclusive", ""}, {"input_tokens_min_inclusive", "null"}, {"input_tokens_min_inclusive", "1"},
		{"input_tokens_max_exclusive", "0"}, {"input_tokens_max_exclusive", ""}, {"input_tokens_max_exclusive", "null"},
		{"rate_application", ""}, {"rate_application", "null"}, {"rate_application", "marginal"},
	}
	for _, column := range []string{"input_tokens_min_inclusive", "input_tokens_max_exclusive"} {
		for _, raw := range []string{"-1", "+1", "1.0", "1e5", "NaN", "1/2", "9223372036854775808", "1_000", "0x20", "１"} {
			cases = append(cases, struct{ column, value string }{column, raw})
		}
	}
	for _, tc := range cases {
		t.Run(tc.column+"="+tc.value, func(t *testing.T) {
			files := requestCatalogFiles(t)
			rows := pricingRecords(t, files)
			setPricingCell(t, rows, "gpt-6.1-sol", "standard", "short", tc.column, tc.value)
			replacePricingRecords(t, files, rows)
			if _, err := loadFiles(files, Source{}); err == nil {
				t.Fatal("accepted malformed binding")
			}
		})
	}
	for _, tc := range []struct{ speed, tier string }{{"fast", "default"}, {"ultrafast", "fast"}, {"standard", "ultrafast"}} {
		files := requestCatalogFiles(t)
		rows := pricingRecords(t, files)
		setPricingCell(t, rows, "gpt-6-astra", tc.speed, "short", "observed_service_tiers", tc.tier)
		replacePricingRecords(t, files, rows)
		if _, err := loadFiles(files, Source{}); err == nil {
			t.Fatalf("accepted cross-speed tier collision %#v", tc)
		}
	}
}

func TestRequestPricingRejectsInvalidGroups(t *testing.T) {
	for _, kind := range []string{"gap", "overlap", "missing partner", "partial activation", "different tier set", "mixed all", "reversed", "bounded long", "runtime model relation", "runtime semantics", "duplicate row"} {
		t.Run(kind, func(t *testing.T) {
			files := requestCatalogFiles(t)
			rows := pricingRecords(t, files)
			switch kind {
			case "gap":
				setPricingCell(t, rows, "gpt-6.1-sol", "standard", "long", "input_tokens_min_inclusive", "272002")
			case "overlap":
				setPricingCell(t, rows, "gpt-6.1-sol", "standard", "long", "input_tokens_min_inclusive", "272000")
			case "missing partner":
				for i, row := range rows {
					if row[0] == "gpt-6.1-sol" && row[1] == "standard" && row[2] == "long" {
						rows = append(rows[:i], rows[i+1:]...)
						break
					}
				}
			case "partial activation":
				for _, column := range requestPricingColumns {
					setPricingCell(t, rows, "gpt-6.1-sol", "standard", "long", column, "")
				}
			case "different tier set":
				setPricingCell(t, rows, "gpt-6.1-sol", "fast", "long", "observed_service_tiers", "priority")
			case "mixed all":
				rows = append(rows, []string{"gpt-6.1-sol", "standard", "all", "2", "10", "0.1", "2.5", "", "", "https://example.invalid", "2026-10-08", "openai-api-key", "default", "0", "", "whole_request"})
			case "reversed":
				setPricingCell(t, rows, "gpt-6.1-sol", "standard", "short", "input_tokens_min_inclusive", "272002")
			case "bounded long":
				setPricingCell(t, rows, "gpt-6.1-sol", "standard", "long", "input_tokens_max_exclusive", "999999")
			case "runtime model relation":
				files["models.csv"] = []byte(strings.Replace(string(files["models.csv"]), "openai-api-key,gpt-5.6-terra,", "openai-api-key,unpriced-custom-model,", 1))
			case "runtime semantics":
				files["runtimes.csv"] = []byte(strings.Replace(string(files["runtimes.csv"]), "openai-api-key,openai,api_key,openai_api,openai_api,", "openai-api-key,openai,api_key,gateway,gateway,", 1))
			case "duplicate row":
				rows = append(rows, append([]string(nil), rows[len(rows)-1]...))
			}
			replacePricingRecords(t, files, rows)
			manifest := readManifestBytes(t, files["manifest.json"])
			manifest.Files["models.csv"] = digest(files["models.csv"])
			manifest.Files["runtimes.csv"] = digest(files["runtimes.csv"])
			files["manifest.json"] = marshalManifest(t, manifest)
			if _, err := loadFiles(files, Source{}); err == nil {
				t.Fatal("accepted invalid request pricing group")
			}
		})
	}
}

func TestRequestPricingCustomBoundariesAndLiteralRates(t *testing.T) {
	for _, boundary := range []int64{1, 100001, math.MaxInt64} {
		files := requestCatalogFiles(t)
		// An explicit custom model must use its own data, regardless of its name.
		for _, name := range []string{"models.csv", "defaults.csv", "pricing.csv"} {
			files[name] = bytes.ReplaceAll(files[name], []byte("gpt-6.1-sol"), []byte("custom-request-model"))
		}
		manifest := readManifestBytes(t, files["manifest.json"])
		manifest.Files = nil
		files["manifest.json"] = marshalManifest(t, manifest)
		rows := pricingRecords(t, files)
		setPricingCell(t, rows, "custom-request-model", "standard", "short", "input_tokens_max_exclusive", fmt.Sprint(boundary))
		setPricingCell(t, rows, "custom-request-model", "standard", "long", "input_tokens_min_inclusive", fmt.Sprint(boundary))
		for column, value := range map[string]string{"input_usd_per_million": "7", "cache_read_usd_per_million": "0.7", "cache_write_usd_per_million": "9", "output_usd_per_million": "17"} {
			setPricingCell(t, rows, "custom-request-model", "standard", "long", column, value)
		}
		replacePricingRecords(t, files, rows)
		catalog := requireRequestCatalog(t, files)
		short, r1 := catalog.PriceForRequest("openai-api-key", "custom-request-model", "default", boundary-1)
		long, r2 := catalog.PriceForRequest("openai-api-key", "custom-request-model", "default", boundary)
		if r1 != "" || short.Price.ContextBand != "short" || r2 != "" || long.Price.ContextBand != "long" {
			t.Fatalf("custom boundary %d not honored", boundary)
		}
		if *long.Price.Input != 7 || *long.Price.CacheRead != 0.7 || *long.Price.CacheWrite != 9 || *long.Price.Output != 17 {
			t.Fatalf("custom rates transformed: %#v", long.Price)
		}
	}
}

func TestRequestPricingAllAndBlankBindings(t *testing.T) {
	for _, binding := range []string{"blank", "null", "bound"} {
		files := requestCatalogFiles(t)
		rows := pricingRecords(t, files)
		filtered := [][]string{rows[0]}
		for _, row := range rows[1:] {
			if row[0] != "gpt-6.1-sol" {
				filtered = append(filtered, row)
			}
		}
		row := []string{"gpt-6.1-sol", "standard", "all", "7", "17", "0.7", "9", "", "", "https://example.invalid", "2026-10-08", "", "", "", "", ""}
		if binding == "null" {
			for i := 11; i < len(row); i++ {
				row[i] = "null"
			}
		}
		if binding == "bound" {
			copy(row[11:], []string{"openai-api-key", "default", "0", "null", "whole_request"})
		}
		replacePricingRecords(t, files, append(filtered, row))
		catalog := requireRequestCatalog(t, files)
		_, reason := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", "default", math.MaxInt64)
		_, legacy := catalog.PriceFor("gpt-6.1-sol", "standard")
		if (reason == "") != (binding == "bound") || legacy != (binding != "bound") {
			t.Fatalf("%s binding request=%q legacy=%t", binding, reason, legacy)
		}
		if binding == "bound" {
			for _, change := range []struct{ column, value string }{{"input_tokens_min_inclusive", "1"}, {"input_tokens_max_exclusive", "100"}} {
				bad := copyBytes(files)
				badRows := pricingRecords(t, bad)
				setPricingCell(t, badRows, "gpt-6.1-sol", "standard", "all", change.column, change.value)
				replacePricingRecords(t, bad, badRows)
				if _, err := loadFiles(bad, Source{}); err == nil {
					t.Fatal("accepted partial all interval")
				}
			}
		}
	}
}

func TestRequestPricingDigestAndDetachedAccessors(t *testing.T) {
	files := requestCatalogFiles(t)
	catalog := requireRequestCatalog(t, files)
	if catalog.Digest() != fmt.Sprintf("%x", snapshotDigest(files)) || len(catalog.Digest()) != 64 || (*Catalog)(nil).Digest() != "" {
		t.Fatal("digest does not identify exact five-file snapshot")
	}
	original, reason := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", "priority", 1)
	if reason != "" {
		t.Fatal(reason)
	}
	for _, accessor := range []string{"selection", "Pricing", "PricesFor"} {
		var price Price
		switch accessor {
		case "selection":
			selection, _ := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", "priority", 1)
			price = selection.Price
		case "Pricing":
			for _, candidate := range catalog.Pricing() {
				if candidate.ModelID == "gpt-6.1-sol" && candidate.Speed == "fast" && candidate.ContextBand == "short" {
					price = candidate
				}
			}
		case "PricesFor":
			for _, candidate := range catalog.PricesFor("gpt-6.1-sol", "fast") {
				if candidate.ContextBand == "short" {
					price = candidate
				}
			}
		}
		if price.RequestPricing == nil {
			t.Fatalf("%s did not return binding", accessor)
		}
		price.RequestPricing.ObservedServiceTiers[0] = "mutated"
		*price.RequestPricing.InputTokensMinInclusive = 100
		*price.RequestPricing.InputTokensMaxExclusive = 101
		price.RequestPricing.RuntimeID = "mutated"
		price.RequestPricing.RateApplication = "mutated"
		*price.Input = 999
		selection, reason := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", "priority", 1)
		if reason != "" || !reflect.DeepEqual(selection, original) {
			t.Fatalf("%s leaked mutable data", accessor)
		}
	}
	// Byte identity must survive a caller mutating the source byte map.
	files["pricing.csv"][0] = '!'
	selected, _ := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", "priority", 1)
	if !reflect.DeepEqual(selected, original) {
		t.Fatal("source bytes mutated loaded selection")
	}

	baselineFiles := requestCatalogFiles(t)
	manifest := readManifestBytes(t, baselineFiles["manifest.json"])
	manifest.Files = nil // Unchecksummed custom snapshots still need exact identity.
	baselineFiles["manifest.json"] = marshalManifest(t, manifest)
	baseline := requireRequestCatalog(t, baselineFiles)
	for _, change := range []string{"bounds", "tiers", "rates", "manifest.json", "runtimes.csv", "models.csv", "defaults.csv"} {
		changed := copyBytes(baselineFiles)
		rows := pricingRecords(t, changed)
		switch change {
		case "bounds":
			setPricingCell(t, rows, "gpt-6.1-sol", "fast", "short", "input_tokens_max_exclusive", "100001")
			setPricingCell(t, rows, "gpt-6.1-sol", "fast", "long", "input_tokens_min_inclusive", "100001")
		case "tiers":
			for _, band := range []string{"short", "long"} {
				setPricingCell(t, rows, "gpt-6.1-sol", "fast", band, "observed_service_tiers", "priority")
			}
		case "rates":
			setPricingCell(t, rows, "gpt-6.1-sol", "fast", "long", "input_usd_per_million", "7")
		}
		replacePricingRecords(t, changed, rows)
		if strings.Contains(change, ".") {
			changed[change] = append(changed[change], '\n')
		}
		other := requireRequestCatalog(t, changed)
		if other.Revision() != baseline.Revision() || other.Digest() == baseline.Digest() {
			t.Fatalf("%s did not change digest at the same revision", change)
		}
	}
}

func TestRequestPricingChecksumAndInvalidUpdateRetention(t *testing.T) {
	source := copyBundledCatalog(t)
	target := t.TempDir()
	before, err := Update(context.Background(), UpdateOptions{SourceDir: source, InstallDir: target})
	if err != nil {
		t.Fatal(err)
	}
	pointer := activeRevision(t, target)
	files := catalogFiles(t, source)
	for _, column := range []string{"input_tokens_max_exclusive", "observed_service_tiers"} {
		bad := copyBytes(files)
		rows := pricingRecords(t, bad)
		value := "100001"
		if column == "observed_service_tiers" {
			value = "fast"
		}
		setPricingCell(t, rows, "gpt-6.1-sol", "fast", "short", column, value)
		replacePricingRecords(t, bad, rows)
		bad["manifest.json"] = files["manifest.json"]
		if _, err := loadFiles(bad, Source{}); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Fatalf("changed %s escaped checksum: %v", column, err)
		}
	}
	rows := pricingRecords(t, files)
	setPricingCell(t, rows, "gpt-6.1-sol", "standard", "long", "input_tokens_min_inclusive", "272002")
	replacePricingRecords(t, files, rows)
	for _, name := range requiredFiles {
		if err := os.WriteFile(filepath.Join(source, name), files[name], 0o600); err != nil {
			t.Fatal(err)
		} // #nosec G703 -- fixed catalog names under t.TempDir.
	}
	if _, err := LoadPath(source); err == nil {
		t.Fatal("invalid explicit catalog fell back")
	}
	if _, err := Update(context.Background(), UpdateOptions{SourceDir: source, InstallDir: target}); err == nil {
		t.Fatal("invalid interval update succeeded")
	}
	if activeRevision(t, target) != pointer {
		t.Fatal("invalid update replaced pointer")
	}
	after, err := LoadPath(filepath.Join(target, pointer))
	if err != nil || after.Digest() != before.Digest() {
		t.Fatalf("previous snapshot not retained: %v", err)
	}
}

func TestRequestPricingJSONPreservesUnboundedAndZero(t *testing.T) {
	catalog := requireRequestCatalog(t, requestCatalogFiles(t))
	for _, input := range []int64{0, 272001} {
		selection, reason := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", "default", input)
		if reason != "" {
			t.Fatal(reason)
		}
		body, err := json.Marshal(selection)
		if err != nil {
			t.Fatal(err)
		}
		var decoded PriceSelection
		if err := json.Unmarshal(body, &decoded); err != nil || !reflect.DeepEqual(decoded, selection) {
			t.Fatalf("selection JSON lost binding: %s, %v", body, err)
		}
		if input == 0 && !bytes.Contains(body, []byte(`"input_tokens_min_inclusive":0`)) {
			t.Fatal("JSON omitted known zero")
		}
		if input == 272001 && !bytes.Contains(body, []byte(`"input_tokens_max_exclusive":null`)) {
			t.Fatal("JSON omitted unbounded maximum")
		}
	}
}

func TestRequestPricingRequiresCompleteMetadataAndPreservesTierSets(t *testing.T) {
	for column, value := range map[string]string{
		"runtime_id": "openai-api-key", "observed_service_tiers": "default",
		"input_tokens_min_inclusive": "0", "input_tokens_max_exclusive": "100",
		"rate_application": "whole_request",
	} {
		files := requestCatalogFiles(t)
		rows := pricingRecords(t, files)
		setPricingCell(t, rows, "claude-sonnet-5", "standard", "all", column, value)
		replacePricingRecords(t, files, rows)
		if _, err := loadFiles(files, Source{}); err == nil {
			t.Fatalf("accepted only %s of a binding", column)
		}
	}
	files := requestCatalogFiles(t)
	rows := pricingRecords(t, files)
	setPricingCell(t, rows, "gpt-6.1-sol", "fast", "long", "observed_service_tiers", "priority|fast")
	setPricingCell(t, rows, "gpt-6.1-sol", "fast", "short", "input_tokens_min_inclusive", " 0 ")
	replacePricingRecords(t, files, rows)
	catalog := requireRequestCatalog(t, files)
	for _, tier := range []string{"fast", "priority"} {
		selection, reason := catalog.PriceForRequest("openai-api-key", "gpt-6.1-sol", tier, 272001)
		if reason != "" || selection.ObservedServiceTier != tier || selection.Price.ContextBand != "long" {
			t.Fatal("tier-set equality lost exact observed tier")
		}
	}
}

func TestRequestPricingRetainsLegacyRateValidation(t *testing.T) {
	for _, schema := range []int{1, 2} {
		for _, invalid := range []string{"-1", "NaN", "+Inf", "1e999", "missing provenance", "all unknown"} {
			files := requestCatalogFiles(t)
			if schema == 1 {
				files = catalogFiles(t, filepath.Join("testdata", "v1"))
			}
			rows := pricingRecords(t, files)
			switch invalid {
			case "missing provenance":
				setPricingCell(t, rows, "claude-sonnet-5", "standard", "all", "source_url", "")
			case "all unknown":
				for _, column := range rows[0][3:9] {
					setPricingCell(t, rows, "claude-sonnet-5", "standard", "all", column, "null")
				}
			default:
				setPricingCell(t, rows, "claude-sonnet-5", "standard", "all", "input_usd_per_million", invalid)
			}
			replacePricingRecords(t, files, rows)
			if _, err := loadFiles(files, Source{}); err == nil {
				t.Fatalf("schema %d accepted %s rate", schema, invalid)
			}
		}
	}
}

func replacePricingHeader(t *testing.T, files map[string][]byte, header string) {
	t.Helper()
	parts := bytes.SplitN(files["pricing.csv"], []byte("\n"), 2)
	if len(parts) != 2 {
		t.Fatal("pricing fixture has no header line")
	}
	files["pricing.csv"] = append([]byte(header+"\n"), parts[1]...)
	manifest := readManifestBytes(t, files["manifest.json"])
	manifest.Files["pricing.csv"] = digest(files["pricing.csv"])
	files["manifest.json"] = marshalManifest(t, manifest)
}

func TestSchemaTwoPricingRequiresExactHeaderNames(t *testing.T) {
	base := requestCatalogFiles(t)
	headers := pricingRecords(t, base)[0]
	cases := map[string]string{}
	for i, header := range headers {
		for _, padding := range []struct{ name, prefix, suffix string }{
			{"leading space", " ", ""}, {"trailing space", "", " "},
			{"leading tab", "\t", ""}, {"trailing tab", "", "\t"},
			{"quoted leading", "\" ", "\""}, {"quoted trailing", "\"", " \""},
		} {
			changed := append([]string(nil), headers...)
			changed[i] = padding.prefix + header + padding.suffix
			cases[header+"/"+padding.name] = strings.Join(changed, ",")
		}
	}
	for _, indexes := range [][]int{{11, 12, 13, 14, 15}, {11, 12}, {13, 15}} {
		changed := append([]string(nil), headers...)
		for _, i := range indexes {
			changed[i] += " "
		}
		cases[fmt.Sprintf("partial-or-all-five-%v", indexes)] = strings.Join(changed, ",")
	}
	duplicate := append([]string(nil), headers...)
	duplicate[12] = duplicate[11]
	cases["duplicate"] = strings.Join(duplicate, ",")
	duplicate[12] = duplicate[11] + " "
	cases["canonical collision"] = strings.Join(duplicate, ",")
	reordered := append([]string(nil), headers...)
	reordered[13], reordered[14] = reordered[14], reordered[13]
	cases["reordered bounds"] = strings.Join(reordered, ",")
	cases["missing column"] = strings.Join(headers[:15], ",")
	cases["extra duplicate column"] = strings.Join(append(append([]string(nil), headers...), headers[15]), ",")
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			files := copyBytes(base)
			replacePricingHeader(t, files, header)
			if _, err := loadFiles(files, Source{}); err == nil || !strings.Contains(err.Error(), "header") {
				t.Fatalf("non-exact v2 header error = %v, want header rejection", err)
			}
		})
	}
}

func TestSchemaOnePricingRetainsHistoricalUnquotedLeadingWhitespace(t *testing.T) {
	files := catalogFiles(t, filepath.Join("testdata", "v1"))
	original := requireRequestCatalog(t, files)
	headers := pricingRecords(t, files)[0]
	for i := range headers {
		headers[i] = " " + headers[i]
	}
	replacePricingHeader(t, files, strings.Join(headers, ","))
	legacy := requireRequestCatalog(t, files)
	if !reflect.DeepEqual(legacy.Pricing(), original.Pricing()) {
		t.Fatal("schema-1 historical leading-header whitespace behavior changed")
	}
}
