// Package hooks dispatches observe-only lifecycle commands.
package hooks

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

const maxOutput = 8 << 10

// Payload is the stable JSON document written to a hook's stdin. Outcome is
// set only for terminal events. The remaining optional fields are specific to
// selection, reviewer, and posting events.
type Payload struct {
	Event string `json:"event"`
	PRURL string `json:"pr_url"`
	RunID string `json:"run_id"`
	// Author is the pull request author's git-host login, known once the run
	// has read the pull request.
	Author         string            `json:"author,omitempty"`
	Outcome        string            `json:"outcome,omitempty"`
	Profile        string            `json:"profile"`
	PassNumber     int               `json:"pass_number"`
	ArtifactDir    string            `json:"artifact_dir"`
	DryRun         bool              `json:"dry_run"`
	ReviewerID     string            `json:"reviewer_id,omitempty"`
	ReviewerStatus string            `json:"reviewer_status,omitempty"`
	ActionKind     string            `json:"action_kind,omitempty"`
	ActionMarker   string            `json:"action_marker,omitempty"`
	Agents         []string          `json:"agents,omitempty"`
	Models         map[string]string `json:"models,omitempty"`
}

// Dispatcher starts matching hook commands asynchronously and tracks them for
// process-exit draining.
type Dispatcher struct {
	entries  []config.Hook
	warnings io.Writer
	warnMu   sync.Mutex
	wg       sync.WaitGroup
}

// New returns a dispatcher for one resolved profile.
func New(entries []config.Hook, warnings io.Writer) *Dispatcher {
	return &Dispatcher{entries: append([]config.Hook(nil), entries...), warnings: warnings}
}

// Dispatch starts every hook matching payload.Event. It never waits for a
// command and never returns hook failures to the caller.
func (d *Dispatcher) Dispatch(payload Payload) {
	if d == nil {
		return
	}
	for _, entry := range d.entries {
		if entry.Event != payload.Event || payload.DryRun && !entry.OnDryRun {
			continue
		}
		timeout, err := time.ParseDuration(entry.Timeout)
		if err != nil || timeout <= 0 || len(entry.Argv) == 0 {
			d.warn(payload.Event, "invalid hook configuration", "")
			continue
		}
		body, err := json.Marshal(payload)
		if err != nil {
			d.warn(payload.Event, err.Error(), "")
			continue
		}
		argv := append([]string(nil), entry.Argv...)
		env := payloadEnv(payload)
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.run(payload.Event, argv, timeout, body, env)
		}()
	}
}

// Drain waits for all commands already dispatched. Each command remains
// bounded by its configured timeout.
func (d *Dispatcher) Drain() {
	if d != nil {
		d.wg.Wait()
	}
}

func (d *Dispatcher) run(event string, argv []string, timeout time.Duration, body []byte, env []string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	// #nosec G204 -- argv is the explicit user-configured hook command and is
	// executed directly without a shell.
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(append(body, '\n'))
	cmd.Env = append(os.Environ(), env...)
	output := &limitedBuffer{remaining: maxOutput}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if err == nil {
		return
	}
	if ctx.Err() == context.DeadlineExceeded {
		err = fmt.Errorf("timed out after %s", timeout)
	}
	d.warn(event, err.Error(), output.String())
}

func (d *Dispatcher) warn(event, detail, output string) {
	if d.warnings == nil {
		return
	}
	d.warnMu.Lock()
	defer d.warnMu.Unlock()
	if output = strings.TrimSpace(output); output != "" {
		_, _ = fmt.Fprintf(d.warnings, "warning: hook %s failed: %s: %s\n", event, detail, output)
		return
	}
	_, _ = fmt.Fprintf(d.warnings, "warning: hook %s failed: %s\n", event, detail)
}

func payloadEnv(payload Payload) []string {
	return []string{
		"CR_EVENT=" + payload.Event,
		"CR_PR_URL=" + payload.PRURL,
		"CR_RUN_ID=" + payload.RunID,
		"CR_AUTHOR=" + payload.Author,
		"CR_OUTCOME=" + payload.Outcome,
		"CR_PROFILE=" + payload.Profile,
		"CR_PASS_NUMBER=" + strconv.Itoa(payload.PassNumber),
		"CR_ARTIFACT_DIR=" + payload.ArtifactDir,
		"CR_DRY_RUN=" + strconv.FormatBool(payload.DryRun),
	}
}

type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	remaining int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) > b.remaining {
		p = p[:b.remaining]
	}
	_, _ = b.buf.Write(p)
	b.remaining -= len(p)
	return n, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
