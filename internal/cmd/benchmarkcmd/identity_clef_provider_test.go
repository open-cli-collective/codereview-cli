package benchmarkcmd

import (
	"context"
	"reflect"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/cloudflareclef"
	"github.com/open-cli-collective/codereview-cli/internal/reportidentityinference"
)

func TestIdentityClefEndpointResolvesTheSelectedAccountAndModel(t *testing.T) {
	provider := identityProviderConfig{
		Name: "cloudflare", Kind: "cloudflare_clef", AccountID: clefTestAccountID,
		APIKeyEnv: defaultClefAPIKeyEnv,
	}
	for _, model := range []string{"clef", "clef-flash"} {
		t.Run(model, func(t *testing.T) {
			got, err := identityClefEndpoint(provider, model)
			if err != nil {
				t.Fatalf("identityClefEndpoint: %v", err)
			}
			want, err := cloudflareclef.Endpoint(clefTestAccountID, model)
			if err != nil {
				t.Fatalf("cloudflareclef.Endpoint: %v", err)
			}
			if got != want {
				t.Fatalf("resolved endpoint = %q, want %q", got, want)
			}
		})
	}
}

func TestIdentityClefEndpointRejectsInvalidConfigurationBeforeRuntime(t *testing.T) {
	valid := identityProviderConfig{
		Name: "cloudflare", Kind: "cloudflare_clef", AccountID: clefTestAccountID,
		APIKeyEnv: defaultClefAPIKeyEnv,
	}
	for _, test := range []struct {
		name     string
		provider identityProviderConfig
		model    string
	}{
		{name: "wrong adapter", provider: func() identityProviderConfig { p := valid; p.Kind = "system_one"; return p }(), model: "clef"},
		{name: "literal endpoint", provider: func() identityProviderConfig { p := valid; p.Endpoint = "https://example.invalid"; return p }(), model: "clef"},
		{name: "auth scheme", provider: func() identityProviderConfig { p := valid; p.AuthScheme = "bearer"; return p }(), model: "clef"},
		{name: "service tier", provider: func() identityProviderConfig { p := valid; p.ServiceTier = "priority"; return p }(), model: "clef"},
		{name: "bad key environment name", provider: func() identityProviderConfig { p := valid; p.APIKeyEnv = "CLOUDFLARE-TOKEN"; return p }(), model: "clef"},
		{name: "invalid account", provider: func() identityProviderConfig { p := valid; p.AccountID = "../account"; return p }(), model: "clef"},
		{name: "unknown model", provider: valid, model: "@cf/cloudflare/clef"},
	} {
		t.Run(test.name, func(t *testing.T) {
			dependencies, state := clefTestDependencies(t, nil, true)
			_, err := identityClefProviderRuntime(test.provider, identityInferenceFlags{model: test.model}, dependencies)
			if err == nil {
				t.Fatal("invalid configuration constructed a Clef runtime")
			}
			if state.keyLookups != 0 || state.clientOpens != 0 {
				t.Fatalf("invalid configuration touched credentials/client: lookups=%d clients=%d", state.keyLookups, state.clientOpens)
			}
		})
	}
}

func TestDefaultIdentityProviderRuntimeRegistersClefWithoutProviderCalls(t *testing.T) {
	provider := identityProviderConfig{
		Name: "cloudflare", Kind: "cloudflare_clef", AccountID: clefTestAccountID,
		APIKeyEnv: defaultClefAPIKeyEnv,
	}
	runtime, err := defaultIdentityProviderRuntime(provider, identityInferenceFlags{
		model: "clef", maxStateBytes: clefTestLimits.MaxStateBytes,
		maxRequestBytes: clefTestLimits.MaxRequestBytes, timeout: defaultIdentityInferenceTimeout,
	})
	if err != nil {
		t.Fatalf("defaultIdentityProviderRuntime: %v", err)
	}
	if runtime.dependencies.lookupAPIKey == nil || runtime.dependencies.now == nil || runtime.dependencies.rename == nil {
		t.Fatalf("Clef runtime dependencies are incomplete: %#v", runtime.dependencies)
	}
	if runtime.backend.provider != "cloudflare_clef" || runtime.backend.contextStatus != "unverified" {
		t.Fatalf("Clef backend metadata = provider:%q context:%q", runtime.backend.provider, runtime.backend.contextStatus)
	}
}

func TestIdentityClefRuntimeUsesTheSharedProjectionAndForcesUnverifiedContext(t *testing.T) {
	for _, model := range []string{"clef", "clef-flash"} {
		t.Run(model, func(t *testing.T) {
			provider := identityProviderConfig{
				Name: "cloudflare", Kind: "cloudflare_clef", AccountID: clefTestAccountID,
				APIKeyEnv: defaultClefAPIKeyEnv,
			}
			dependencies, state := clefTestDependencies(t, nil, true)
			flags := identityInferenceFlags{
				model: model, maxStateBytes: clefTestLimits.MaxStateBytes,
				maxRequestBytes: clefTestLimits.MaxRequestBytes, timeout: defaultIdentityInferenceTimeout,
			}
			runtime, err := identityClefProviderRuntime(provider, flags, dependencies)
			if err != nil {
				t.Fatalf("identityClefProviderRuntime: %v", err)
			}
			if runtime.backend.provider != "cloudflare_clef" || runtime.backend.contextStatus != "unverified" {
				t.Fatalf("Clef backend provenance = provider:%q context:%q", runtime.backend.provider, runtime.backend.contextStatus)
			}
			if key, ok := runtime.dependencies.lookupAPIKey(provider.APIKeyEnv); !ok || key != clefTestAPIKey {
				t.Fatalf("runtime credential adapter = (%q, %t)", key, ok)
			}

			input := clefCohortInput(t, 2)
			projection, err := reportidentityinference.Project(reportidentityinference.TaskCohort, inferenceFindingRecord(input.Findings[0]), inferenceFindingRecord(input.Findings[1]))
			if err != nil {
				t.Fatalf("shared Project: %v", err)
			}
			if _, err := runtime.backend.prepare(projection); err != nil {
				t.Fatalf("Clef request preparation: %v", err)
			}
			decider, err := runtime.backend.newClient(clefTestAPIKey)
			if err != nil {
				t.Fatalf("Clef client construction: %v", err)
			}
			receipt, err := decider.Decide(context.Background(), projection)
			if err != nil {
				t.Fatalf("Clef decision: %v", err)
			}
			if receipt.ContextStatus != "unverified" {
				t.Fatalf("Clef context status = %q, want unverified", receipt.ContextStatus)
			}
			if state.clientOpts.AccountID != clefTestAccountID || state.clientOpts.APIKey != clefTestAPIKey || state.clientOpts.Timeout != defaultIdentityInferenceTimeout || state.clientOpts.Limits != clefTestLimits {
				t.Fatalf("Cloudflare client options = %#v", state.clientOpts)
			}
			if state.client == nil || len(state.client.requests) != 1 {
				t.Fatalf("Clef client requests = %#v", state.client)
			}
			request := state.client.requests[0]
			if request.Model != model || request.ExpectedModel != model || !reflect.DeepEqual(request.State, projection.State) {
				t.Fatalf("request selection/projection = %#v", request)
			}
			question, ok := request.Questions[projection.QuestionName]
			if !ok || question.Instructions != projection.Question.Instructions || !reflect.DeepEqual(question.Criteria, projection.Question.Criteria) {
				t.Fatalf("request question diverged from shared projection: %#v", request.Questions)
			}
			if !state.client.claimedVerifiedContext {
				t.Fatal("fake provider did not return the deliberately conflicting verified status")
			}
		})
	}
}
