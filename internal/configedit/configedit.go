// Package configedit provides reusable config mutation helpers.
//
// This package owns pure config data transforms only. Keep command parsing,
// Cobra/view dependencies, keyring operations, and exit-code mapping in command
// packages such as configcmd.
package configedit

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

var (
	// ErrRouteHostRequired means a repository route host was omitted.
	ErrRouteHostRequired = errors.New("route host is required")
	// ErrRouteNamespaceRequired means a repository route namespace was omitted.
	ErrRouteNamespaceRequired = errors.New("route namespace is required")
	// ErrRouteRepoRequired means an explicitly supplied repository route repo was blank.
	ErrRouteRepoRequired = errors.New("route repo is required")
	// ErrAgentSourcePathRequired means an agent source path was omitted.
	ErrAgentSourcePathRequired = errors.New("agent source path is required")
	// ErrProfileNameRequired means a profile name was omitted.
	ErrProfileNameRequired = errors.New("profile name is required")
	// ErrProfileExists means the destination profile already exists.
	ErrProfileExists = errors.New("profile already exists")
)

// RepositoryRouteSpec identifies one namespace route or one set of repo routes.
type RepositoryRouteSpec struct {
	Host      string
	Namespace string
	Repos     []string
}

type repositoryRouteState struct {
	namespace map[string]map[string]struct{}
	repos     map[string]map[string]struct{}
}

// NormalizeRepositoryRouteSpec returns a normalized copy of spec.
func NormalizeRepositoryRouteSpec(spec RepositoryRouteSpec) (RepositoryRouteSpec, error) {
	return normalizeRepositoryRouteSpec(spec)
}

func normalizeRepositoryRouteSpec(spec RepositoryRouteSpec) (RepositoryRouteSpec, error) {
	host := config.NormalizeHost(spec.Host)
	if host == "" {
		return RepositoryRouteSpec{}, ErrRouteHostRequired
	}
	namespace := strings.TrimSpace(spec.Namespace)
	if namespace == "" {
		return RepositoryRouteSpec{}, ErrRouteNamespaceRequired
	}
	repos, err := NormalizeRepositoryRouteRepos(spec.Repos)
	if err != nil {
		return RepositoryRouteSpec{}, err
	}
	return RepositoryRouteSpec{Host: host, Namespace: namespace, Repos: repos}, nil
}

// NormalizeRepositoryRouteRepos trims, dedupes, and sorts repository names.
func NormalizeRepositoryRouteRepos(raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(raw))
	repos := make([]string, 0, len(raw))
	for _, repo := range raw {
		trimmed := strings.TrimSpace(repo)
		if trimmed == "" {
			return nil, ErrRouteRepoRequired
		}
		if _, ok := seen[trimmed]; ok {
			continue
		}
		seen[trimmed] = struct{}{}
		repos = append(repos, trimmed)
	}
	sort.Strings(repos)
	return repos, nil
}

// FormatRepositoryRouteSpec returns a stable domain representation of a route.
func FormatRepositoryRouteSpec(spec RepositoryRouteSpec) string {
	target := spec.Host + "/" + spec.Namespace
	if len(spec.Repos) == 0 {
		return target
	}
	return target + " [" + strings.Join(spec.Repos, ", ") + "]"
}

// CanonicalRepositoryRoutes returns normalized repository routes in stable order.
func CanonicalRepositoryRoutes(routes []config.RepositoryProfile) []config.RepositoryProfile {
	state := newRepositoryRouteState(routes)
	return state.routes()
}

// SetRepositoryRoutes assigns the specified routes to profileName.
func SetRepositoryRoutes(routes []config.RepositoryProfile, profileName string, spec RepositoryRouteSpec) ([]config.RepositoryProfile, error) {
	spec, err := normalizeRepositoryRouteSpec(spec)
	if err != nil {
		return nil, err
	}
	state := newRepositoryRouteState(routes)
	if len(spec.Repos) == 0 {
		state.addNamespace(spec.Host, spec.Namespace, profileName)
		return state.routes(), nil
	}
	for _, repo := range spec.Repos {
		state.addRepo(spec.Host, spec.Namespace, repo, profileName)
	}
	return state.routes(), nil
}

// UnsetRepositoryRoutes removes the specified routes.
func UnsetRepositoryRoutes(routes []config.RepositoryProfile, spec RepositoryRouteSpec) ([]config.RepositoryProfile, bool, error) {
	spec, err := normalizeRepositoryRouteSpec(spec)
	if err != nil {
		return nil, false, err
	}
	state := newRepositoryRouteState(routes)
	changed := false
	if len(spec.Repos) == 0 {
		key := routeKey(spec.Host, spec.Namespace, "")
		if _, ok := state.namespace[key]; ok {
			delete(state.namespace, key)
			changed = true
		}
		return state.routes(), changed, nil
	}
	for _, repo := range spec.Repos {
		key := routeKey(spec.Host, spec.Namespace, repo)
		if _, ok := state.repos[key]; ok {
			delete(state.repos, key)
			changed = true
		}
	}
	return state.routes(), changed, nil
}

// UnsetRepositoryRoutesForProfile removes the specified routes only for profileName.
func UnsetRepositoryRoutesForProfile(routes []config.RepositoryProfile, profileName string, spec RepositoryRouteSpec) ([]config.RepositoryProfile, bool, error) {
	profileName = strings.TrimSpace(profileName)
	if profileName == "" {
		return nil, false, ErrProfileNameRequired
	}
	spec, err := normalizeRepositoryRouteSpec(spec)
	if err != nil {
		return nil, false, err
	}
	state := newRepositoryRouteState(routes)
	changed := false
	if len(spec.Repos) == 0 {
		key := routeKey(spec.Host, spec.Namespace, "")
		if state.removeProfile(state.namespace, key, profileName) {
			changed = true
		}
		return state.routes(), changed, nil
	}
	for _, repo := range spec.Repos {
		key := routeKey(spec.Host, spec.Namespace, repo)
		if state.removeProfile(state.repos, key, profileName) {
			changed = true
		}
	}
	return state.routes(), changed, nil
}

// PruneRepositoryProfileRoutes removes all repository routes pointing at profileName.
func PruneRepositoryProfileRoutes(routes []config.RepositoryProfile, profileName string) []config.RepositoryProfile {
	if len(routes) == 0 {
		return routes
	}
	pruned := make([]config.RepositoryProfile, 0, len(routes))
	for _, route := range routes {
		if route.Profile != profileName {
			pruned = append(pruned, route)
		}
	}
	if len(pruned) == 0 {
		return nil
	}
	return pruned
}

// UpdateRepositoryProfileReferences renames route profile references.
func UpdateRepositoryProfileReferences(routes []config.RepositoryProfile, oldName string, newName string) ([]config.RepositoryProfile, bool) {
	if len(routes) == 0 || oldName == newName {
		return routes, false
	}
	out := make([]config.RepositoryProfile, len(routes))
	changed := false
	for index, route := range routes {
		if route.Profile == oldName {
			route.Profile = newName
			changed = true
		}
		out[index] = route
	}
	if !changed {
		return routes, false
	}
	return out, true
}

// NormalizeAgentSourcePath trims and cleans an agent source path.
func NormalizeAgentSourcePath(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrAgentSourcePathRequired
	}
	return filepath.Clean(trimmed), nil
}

// AddAgentSource appends raw when an equivalent normalized path is absent.
func AddAgentSource(existing []string, raw string) ([]string, bool, error) {
	target, err := NormalizeAgentSourcePath(raw)
	if err != nil {
		return nil, false, err
	}
	out := append([]string(nil), existing...)
	for _, source := range existing {
		normalized, err := NormalizeAgentSourcePath(source)
		if err != nil {
			continue
		}
		if normalized == target {
			return out, false, nil
		}
	}
	out = append(out, target)
	return out, true, nil
}

// RemoveAgentSource removes entries that normalize to raw.
func RemoveAgentSource(existing []string, raw string) ([]string, bool, error) {
	target, err := NormalizeAgentSourcePath(raw)
	if err != nil {
		return nil, false, err
	}
	out := make([]string, 0, len(existing))
	changed := false
	for _, source := range existing {
		normalized, err := NormalizeAgentSourcePath(source)
		if err == nil && normalized == target {
			changed = true
			continue
		}
		out = append(out, source)
	}
	if !changed {
		return append([]string(nil), existing...), false, nil
	}
	if len(out) == 0 {
		return nil, true, nil
	}
	return out, true, nil
}

// FirstProfileName returns the lexicographically first profile name.
func FirstProfileName(profiles map[string]config.Profile) string {
	if len(profiles) == 0 {
		return ""
	}
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names[0]
}

// RenameProfile renames a profile and updates route references.
func RenameProfile(cfg config.File, oldName string, newName string) (config.File, bool, error) {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || newName == "" {
		return cfg, false, ErrProfileNameRequired
	}
	profile, ok := cfg.Profiles[oldName]
	if !ok {
		return cfg, false, fmt.Errorf("%w: %s", config.ErrProfileNotFound, oldName)
	}
	if oldName == newName {
		return cfg, false, nil
	}
	if _, exists := cfg.Profiles[newName]; exists {
		return cfg, false, fmt.Errorf("%w: %s", ErrProfileExists, newName)
	}

	profiles := make(map[string]config.Profile, len(cfg.Profiles))
	for name, item := range cfg.Profiles {
		if name == oldName {
			continue
		}
		profiles[name] = item
	}
	profiles[newName] = profile
	cfg.Profiles = profiles
	cfg.RepositoryProfiles, _ = UpdateRepositoryProfileReferences(cfg.RepositoryProfiles, oldName, newName)
	return cfg, true, nil
}

func newRepositoryRouteState(routes []config.RepositoryProfile) repositoryRouteState {
	state := repositoryRouteState{
		namespace: map[string]map[string]struct{}{},
		repos:     map[string]map[string]struct{}{},
	}
	for _, route := range canonicalRepositoryRoutesFromConfig(routes) {
		if len(route.Match.Repos) == 0 {
			state.addNamespace(route.Match.Host, route.Match.Namespace, route.Profile)
			continue
		}
		for _, repo := range route.Match.Repos {
			state.addRepo(route.Match.Host, route.Match.Namespace, repo, route.Profile)
		}
	}
	return state
}

func canonicalRepositoryRoutesFromConfig(routes []config.RepositoryProfile) []config.RepositoryProfile {
	if len(routes) == 0 {
		return nil
	}
	canonical := make([]config.RepositoryProfile, len(routes))
	for i, route := range routes {
		canonical[i] = route
		canonical[i].Profile = strings.TrimSpace(route.Profile)
		canonical[i].Match.Host = config.NormalizeHost(route.Match.Host)
		canonical[i].Match.Namespace = strings.TrimSpace(route.Match.Namespace)
		if len(route.Match.Repos) > 0 {
			repos, err := NormalizeRepositoryRouteRepos(route.Match.Repos)
			if err != nil {
				canonical[i].Match.Repos = nil
			} else {
				canonical[i].Match.Repos = repos
			}
		}
	}
	return canonical
}

func (s repositoryRouteState) routes() []config.RepositoryProfile {
	namespaceKeys := make([]string, 0, len(s.namespace))
	for key := range s.namespace {
		namespaceKeys = append(namespaceKeys, key)
	}
	sort.Strings(namespaceKeys)

	type repoGroup struct {
		profile   string
		host      string
		namespace string
		repos     []string
	}
	repoGroupsByKey := map[string]*repoGroup{}
	for key, profiles := range s.repos {
		host, namespace, repo := splitRouteKey(key)
		for _, profile := range sortedProfileSet(profiles) {
			profileGroupKey := routeKey(host, namespace, profile)
			group := repoGroupsByKey[profileGroupKey]
			if group == nil {
				group = &repoGroup{profile: profile, host: host, namespace: namespace}
				repoGroupsByKey[profileGroupKey] = group
			}
			group.repos = append(group.repos, repo)
		}
	}
	repoGroupKeys := make([]string, 0, len(repoGroupsByKey))
	for key := range repoGroupsByKey {
		repoGroupKeys = append(repoGroupKeys, key)
	}
	sort.Strings(repoGroupKeys)

	routes := make([]config.RepositoryProfile, 0, len(namespaceKeys)+len(repoGroupKeys))
	for _, key := range namespaceKeys {
		host, namespace, _ := splitRouteKey(key)
		for _, profile := range sortedProfileSet(s.namespace[key]) {
			routes = append(routes, config.RepositoryProfile{
				Profile: profile,
				Match: config.RepositoryProfileMatch{
					Host:      host,
					Namespace: namespace,
				},
			})
		}
	}
	for _, key := range repoGroupKeys {
		group := repoGroupsByKey[key]
		sort.Strings(group.repos)
		routes = append(routes, config.RepositoryProfile{
			Profile: group.profile,
			Match: config.RepositoryProfileMatch{
				Host:      group.host,
				Namespace: group.namespace,
				Repos:     group.repos,
			},
		})
	}
	return routes
}

func (s repositoryRouteState) addNamespace(host, namespace, profile string) {
	s.addProfile(s.namespace, routeKey(host, namespace, ""), profile)
}

func (s repositoryRouteState) addRepo(host, namespace, repo, profile string) {
	s.addProfile(s.repos, routeKey(host, namespace, repo), profile)
}

func (s repositoryRouteState) addProfile(profilesByKey map[string]map[string]struct{}, key, profile string) {
	profile = strings.TrimSpace(profile)
	if profilesByKey[key] == nil {
		profilesByKey[key] = map[string]struct{}{}
	}
	profilesByKey[key][profile] = struct{}{}
}

func (s repositoryRouteState) removeProfile(profilesByKey map[string]map[string]struct{}, key, profile string) bool {
	profiles, ok := profilesByKey[key]
	if !ok {
		return false
	}
	if _, ok := profiles[profile]; !ok {
		return false
	}
	delete(profiles, profile)
	if len(profiles) == 0 {
		delete(profilesByKey, key)
	}
	return true
}

func sortedProfileSet(profiles map[string]struct{}) []string {
	out := make([]string, 0, len(profiles))
	for profile := range profiles {
		out = append(out, profile)
	}
	sort.Strings(out)
	return out
}

const routeKeySeparator = "\x00"

func routeKey(host, namespace, repo string) string {
	// NUL is collision-safe here because normalized config strings cannot
	// contain embedded NUL bytes.
	return host + routeKeySeparator + namespace + routeKeySeparator + repo
}

func splitRouteKey(key string) (string, string, string) {
	parts := strings.SplitN(key, routeKeySeparator, 3)
	if len(parts) != 3 {
		return "", "", ""
	}
	return parts[0], parts[1], parts[2]
}
