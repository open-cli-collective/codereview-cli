package llm

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// claudeBGTranscriptUsage aggregates adapter-reported usage for a completed
// Claude background job from the session transcript referenced by the job
// state. Resumed sessions share a transcript with earlier jobs, so only
// assistant turns timestamped at or after the job's createdAt are counted;
// without a provable job boundary the usage stays nullable, never zero.
func claudeBGTranscriptUsage(state map[string]any) Usage {
	path, _ := state["linkScanPath"].(string)
	if strings.TrimSpace(path) == "" {
		return Usage{}
	}
	createdAtRaw, _ := state["createdAt"].(string)
	createdAt, err := time.Parse(time.RFC3339, strings.TrimSpace(createdAtRaw))
	if err != nil {
		return Usage{}
	}
	return claudeTranscriptUsage(path, createdAt)
}

type claudeTranscriptUsagePayload struct {
	InputTokens         int    `json:"input_tokens"`
	OutputTokens        int    `json:"output_tokens"`
	CacheReadTokens     int    `json:"cache_read_input_tokens"`
	CacheCreationTokens int    `json:"cache_creation_input_tokens"`
	Speed               string `json:"speed"`
}

type claudeTranscriptEvent struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Message   struct {
		ID    string                        `json:"id"`
		Usage *claudeTranscriptUsagePayload `json:"usage"`
	} `json:"message"`
}

// claudeTranscriptUsage sums per-turn assistant usage recorded at or after
// since. Streaming writes repeat an assistant message with the same id; the
// last occurrence per message id wins so turns are not double-counted.
func claudeTranscriptUsage(path string, since time.Time) Usage {
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
		timestamp, err := time.Parse(time.RFC3339, event.Timestamp)
		if err != nil {
			// An unscopeable usage event could belong to this job; a sum
			// that skips it would look complete while being partial.
			return Usage{}
		}
		if timestamp.Before(since) {
			continue
		}
		perMessage[event.Message.ID] = *event.Message.Usage
	}
	if scanner.Err() != nil || len(perMessage) == 0 {
		return Usage{}
	}

	var tokensIn, tokensOut, cacheRead, cacheCreate int
	speed := "fast"
	for _, usage := range perMessage {
		tokensIn += usage.InputTokens
		tokensOut += usage.OutputTokens
		cacheRead += usage.CacheReadTokens
		cacheCreate += usage.CacheCreationTokens
		switch usage.Speed {
		case "standard":
			speed = "standard"
		case "fast":
		default:
			if speed != "standard" {
				speed = "unknown"
			}
		}
	}
	return Usage{
		TokensIn:    &tokensIn,
		TokensOut:   &tokensOut,
		CacheRead:   &cacheRead,
		CacheCreate: &cacheCreate,
		Speed:       speed,
	}
}
