package credentials

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

// PlanState describes the credential transition init must apply.
type PlanState string

const (
	// PlanStateKeepExisting preserves an unchanged credential reference.
	PlanStateKeepExisting PlanState = "keep_existing"
	// PlanStateDefer leaves credential setup for a later command.
	PlanStateDefer PlanState = "defer"
	// PlanStateOverwriteRef replaces a changed credential reference.
	// #nosec G101 -- planner state label, not secret material.
	PlanStateOverwriteRef PlanState = "overwrite_ref"
	// PlanStateWrite writes the staged credential keys.
	PlanStateWrite PlanState = "write"
	// PlanStateClearRef removes a credential reference no longer desired.
	PlanStateClearRef PlanState = "clear_ref"
	// PlanStateMissingRequired reports an incomplete staged credential bundle.
	PlanStateMissingRequired PlanState = "missing_required"
)

// PlanEntry describes one pure credential transition.
type PlanEntry struct {
	Ref                 config.CredentialRef
	PreviousRef         *config.CredentialRef
	SecretsStore        ResolvedSecretsStore
	KeySpecs            []KeySpec
	PlannedWriteKeys    []string
	MissingRequiredKeys []string
	State               PlanState
}

// StoreIdentity is the comparable identity of a resolved credential store.
type StoreIdentity struct {
	ID      string
	Source  config.EffectiveSecretsStoreSource
	Backend string
}

// Identity returns the comparable identity used to group resolved stores.
func (r ResolvedSecretsStore) Identity() StoreIdentity {
	return StoreIdentity{ID: r.ID, Source: r.Source, Backend: r.Backend}
}

// WriteGroup collects staged writes targeting one resolved credential store.
type WriteGroup struct {
	Resolved      ResolvedSecretsStore
	Writes        map[string]map[string]string
	OverwriteRefs map[string]bool
}

// ReviewerCredentialCleanupGroup collects reviewer refs requiring stale-key cleanup in one store.
type ReviewerCredentialCleanupGroup struct {
	Resolved ResolvedSecretsStore
	Entries  map[string][]PlanEntry
}

// Plan compares previous and desired profiles using the built-in store configuration.
func Plan(previousProfile *config.Profile, desiredProfile config.Profile, plannedWriteKeys map[string][]string) ([]PlanEntry, error) {
	return PlanWithConfig(config.File{}, previousProfile, desiredProfile, plannedWriteKeys)
}

// PlanWithConfig compares previous and desired profiles and resolves each target store.
func PlanWithConfig(cfg config.File, previousProfile *config.Profile, desiredProfile config.Profile, plannedWriteKeys map[string][]string) ([]PlanEntry, error) {
	desiredRefs, err := config.CredentialRefs(desiredProfile)
	if err != nil {
		return nil, err
	}
	var previousRefs []config.CredentialRef
	if previousProfile != nil {
		previousRefs, err = config.CredentialRefs(*previousProfile)
		if err != nil {
			return nil, err
		}
	}

	previousByPurpose := make(map[string]config.CredentialRef, len(previousRefs))
	for _, ref := range previousRefs {
		previousByPurpose[ref.Purpose] = ref
	}

	entries := make([]PlanEntry, 0, len(desiredRefs)+len(previousRefs))
	for _, ref := range desiredRefs {
		resolvedStore, err := ResolveCredentialStore(cfg, ref.Store)
		if err != nil {
			return nil, err
		}
		specs, err := KeySpecsForPurpose(ref)
		if err != nil {
			return nil, err
		}
		writeKeys := append([]string(nil), plannedWriteKeys[ref.Ref]...)
		if err := ValidatePlannedWriteKeys(ref, specs, writeKeys); err != nil {
			return nil, err
		}
		entry := PlanEntry{
			Ref:              ref,
			SecretsStore:     resolvedStore,
			KeySpecs:         append([]KeySpec(nil), specs...),
			PlannedWriteKeys: writeKeys,
		}
		if previousRef, ok := previousByPurpose[ref.Purpose]; ok {
			previousCopy := previousRef
			entry.PreviousRef = &previousCopy
			delete(previousByPurpose, ref.Purpose)
		}
		entry.MissingRequiredKeys = MissingRequiredPlannedKeys(entry.KeySpecs, entry.PlannedWriteKeys)
		entry.State = ClassifyPlanEntry(entry)
		entries = append(entries, entry)
	}
	for _, ref := range previousRefs {
		if _, ok := previousByPurpose[ref.Purpose]; !ok {
			continue
		}
		resolvedStore, err := ResolveCredentialStore(cfg, ref.Store)
		if err != nil {
			return nil, err
		}
		entries = append(entries, PlanEntry{
			Ref:          ref,
			SecretsStore: resolvedStore,
			State:        PlanStateClearRef,
		})
	}
	return entries, nil
}

// ValidatePlannedWriteKeys rejects staged keys outside a credential purpose's key specs.
func ValidatePlannedWriteKeys(ref config.CredentialRef, specs []KeySpec, plannedWriteKeys []string) error {
	if len(plannedWriteKeys) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		allowed[spec.Key] = struct{}{}
	}
	for _, key := range plannedWriteKeys {
		if _, ok := allowed[key]; ok {
			continue
		}
		return fmt.Errorf("init credential planner: unexpected planned write key %q for %s credential name %q", key, ref.Purpose, ref.Ref)
	}
	return nil
}

// MissingRequiredPlannedKeys returns required keys absent from a non-empty staged bundle.
func MissingRequiredPlannedKeys(specs []KeySpec, plannedWriteKeys []string) []string {
	if len(plannedWriteKeys) == 0 {
		return nil
	}
	present := make(map[string]struct{}, len(plannedWriteKeys))
	for _, key := range plannedWriteKeys {
		present[key] = struct{}{}
	}
	var missing []string
	for _, spec := range specs {
		if !spec.Required {
			continue
		}
		if _, ok := present[spec.Key]; !ok {
			missing = append(missing, spec.Key)
		}
	}
	return missing
}

// ClassifyPlanEntry returns the transition state for a planned entry.
func ClassifyPlanEntry(entry PlanEntry) PlanState {
	if len(entry.MissingRequiredKeys) > 0 {
		return PlanStateMissingRequired
	}
	if len(entry.PlannedWriteKeys) > 0 {
		return PlanStateWrite
	}
	if entry.PreviousRef == nil {
		return PlanStateDefer
	}
	if entry.PreviousRef.Purpose == entry.Ref.Purpose &&
		entry.PreviousRef.Store == entry.Ref.Store &&
		entry.PreviousRef.Ref == entry.Ref.Ref &&
		entry.PreviousRef.Mode == entry.Ref.Mode &&
		entry.PreviousRef.Provider == entry.Ref.Provider {
		return PlanStateKeepExisting
	}
	return PlanStateOverwriteRef
}

// GroupWritesByStore groups staged credential bundles by comparable store identity.
func GroupWritesByStore(entries []PlanEntry, writes map[string]map[string]string, overwriteRefs map[string]bool) ([]WriteGroup, error) {
	if len(entries) == 0 {
		return []WriteGroup{{Writes: writes, OverwriteRefs: overwriteRefs}}, nil
	}
	groups := map[StoreIdentity]*WriteGroup{}
	for _, entry := range entries {
		bundle := writes[entry.Ref.Ref]
		if len(bundle) == 0 {
			continue
		}
		identity := entry.SecretsStore.Identity()
		group := groups[identity]
		if group == nil {
			group = &WriteGroup{
				Resolved:      entry.SecretsStore,
				Writes:        map[string]map[string]string{},
				OverwriteRefs: map[string]bool{},
			}
			groups[identity] = group
		}
		group.Writes[entry.Ref.Ref] = bundle
		if overwriteRefs[entry.Ref.Ref] {
			group.OverwriteRefs[entry.Ref.Ref] = true
		}
	}
	identities := slices.Collect(maps.Keys(groups))
	sortStoreIdentities(identities)
	out := make([]WriteGroup, 0, len(identities))
	for _, identity := range identities {
		out = append(out, *groups[identity])
	}
	return out, nil
}

// GroupStaleReviewerCredentialCleanupsByStore finds in-place reviewer auth changes and protects keys used by every active profile.
func GroupStaleReviewerCredentialCleanupsByStore(cfg config.File, entries []PlanEntry) ([]ReviewerCredentialCleanupGroup, error) {
	activeByStoreRef, resolvedByStore, err := activeReviewerCredentialEntriesByStoreRef(cfg, entries)
	if err != nil {
		return nil, err
	}
	cleanupRefsByStore := map[StoreIdentity]map[string]struct{}{}
	for _, entry := range entries {
		if !credentialEntryChangesReviewerModeInPlace(entry) {
			continue
		}
		identity := entry.SecretsStore.Identity()
		if cleanupRefsByStore[identity] == nil {
			cleanupRefsByStore[identity] = map[string]struct{}{}
		}
		cleanupRefsByStore[identity][entry.Ref.Ref] = struct{}{}
	}

	identities := slices.Collect(maps.Keys(cleanupRefsByStore))
	sortStoreIdentities(identities)
	groups := make([]ReviewerCredentialCleanupGroup, 0, len(identities))
	for _, identity := range identities {
		group := ReviewerCredentialCleanupGroup{
			Resolved: resolvedByStore[identity],
			Entries:  map[string][]PlanEntry{},
		}
		for _, ref := range slices.Sorted(maps.Keys(cleanupRefsByStore[identity])) {
			activeEntries := activeByStoreRef[identity][ref]
			if len(activeEntries) > 0 {
				group.Entries[ref] = activeEntries
			}
		}
		if len(group.Entries) > 0 {
			groups = append(groups, group)
		}
	}
	return groups, nil
}

// StaleReviewerCredentialKeys returns reviewer bundle keys unused by all active entries sharing a ref.
func StaleReviewerCredentialKeys(entries []PlanEntry) []string {
	current := map[string]struct{}{}
	for _, entry := range entries {
		for _, spec := range entry.KeySpecs {
			current[spec.Key] = struct{}{}
		}
	}
	candidates := []string{GitTokenKey, GitHubAppIDKey, GitHubAppPrivateKeyKey, GitHubAppInstallationIDKey}
	var stale []string
	for _, key := range candidates {
		if _, ok := current[key]; !ok {
			stale = append(stale, key)
		}
	}
	return stale
}

func activeReviewerCredentialEntriesByStoreRef(cfg config.File, fallback []PlanEntry) (map[StoreIdentity]map[string][]PlanEntry, map[StoreIdentity]ResolvedSecretsStore, error) {
	activeByStoreRef := map[StoreIdentity]map[string][]PlanEntry{}
	resolvedByStore := map[StoreIdentity]ResolvedSecretsStore{}
	if len(cfg.Profiles) == 0 {
		for _, entry := range fallback {
			if credentialEntryUsesActiveReviewerRef(entry) {
				addActiveReviewerCredentialEntry(activeByStoreRef, resolvedByStore, entry)
			}
		}
		return activeByStoreRef, resolvedByStore, nil
	}
	for _, profileName := range slices.Sorted(maps.Keys(cfg.Profiles)) {
		refs, err := config.CredentialRefs(cfg.Profiles[profileName])
		if err != nil {
			return nil, nil, err
		}
		for _, ref := range refs {
			if ref.Purpose != "reviewer_credentials" || strings.TrimSpace(ref.Ref) == "" {
				continue
			}
			resolved, err := ResolveCredentialStore(cfg, ref.Store)
			if err != nil {
				return nil, nil, err
			}
			specs, err := KeySpecsForPurpose(ref)
			if err != nil {
				return nil, nil, err
			}
			addActiveReviewerCredentialEntry(activeByStoreRef, resolvedByStore, PlanEntry{
				Ref: ref, SecretsStore: resolved, KeySpecs: append([]KeySpec(nil), specs...), State: PlanStateKeepExisting,
			})
		}
	}
	return activeByStoreRef, resolvedByStore, nil
}

func addActiveReviewerCredentialEntry(active map[StoreIdentity]map[string][]PlanEntry, resolved map[StoreIdentity]ResolvedSecretsStore, entry PlanEntry) {
	identity := entry.SecretsStore.Identity()
	resolved[identity] = entry.SecretsStore
	if active[identity] == nil {
		active[identity] = map[string][]PlanEntry{}
	}
	active[identity][entry.Ref.Ref] = append(active[identity][entry.Ref.Ref], entry)
}

func credentialEntryUsesActiveReviewerRef(entry PlanEntry) bool {
	return entry.State != PlanStateClearRef && entry.Ref.Purpose == "reviewer_credentials" && strings.TrimSpace(entry.Ref.Ref) != ""
}

func credentialEntryChangesReviewerModeInPlace(entry PlanEntry) bool {
	return credentialEntryUsesActiveReviewerRef(entry) && entry.PreviousRef != nil &&
		entry.PreviousRef.Purpose == entry.Ref.Purpose &&
		entry.PreviousRef.Ref == entry.Ref.Ref &&
		entry.PreviousRef.Mode != entry.Ref.Mode
}

func sortStoreIdentities(identities []StoreIdentity) {
	slices.SortFunc(identities, func(a, b StoreIdentity) int {
		if n := cmp.Compare(a.ID, b.ID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.Source, b.Source); n != 0 {
			return n
		}
		return cmp.Compare(a.Backend, b.Backend)
	})
}
