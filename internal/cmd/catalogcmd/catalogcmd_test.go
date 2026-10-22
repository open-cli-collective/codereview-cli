package catalogcmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/modelcatalog"
)

func TestCatalogShowJSONIncludesExactRequestBinding(t *testing.T) {
	catalog, err := modelcatalog.LoadBundled()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := render(&output, true, catalog); err != nil {
		t.Fatal(err)
	}
	var value catalogView
	if err := json.Unmarshal(output.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if value.Digest != catalog.Digest() || value.Manifest.SchemaVersion != 2 || value.Manifest.Revision != catalog.Revision() {
		t.Fatal("catalog JSON lost exact snapshot identity")
	}
	bound := 0
	for _, price := range value.Pricing {
		binding := price.RequestPricing
		if binding == nil {
			continue
		}
		bound++
		if binding.RuntimeID != "openai-api-key" || binding.RateApplication != "whole_request" || len(binding.ObservedServiceTiers) == 0 || binding.InputTokensMinInclusive == nil {
			t.Fatalf("incomplete JSON binding: %#v", binding)
		}
		if price.ContextBand == "short" && (*binding.InputTokensMinInclusive != 0 || binding.InputTokensMaxExclusive == nil || *binding.InputTokensMaxExclusive != 272001) {
			t.Fatal("JSON lost short interval")
		}
		if price.ContextBand == "long" && (*binding.InputTokensMinInclusive != 272001 || binding.InputTokensMaxExclusive != nil) {
			t.Fatal("JSON lost long interval")
		}
	}
	if bound != 24 {
		t.Fatalf("JSON request binding count = %d, want 24", bound)
	}
}
