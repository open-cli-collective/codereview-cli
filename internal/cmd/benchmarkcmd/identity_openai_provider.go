package benchmarkcmd

func openAIDecisionsIdentityProviderRuntime(provider identityProviderConfig, flags identityInferenceFlags) identityProviderRuntime {
	providerFlags := inferOpenAIDecisionsFlags{identityInferenceFlags: flags}
	providerFlags.endpoint = provider.Endpoint
	providerFlags.authScheme = "bearer"

	dependencies := defaultInferOpenAIDecisionsDependencies()
	return identityProviderRuntime{
		dependencies: identityInferenceDependencies{
			resolveAPIKey: dependencies.resolveAPIKey,
			now:           dependencies.now,
			rename:        dependencies.rename,
		},
		backend: inferOpenAIDecisionsBackend(providerFlags, dependencies),
	}
}
