package llm

import (
	"fmt"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

// NewAdapterFromConfig constructs the LLM adapter selected by an LLM config,
// dispatching across the subprocess (Claude/Codex/Pi) and direct-API
// (Anthropic/OpenAI) adapters. The store is only consulted for API-key adapters
// to resolve the credential; subprocess adapters ignore it.
//
// This is the single source of truth for adapter selection shared by the review
// and respond commands.
func NewAdapterFromConfig(llmConfig config.LLMConfig, store APITokenStore) (Adapter, error) {
	switch llmConfig.Adapter {
	case config.LLMAdapterClaudeCLI:
		return NewClaudeCLIAdapter(SubprocessOptions{}), nil
	case config.LLMAdapterCodexCLI:
		if llmConfig.Provider != config.LLMProviderOpenAI || llmConfig.Auth != config.LLMAuthSubscription {
			return nil, fmt.Errorf("%w: codex_cli requires provider openai with subscription auth", config.ErrUnsupported)
		}
		return NewCodexCLIAdapter(SubprocessOptions{AllowBestEffortNoTools: true}), nil
	case config.LLMAdapterPiRPC:
		if llmConfig.Provider != config.LLMProviderPi || llmConfig.Auth != config.LLMAuthSubscription {
			return nil, fmt.Errorf("%w: pi_rpc requires provider pi with subscription auth", config.ErrUnsupported)
		}
		return NewPiRPCAdapter(PiRPCOptions{}), nil
	case config.LLMAdapterAnthropicAPI, config.LLMAdapterOpenAIAPI:
		return NewAPIAdapterFromConfig(llmConfig, store, APIOptions{})
	default:
		return nil, fmt.Errorf("%w: unsupported LLM adapter %q", config.ErrUnsupported, llmConfig.Adapter)
	}
}
