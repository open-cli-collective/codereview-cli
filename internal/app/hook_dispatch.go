package app

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/open-cli-collective/codereview-cli/internal/hooks"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/runartifact"
)

type hookRunStore interface {
	GetRun(context.Context, string) (ledger.Run, error)
}

type hookDispatcher struct {
	dispatcher *hooks.Dispatcher
	enabled    bool
	store      hookRunStore
	command    string
	prURL      string
	profile    string

	mu            sync.Mutex
	dryRun        bool
	author        string
	run           ledger.Run
	once          map[string]bool
	selectionSeen bool
}

func newHookDispatcher(req OpenRequest, store hookRunStore) *hookDispatcher {
	return &hookDispatcher{
		dispatcher: hooks.New(req.Profile.Hooks, req.Warnings),
		enabled:    len(req.Profile.Hooks) > 0,
		store:      store,
		command:    commandName(req.Command),
		prURL:      req.PRURL,
		profile:    req.ProfileName,
		once:       map[string]bool{},
	}
}

func (d *hookDispatcher) begin(dryRun bool) {
	d.mu.Lock()
	d.dryRun = dryRun
	d.mu.Unlock()
	d.emit(d.event("run.started"), hooks.Payload{}, ledger.Run{}, false)
}

func (d *hookDispatcher) event(name string) string {
	if d.command == "respond" {
		return "respond." + name
	}
	return name
}

func (d *hookDispatcher) emitOnce(event string, extra hooks.Payload, run ledger.Run) {
	d.mu.Lock()
	if d.once[event] {
		d.mu.Unlock()
		return
	}
	d.once[event] = true
	d.mu.Unlock()
	d.emit(event, extra, run, false)
}

func (d *hookDispatcher) emit(event string, extra hooks.Payload, run ledger.Run, terminal bool) {
	if d == nil || d.dispatcher == nil {
		return
	}
	d.mu.Lock()
	if run.RunID != "" {
		d.run = run
	} else {
		run = d.run
	}
	payload := hooks.Payload{
		Event:       event,
		PRURL:       d.prURL,
		RunID:       run.RunID,
		Author:      d.author,
		Profile:     d.profile,
		PassNumber:  run.Attempt,
		ArtifactDir: run.ArtifactPath,
		DryRun:      d.dryRun,
	}
	d.mu.Unlock()
	payload.Outcome = extra.Outcome
	payload.ReviewerID = extra.ReviewerID
	payload.ReviewerStatus = extra.ReviewerStatus
	payload.ActionKind = extra.ActionKind
	payload.ActionMarker = extra.ActionMarker
	payload.Agents = extra.Agents
	payload.Models = extra.Models
	if terminal && payload.Outcome == "" {
		payload.Outcome = "failed"
	}
	d.dispatcher.Dispatch(payload)
}

func (d *hookDispatcher) observeArtifactLog(logPath string) ledger.Run {
	artifactDir := filepath.Dir(filepath.Dir(strings.TrimSpace(logPath)))
	if artifactDir == "." || artifactDir == "" {
		return ledger.Run{}
	}
	kind := runartifact.KindReview
	if d.command == "respond" {
		kind = runartifact.KindThreadResponse
	}
	marker, err := runartifact.ReadMarker(artifactDir, kind)
	if err != nil {
		return ledger.Run{ArtifactPath: artifactDir}
	}
	run, err := d.store.GetRun(context.Background(), marker.RunID)
	if err != nil {
		return ledger.Run{RunID: marker.RunID, ArtifactPath: artifactDir}
	}
	return run
}

func (d *hookDispatcher) observeRunID(runID string) ledger.Run {
	runID = strings.TrimSpace(runID)
	if runID == "" || d.store == nil {
		return ledger.Run{RunID: runID}
	}
	run, err := d.store.GetRun(context.Background(), runID)
	if err != nil {
		return ledger.Run{RunID: runID}
	}
	return run
}

// observeAuthor records the pull request author for every later event. The
// first non-empty login wins: a run reads the pull request repeatedly, and a
// later read that fails or returns an unauthored snapshot must not erase an
// identity the hooks already reported.
func (d *hookDispatcher) observeAuthor(login string) {
	login = strings.TrimSpace(login)
	if d == nil || login == "" {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.author == "" {
		d.author = login
	}
}

func (d *hookDispatcher) observeRun(run ledger.Run) {
	if d == nil || run.RunID == "" {
		return
	}
	d.mu.Lock()
	d.run = run
	d.mu.Unlock()
}

func (d *hookDispatcher) selectionReady(result pipeline.Result) {
	d.emitOnce(d.event("workspace.prepared"), hooks.Payload{}, result.Run)
	d.emitOnce(d.event("dossier.ready"), hooks.Payload{}, result.Run)
	d.mu.Lock()
	selectionSeen := d.selectionSeen
	d.mu.Unlock()
	if !selectionSeen {
		return
	}
	agents := make([]string, 0, len(result.Selection.SelectedAgents))
	models := map[string]string{}
	for _, selected := range result.Selection.SelectedAgents {
		agents = append(agents, selected.AgentID)
	}
	for _, session := range result.Sessions {
		if session.AgentID != nil && strings.TrimSpace(*session.AgentID) != "" {
			models[*session.AgentID] = session.Model
		}
	}
	sort.Strings(agents)
	if len(models) == 0 {
		models = nil
	}
	d.emitOnce(d.event("selection.completed"), hooks.Payload{Agents: agents, Models: models}, result.Run)
}

func (d *hookDispatcher) observeSelection() {
	d.mu.Lock()
	d.selectionSeen = true
	d.mu.Unlock()
}

func (d *hookDispatcher) completeReview(result pipeline.Result) {
	d.selectionReady(result)
	d.emitOnce(d.event("plan.ready"), hooks.Payload{}, result.Run)
}

func (d *hookDispatcher) drain() {
	if d != nil {
		d.dispatcher.Drain()
	}
}
