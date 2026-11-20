package benchmarkcmd

import (
	"context"
	"os"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/openaidecisions"
)

type inferOpenAIDecisionsFlags struct {
	identityInferenceFlags
}

type openAIDecisionsDecider = identityexperiment.OpenAIDecisionsDecider

type inferOpenAIDecisionsDependencies struct {
	resolveAPIKey func(context.Context) (string, error)
	newClient     func(openaidecisions.Options) (identityexperiment.OpenAIDecisionsDecider, error)
	now           func() time.Time
	rename        func(string, string) error
}

func defaultInferOpenAIDecisionsDependencies() inferOpenAIDecisionsDependencies {
	return inferOpenAIDecisionsDependencies{
		newClient: func(options openaidecisions.Options) (identityexperiment.OpenAIDecisionsDecider, error) {
			client, err := openaidecisions.New(options)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		now:    time.Now,
		rename: os.Rename,
	}
}

func inferOpenAIDecisionsBackend(flags inferOpenAIDecisionsFlags, dependencies inferOpenAIDecisionsDependencies) identityInferenceBackend {
	f := flags.identityInferenceFlags
	backend := identityexperiment.OpenAIDecisionsBackend(identityexperiment.Configuration{
		Endpoint: f.endpoint, RequestedModel: f.model, ExpectedModel: effectiveIdentityInferenceExpectedModel(f),
		MaxStateBytes: f.maxStateBytes, MaxRequestBytes: f.maxRequestBytes, TimeoutMS: f.timeout.Milliseconds(),
	}, dependencies.newClient)
	return identityInferenceBackend{prepare: backend.Prepare, newClient: backend.NewClient,
		errorCode: backend.ErrorCode, estimateCost: backend.EstimateCost, provider: backend.Provider}
}
