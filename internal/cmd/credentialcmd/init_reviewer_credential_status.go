package credentialcmd

import (
	"fmt"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

type initReviewerCredentialKeyState string

const (
	initReviewerCredentialKeyMissing         initReviewerCredentialKeyState = "missing"
	initReviewerCredentialKeyExisting        initReviewerCredentialKeyState = "existing"
	initReviewerCredentialKeyStaged          initReviewerCredentialKeyState = "staged"
	initReviewerCredentialKeySkippedOptional initReviewerCredentialKeyState = "skipped optional"
	initReviewerCredentialKeyDeferred        initReviewerCredentialKeyState = "deferred"
	initReviewerCredentialKeyOptional        initReviewerCredentialKeyState = "optional"
	initReviewerCredentialKeyUnavailable     initReviewerCredentialKeyState = "status unavailable"
)

type initReviewerCredentialStatus struct {
	Ref            config.CredentialRef
	SecretsProfile credentials.ResolvedSecretsProfile
	Keys           []initReviewerCredentialKeyStatus
	Unavailable    string
}

type initReviewerCredentialKeyStatus struct {
	Key      string
	Required bool
	State    initReviewerCredentialKeyState
}

func currentInteractiveInitReviewerEntityPromptContext(opts *root.Options, deps initDeps, session initSessionDraft) initPromptContext {
	ctx := currentInteractiveInitInventoryPromptContext(session)
	ctx.ReviewerCredentialStatuses = buildInteractiveInitReviewerCredentialStatuses(opts, deps, session)
	return ctx
}

func buildInteractiveInitReviewerCredentialStatuses(opts *root.Options, deps initDeps, session initSessionDraft) []initReviewerCredentialStatus {
	if session.workspace == nil {
		return nil
	}
	plannedWriteKeys := projectInitPlannedWriteKeys(session.writes)
	entries := interactiveInitReviewerCredentialPlanEntries(session, plannedWriteKeys)
	statuses := make([]initReviewerCredentialStatus, 0, len(entries))
	stores := map[string]initStore{}
	defer func() {
		for _, store := range stores {
			if store != nil {
				_ = store.Close()
			}
		}
	}()
	for _, entry := range entries {
		if entry.Ref.Purpose != "reviewer_credentials" || entry.State == initCredentialPlanStateClearRef {
			continue
		}
		existing := map[string]bool{}
		unavailable := ""
		storeKey := initCredentialStoreKey(entry.SecretsProfile)
		store := stores[storeKey]
		if store == nil {
			if !canOpenInitStoreForReviewerStatus(deps, entry) {
				unavailable = "credential backend status unavailable"
			} else {
				opened, err := openInitStoreForEntry(deps, opts, session.backendFlagSet, session.cfg, entry)
				if err != nil || opened == nil {
					unavailable = "credential backend status unavailable"
				} else {
					store = opened
					stores[storeKey] = store
				}
			}
		}
		if store != nil {
			keys, err := existingInitCredentialKeys(store, entry.Ref.Ref)
			if err != nil {
				unavailable = "credential backend status unavailable"
			} else {
				existing = keys
			}
		}
		statuses = append(statuses, initReviewerCredentialStatusFromEntry(entry, session.writes[entry.Ref.Ref], session.credentialDecisions, existing, unavailable))
	}
	return statuses
}

func canOpenInitStoreForReviewerStatus(deps initDeps, entry initCredentialPlanEntry) bool {
	if entry.SecretsProfile.IsNamed() {
		return deps.openResolvedStore != nil
	}
	return deps.openStore != nil
}

func interactiveInitReviewerCredentialPlanEntries(session initSessionDraft, plannedWriteKeys map[string][]string) []initCredentialPlanEntry {
	profile := session.workspace.profile
	if currentProfile, ok := session.cfg.Profiles[session.workspace.profileName]; ok {
		profile = currentProfile
	}
	entries, err := planInitCredentialsWithConfig(session.cfg, session.workspace.previousProfile, profile, plannedWriteKeys)
	if err != nil || !hasInitReviewerCredentialPlanEntry(entries) {
		entries = append([]initCredentialPlanEntry(nil), session.workspace.credentialPlan...)
	}
	entries = appendSelectableReviewerCredentialPlanEntries(session, profile, plannedWriteKeys, entries)
	return refreshInteractiveCredentialPlan(entries, plannedWriteKeys, session.satisfiedRefs)
}

func hasInitReviewerCredentialPlanEntry(entries []initCredentialPlanEntry) bool {
	for _, entry := range entries {
		if entry.Ref.Purpose == "reviewer_credentials" {
			return true
		}
	}
	return false
}

func appendSelectableReviewerCredentialPlanEntries(session initSessionDraft, profile config.Profile, plannedWriteKeys map[string][]string, entries []initCredentialPlanEntry) []initCredentialPlanEntry {
	resolved, err := credentials.ResolveSecretsProfileForProfile(session.cfg, profile)
	if err != nil {
		return entries
	}
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		seen[initCredentialEntryKey(entry.Ref)] = struct{}{}
	}
	if standardRef, err := credentials.FormatRef(session.workspace.profileName + "-reviewer"); err == nil {
		entries = appendReviewerCredentialPlanEntry(entries, seen, resolved, plannedWriteKeys, config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     standardRef,
			Mode:    string(config.GitAuthModePAT),
		})
		entries = appendReviewerCredentialPlanEntry(entries, seen, resolved, plannedWriteKeys, config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     standardRef,
			Mode:    string(config.GitAuthModeGitHubApp),
		})
	}
	entities, _ := buildInitReviewerEntityInventory(session.cfg)
	for _, entity := range entities {
		if entity.Kind == initReviewerEntityKindUseGitIdentity {
			continue
		}
		ref := config.CredentialRef{
			Purpose: "reviewer_credentials",
			Ref:     strings.TrimSpace(entity.CredentialRef),
			Mode:    string(entity.AuthMode),
		}
		if ref.Ref == "" {
			continue
		}
		entries = appendReviewerCredentialPlanEntry(entries, seen, resolved, plannedWriteKeys, ref)
	}
	return entries
}

func appendReviewerCredentialPlanEntry(entries []initCredentialPlanEntry, seen map[string]struct{}, resolved credentials.ResolvedSecretsProfile, plannedWriteKeys map[string][]string, ref config.CredentialRef) []initCredentialPlanEntry {
	key := initCredentialEntryKey(ref)
	if _, ok := seen[key]; ok {
		return entries
	}
	specs, err := credentials.KeySpecsForPurpose(ref)
	if err != nil {
		return entries
	}
	entry := initCredentialPlanEntry{
		Ref:              ref,
		SecretsProfile:   resolved,
		KeySpecs:         append([]credentials.KeySpec(nil), specs...),
		PlannedWriteKeys: append([]string(nil), plannedWriteKeys[ref.Ref]...),
	}
	entry.MissingRequiredKeys = missingRequiredInitCredentialKeys(entry.KeySpecs, entry.PlannedWriteKeys)
	entry.State = classifyInitCredentialPlanEntry(entry)
	entries = append(entries, entry)
	seen[key] = struct{}{}
	return entries
}

func initReviewerCredentialStatusFromEntry(entry initCredentialPlanEntry, planned map[string]string, decisions map[initCredentialDecisionKey]initCredentialDecisionKind, existing map[string]bool, unavailable string) initReviewerCredentialStatus {
	status := initReviewerCredentialStatus{
		Ref:            entry.Ref,
		SecretsProfile: entry.SecretsProfile,
		Unavailable:    unavailable,
		Keys:           make([]initReviewerCredentialKeyStatus, 0, len(entry.KeySpecs)),
	}
	for _, spec := range entry.KeySpecs {
		status.Keys = append(status.Keys, initReviewerCredentialKeyStatus{
			Key:      spec.Key,
			Required: spec.Required,
			State:    deriveInitReviewerCredentialKeyState(entry, spec, planned, decisions, existing, unavailable != ""),
		})
	}
	return status
}

func deriveInitReviewerCredentialKeyState(entry initCredentialPlanEntry, spec credentials.KeySpec, planned map[string]string, decisions map[initCredentialDecisionKey]initCredentialDecisionKind, existing map[string]bool, unavailable bool) initReviewerCredentialKeyState {
	if _, ok := planned[spec.Key]; ok {
		return initReviewerCredentialKeyStaged
	}
	decision := decisions[initCredentialDecisionMapKey(entry, spec.Key)]
	if decision == initCredentialDecisionSkipOptional && !spec.Required {
		return initReviewerCredentialKeySkippedOptional
	}
	if existing[spec.Key] {
		return initReviewerCredentialKeyExisting
	}
	if decision == initCredentialDecisionDefer && spec.Required {
		return initReviewerCredentialKeyDeferred
	}
	if unavailable {
		return initReviewerCredentialKeyUnavailable
	}
	if spec.Required {
		return initReviewerCredentialKeyMissing
	}
	return initReviewerCredentialKeyOptional
}

func initReviewerCredentialStatusForSelectionRef(ctx initPromptContext, seed initDraft, selection string, reviewerSecretLocation string) (initReviewerCredentialStatus, bool) {
	state, err := reviewerEntityEditorStateForSelection(ctx, seed, selection)
	if err != nil || state.kind == initReviewerEntityKindUseGitIdentity {
		return initReviewerCredentialStatus{}, false
	}
	effectiveCredentialRef := strings.TrimSpace(reviewerSecretLocation)
	if effectiveCredentialRef == "" {
		effectiveCredentialRef = strings.TrimSpace(state.seed.ReviewerCredentialRef)
		if effectiveCredentialRef == "" {
			effectiveCredentialRef = state.standardReviewerRef
		}
	}
	if effectiveCredentialRef == "" {
		return initReviewerCredentialStatus{}, false
	}
	authMode := reviewerCredentialAuthModeForKind(state.kind)
	statusRef := config.CredentialRef{
		Purpose: "reviewer_credentials",
		Ref:     effectiveCredentialRef,
		Mode:    string(authMode),
	}
	for _, status := range ctx.ReviewerCredentialStatuses {
		if status.Ref.Purpose == statusRef.Purpose && status.Ref.Ref == statusRef.Ref && status.Ref.Mode == statusRef.Mode {
			return status, true
		}
	}
	backendStatusesWereComputed := ctx.ReviewerCredentialStatuses != nil
	if backendStatusesWereComputed {
		return synthesizeUnavailableReviewerCredentialStatus(ctx, statusRef), true
	}
	return synthesizeReviewerCredentialStatus(ctx, statusRef), true
}

func synthesizeReviewerCredentialStatus(ctx initPromptContext, ref config.CredentialRef) initReviewerCredentialStatus {
	status := initReviewerCredentialStatus{Ref: ref}
	if ctx.ExistingProfile != nil {
		if resolved, err := credentials.ResolveSecretsProfileForProfile(ctx.ExistingConfig, *ctx.ExistingProfile); err == nil {
			status.SecretsProfile = resolved
		} else {
			status.Unavailable = "credential backend status unavailable"
		}
	}
	specs, err := credentials.KeySpecsForPurpose(ref)
	if err != nil {
		status.Unavailable = "credential status unavailable"
		return status
	}
	for _, spec := range specs {
		keyState := initReviewerCredentialKeyMissing
		if !spec.Required {
			keyState = initReviewerCredentialKeyOptional
		}
		status.Keys = append(status.Keys, initReviewerCredentialKeyStatus{
			Key:      spec.Key,
			Required: spec.Required,
			State:    keyState,
		})
	}
	return status
}

func synthesizeUnavailableReviewerCredentialStatus(ctx initPromptContext, ref config.CredentialRef) initReviewerCredentialStatus {
	status := synthesizeReviewerCredentialStatus(ctx, ref)
	status.Unavailable = "credential backend status unavailable"
	for i := range status.Keys {
		status.Keys[i].State = initReviewerCredentialKeyUnavailable
	}
	return status
}

func reviewerCredentialAuthModeForKind(kind initReviewerEntityKind) config.GitAuthMode {
	switch kind {
	case initReviewerEntityKindGitHubApp:
		return config.GitAuthModeGitHubApp
	case initReviewerEntityKindPAT, initReviewerEntityKindUseGitIdentity:
		return config.GitAuthModePAT
	default:
		return config.GitAuthModePAT
	}
}

func initReviewerCredentialStatusDescription(status initReviewerCredentialStatus) string {
	var lines []string
	lines = append(lines, "Destination: "+initReviewerCredentialDestinationDescription(status))
	if strings.TrimSpace(status.Unavailable) != "" {
		lines = append(lines, strings.TrimSpace(status.Unavailable)+".")
	}
	for _, key := range status.Keys {
		lines = append(lines, fmt.Sprintf("- %s: %s", key.Key, key.State))
	}
	return strings.Join(lines, "\n")
}

func initReviewerCredentialDestinationDescription(status initReviewerCredentialStatus) string {
	ref := strings.TrimSpace(status.Ref.Ref)
	if ref == "" {
		ref = "(standard reviewer secret location)"
	}
	backend := strings.TrimSpace(status.SecretsProfile.Backend)
	storeLabel := strings.TrimSpace(status.SecretsProfile.DisplayName())
	switch {
	case storeLabel != "" && backend != "":
		return fmt.Sprintf("%s via %s (%s)", ref, storeLabel, backend)
	case storeLabel != "":
		return fmt.Sprintf("%s via %s", ref, storeLabel)
	case backend != "":
		return fmt.Sprintf("%s via %s", ref, backend)
	default:
		return ref
	}
}
