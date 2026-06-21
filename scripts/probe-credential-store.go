//go:build ignore

package main

import (
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

func main() {
	var configPath string
	var reviewProfile string
	var secretsProfile string
	var ref string
	var keep bool
	flag.StringVar(&configPath, "config", "", "cr config path; defaults to the standard cr config path")
	flag.StringVar(&reviewProfile, "profile", "", "review profile whose resolved secrets-management profile should be probed")
	flag.StringVar(&secretsProfile, "secrets-profile", "", "configured secrets-management profile id to probe directly")
	flag.StringVar(&ref, "ref", "", "temporary credential ref to write; defaults to codereview/probe-<timestamp>")
	flag.BoolVar(&keep, "keep", false, "leave the probe credential behind instead of deleting it")
	flag.Parse()

	if configPath == "" {
		path, err := config.Path()
		fatalIf(err, "resolve config path")
		configPath = path
	}
	cfg, err := config.Load(configPath)
	fatalIf(err, "load config")

	resolved, err := resolveProbeSecretsProfile(cfg, reviewProfile, secretsProfile)
	fatalIf(err, "resolve secrets-management profile")

	if ref == "" {
		ref = fmt.Sprintf("codereview/probe-%d", time.Now().UnixNano())
	}
	parsed, err := credentials.ParseRef(ref)
	fatalIf(err, "parse probe ref")

	value, err := randomProbeValue()
	fatalIf(err, "generate probe value")

	fmt.Printf("Config: %s\n", configPath)
	fmt.Printf("Secrets profile: %s (%s, backend %s)\n", resolved.DisplayName(), resolved.ID, resolved.Backend)
	fmt.Printf("Probe ref: %s\n", ref)

	store, err := credentials.OpenResolvedStore("", false, cfg, resolved)
	fatalIf(err, "open credential store")
	defer store.Close()

	_, err = store.SetBundle(parsed.Profile, map[string]string{
		credentials.GitTokenKey: value,
	})
	fatalIf(err, "write probe credential")

	got, err := store.Get(parsed.Profile, credentials.GitTokenKey)
	fatalIf(err, "read probe credential")
	if got != value {
		fatalf("read probe credential: value mismatch")
	}

	if keep {
		fmt.Printf("OK: wrote and read probe credential; left it in place because --keep was set\n")
		return
	}
	if _, err := store.DeleteBundle(parsed.Profile); err != nil {
		fatalf("delete probe credential after successful write/read: %v\nmanual cleanup: cr delete-credential --ref %s --key %s", err, ref, credentials.GitTokenKey)
	}
	fmt.Printf("OK: wrote, read, and deleted probe credential\n")
}

func resolveProbeSecretsProfile(cfg config.File, reviewProfile string, secretsProfile string) (credentials.ResolvedSecretsProfile, error) {
	if strings.TrimSpace(secretsProfile) != "" {
		query := strings.TrimSpace(secretsProfile)
		profile, ok := cfg.Secrets.Profiles[query]
		if ok {
			return resolvedProbeSecretsProfile(query, profile), nil
		}
		matches := matchingProbeSecretsProfiles(cfg, query)
		if len(matches) == 1 {
			profileID := matches[0]
			return resolvedProbeSecretsProfile(profileID, cfg.Secrets.Profiles[profileID]), nil
		}
		if len(matches) > 1 {
			return credentials.ResolvedSecretsProfile{}, fmt.Errorf("%w: %s matched multiple secrets-management profiles: %s", config.ErrSecretsProfileNotFound, query, strings.Join(matches, ", "))
		}
		return credentials.ResolvedSecretsProfile{}, fmt.Errorf("%w: %s\nconfigured secrets-management profiles: %s", config.ErrSecretsProfileNotFound, query, availableProbeSecretsProfiles(cfg))
	}
	_, profile, err := config.ResolveProfile(cfg, reviewProfile)
	if err != nil {
		return credentials.ResolvedSecretsProfile{}, err
	}
	return credentials.ResolveSecretsProfileForProfile(cfg, profile)
}

func matchingProbeSecretsProfiles(cfg config.File, query string) []string {
	query = strings.TrimSpace(query)
	var matches []string
	for id, profile := range cfg.Secrets.Profiles {
		if strings.EqualFold(id, query) || strings.EqualFold(strings.TrimSpace(profile.Label), query) {
			matches = append(matches, id)
		}
	}
	sort.Strings(matches)
	return matches
}

func resolvedProbeSecretsProfile(id string, profile config.SecretsProfile) credentials.ResolvedSecretsProfile {
	return credentials.ResolvedSecretsProfile{
		ID:              id,
		Label:           strings.TrimSpace(profile.Label),
		Backend:         string(profile.Backend.Kind),
		Source:          config.EffectiveSecretsProfileSourceConfigured,
		SelectionSource: credentials.SecretsProfileSelectionExplicit,
	}
}

func availableProbeSecretsProfiles(cfg config.File) string {
	if len(cfg.Secrets.Profiles) == 0 {
		return "none configured"
	}
	ids := make([]string, 0, len(cfg.Secrets.Profiles))
	for id := range cfg.Secrets.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		profile := cfg.Secrets.Profiles[id]
		label := strings.TrimSpace(profile.Label)
		if label != "" {
			items = append(items, fmt.Sprintf("%s (%s, backend %s)", id, label, profile.Backend.Kind))
			continue
		}
		items = append(items, fmt.Sprintf("%s (backend %s)", id, profile.Backend.Kind))
	}
	return strings.Join(items, "; ")
}

func randomProbeValue() (string, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", err
	}
	return "cr-probe-" + hex.EncodeToString(bytes[:]), nil
}

func fatalIf(err error, context string) {
	if err != nil {
		fatalf("%s: %v", context, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "probe failed: "+format+"\n", args...)
	os.Exit(1)
}
