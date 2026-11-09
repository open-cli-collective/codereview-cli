package benchmarkcmd

import (
	"os"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/identityexperiment"
	"github.com/open-cli-collective/codereview-cli/internal/systemone"
)

const (
	defaultSystemOneTimeout  = defaultIdentityInferenceTimeout
	minimumSystemOneTimeout  = minimumIdentityInferenceTimeout
	maximumSystemOneTimeout  = maximumIdentityInferenceTimeout
	maximumSystemOneCalls    = maximumIdentityInferenceCalls
	defaultSystemOneEndpoint = "https://inference.baseten.co/v1/systemone"
	defaultSystemOneKeyEnv   = "BASETEN_API_KEY"
)

type systemOneDecider = identityexperiment.SystemOneDecider

type inferSystemOneDependencies struct {
	lookupAPIKey func(string) (string, bool)
	newClient    func(systemone.Options) (systemOneDecider, error)
	now          func() time.Time
	rename       func(string, string) error
	lstat        func(string) (os.FileInfo, error)
	mkdir        func(string, os.FileMode) error
	chmod        func(string, os.FileMode) error
}

func defaultInferSystemOneDependencies() inferSystemOneDependencies {
	return inferSystemOneDependencies{
		lookupAPIKey: func(name string) (string, bool) { return os.LookupEnv(name) },
		newClient: func(options systemone.Options) (systemOneDecider, error) {
			client, err := systemone.New(options)
			if err != nil {
				return nil, err
			}
			return client, nil
		},
		now:    time.Now,
		rename: os.Rename,
	}
}

func normalizedAuthScheme(value string) (string, bool) {
	switch value {
	case "api-key":
		return "Api-Key", true
	case "bearer":
		return "Bearer", true
	default:
		return "", false
	}
}

func inferSystemOneBackend(flags identityInferenceFlags, dependencies inferSystemOneDependencies) identityInferenceBackend {
	metadata := identityInferenceBackend{provider: "system_one"}
	configuration := identityInferenceConfigurationFor(flags, metadata)
	shared := identityexperiment.SystemOneBackend(configuration, identityexperiment.SystemOneDependencies{
		NewClient: dependencies.newClient,
	})
	return identityInferenceBackend{
		prepare: shared.Prepare, newClient: shared.NewClient, errorCode: shared.ErrorCode,
		estimateCost: shared.EstimateCost, provider: shared.Provider, contextStatus: shared.ContextStatus,
	}
}
