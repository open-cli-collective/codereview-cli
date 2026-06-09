package llm

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

// claudeBGTranscriptUsage aggregates adapter-reported usage for a completed
// Claude background job from the session transcript referenced by the job
// state. Missing or unreadable transcripts yield empty usage: the caller
// treats usage as nullable, never zero.
func claudeBGTranscriptUsage(state map[string]any) Usage {
	path, _ := state["linkScanPath"].(string)
	if strings.TrimSpace(path) == "" {
		return Usage{}
	}
	return claudeTranscriptUsage(path)
}

type claudeTranscriptUsagePayload struct {
	InputTokens         int `json:"input_tokens"`
	OutputTokens        int `json:"output_tokens"`
	CacheReadTokens     int `json:"cache_read_input_tokens"`
	CacheCreationTokens int `json:"cache_creation_input_tokens"`
}

type claudeTranscriptEvent struct {
	Type    string `json:"type"`
	Message struct {
		ID    string                        `json:"id"`
		Usage *claudeTranscriptUsagePayload `json:"usage"`
	} `json:"message"`
}

// claudeTranscriptUsage sums per-turn assistant usage from a Claude session
// transcript. Streaming writes repeat an assistant message with the same id;
// the last occurrence per message id wins so turns are not double-counted.
func claudeTranscriptUsage(path string) Usage {
	// #nosec G304 -- transcript path comes from the Claude bg job state file.
	file, err := os.Open(path)
	if err != nil {
		return Usage{}
	}
	defer func() { _ = file.Close() }()

	perMessage := map[string]claudeTranscriptUsagePayload{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var event claudeTranscriptEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			continue
		}
		if event.Type != "assistant" || event.Message.Usage == nil || event.Message.ID == "" {
			continue
		}
		perMessage[event.Message.ID] = *event.Message.Usage
	}
	if scanner.Err() != nil || len(perMessage) == 0 {
		return Usage{}
	}

	var tokensIn, tokensOut, cacheRead, cacheCreate int
	for _, usage := range perMessage {
		tokensIn += usage.InputTokens
		tokensOut += usage.OutputTokens
		cacheRead += usage.CacheReadTokens
		cacheCreate += usage.CacheCreationTokens
	}
	return Usage{
		TokensIn:    &tokensIn,
		TokensOut:   &tokensOut,
		CacheRead:   &cacheRead,
		CacheCreate: &cacheCreate,
	}
}
