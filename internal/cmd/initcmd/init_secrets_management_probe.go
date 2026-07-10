package initcmd

import (
	"fmt"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

const initSecretsBackendDiscoveryEnv = "CR_SECRET_BACKEND_DISCOVERY" // #nosec G101 -- environment variable name, not a credential.

type initExecutableLookPath = credentials.ExecutableLookPath

type initSecretsBackendDiscoveryMode string

const (
	initSecretsBackendDiscoveryModeFull initSecretsBackendDiscoveryMode = "full"
	initSecretsBackendDiscoveryModeSafe initSecretsBackendDiscoveryMode = "safe"
	initSecretsBackendDiscoveryModeOff  initSecretsBackendDiscoveryMode = "off"
)

type initSecretsProbeStatus = credentials.ProbeStatus

const (
	initSecretsProbeAvailable   = credentials.ProbeAvailable
	initSecretsProbeNotFound    = credentials.ProbeNotFound
	initSecretsProbeSkipped     = credentials.ProbeSkipped
	initSecretsProbeUnavailable = credentials.ProbeUnavailable
)

type initSecretsProbeResult struct {
	Label  string
	Status initSecretsProbeStatus
	Detail string
}

func (r initSecretsProbeResult) Line() string {
	if r.Detail == "" {
		return fmt.Sprintf("%s: %s", r.Label, r.Status)
	}
	return fmt.Sprintf("%s: %s (%s)", r.Label, r.Status, r.Detail)
}

func (p huhInitKeyringBackendPrompter) resolvedDiscoveryMode() initSecretsBackendDiscoveryMode {
	mode := p.discoveryMode
	if mode == "" {
		return initSecretsBackendDiscoveryModeFull
	}
	return mode
}

func parseInitSecretsBackendDiscoveryMode(value string) (initSecretsBackendDiscoveryMode, error) {
	switch initSecretsBackendDiscoveryMode(strings.TrimSpace(strings.ToLower(value))) {
	case "", initSecretsBackendDiscoveryModeFull:
		return initSecretsBackendDiscoveryModeFull, nil
	case initSecretsBackendDiscoveryModeSafe:
		return initSecretsBackendDiscoveryModeSafe, nil
	case initSecretsBackendDiscoveryModeOff:
		return initSecretsBackendDiscoveryModeOff, nil
	default:
		return "", fmt.Errorf("secret backend discovery %q is invalid; valid values are full, safe, off", value)
	}
}

func (p huhInitKeyringBackendPrompter) discoverOnePasswordDesktopForMode(mode initSecretsBackendDiscoveryMode) initOnePasswordDesktopDiscovery {
	if mode != initSecretsBackendDiscoveryModeFull {
		return initOnePasswordDesktopDiscovery{}
	}
	return p.discoverOnePasswordDesktop()
}

func (p huhInitKeyringBackendPrompter) writeSecretsStorageDiscoveryResults(mode initSecretsBackendDiscoveryMode, discovery initOnePasswordDesktopDiscovery) {
	if p.stderr == nil {
		return
	}
	for _, result := range []initSecretsProbeResult{
		{
			Label:  initBuiltInOSCredentialStoreTitle(),
			Status: initSecretsProbeAvailable,
		},
		p.onePasswordDesktopProbeResult(mode, discovery),
		p.passPasswordStoreProbeResult(mode),
	} {
		_, _ = fmt.Fprintln(p.stderr, result.Line())
	}
	_, _ = fmt.Fprintln(p.stderr)
}

func (p huhInitKeyringBackendPrompter) onePasswordDesktopProbeResult(mode initSecretsBackendDiscoveryMode, discovery initOnePasswordDesktopDiscovery) initSecretsProbeResult {
	result := initSecretsProbeResult{
		Label: initSecretsBackendDisplayLabel(config.SecretsBackendKind(credstore.BackendOPDesktop)),
	}
	if mode != initSecretsBackendDiscoveryModeFull {
		result.Status = initSecretsProbeSkipped
		return result
	}
	if discovery.Err != nil {
		result.Status = initSecretsProbeStatusForError(discovery.Err)
		result.Detail = initSecretsProbeDetailForError(discovery.Err, result.Status)
		return result
	}
	accounts, vaults := discovery.Counts()
	result.Status = initSecretsProbeAvailable
	result.Detail = fmt.Sprintf("%d %s, %d %s", accounts, initSecretsPlural(accounts, "account", "accounts"), vaults, initSecretsPlural(vaults, "vault", "vaults"))
	return result
}

func (p huhInitKeyringBackendPrompter) passPasswordStoreProbeResult(mode initSecretsBackendDiscoveryMode) initSecretsProbeResult {
	result := initSecretsProbeResult{
		Label: initSecretsBackendDisplayLabel(config.SecretsBackendKind(credstore.BackendPass)),
	}
	if mode == initSecretsBackendDiscoveryModeOff {
		result.Status = initSecretsProbeSkipped
		return result
	}
	_, err := p.lookPath("pass")
	if err != nil {
		result.Status = initSecretsProbeStatusForError(err)
		result.Detail = initSecretsProbeDetailForError(err, result.Status)
		return result
	}
	result.Status = initSecretsProbeAvailable
	return result
}

func (p huhInitKeyringBackendPrompter) lookPath(name string) (string, error) {
	return credentials.FindExecutable(name, p.executableLookPath)
}

func initSecretsProbeStatusForError(err error) initSecretsProbeStatus {
	return credentials.ProbeStatusForError(err)
}

func initSecretsProbeDetailForError(err error, status initSecretsProbeStatus) string {
	return credentials.ProbeDetailForError(err, status)
}

func initSecretsPlural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
