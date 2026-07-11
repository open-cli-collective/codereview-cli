package app

import (
	"slices"
	"testing"

	"github.com/open-cli-collective/codereview-cli/internal/llm"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
)

func TestLLMProgressFieldsIncludeFastOnlyWhenRequested(t *testing.T) {
	withoutFast := llmProgressFields("anthropic", "claude_cli", llm.Request{Model: "claude-opus-4-8"})
	withFast := llmProgressFields("anthropic", "claude_cli", llm.Request{Model: "claude-opus-4-8", Fast: true})
	fast := progress.Field{Key: "fast", Value: "true"}
	if slices.Contains(withoutFast, fast) || !slices.Contains(withFast, fast) {
		t.Fatalf("progress fields without fast = %#v, with fast = %#v", withoutFast, withFast)
	}
}
