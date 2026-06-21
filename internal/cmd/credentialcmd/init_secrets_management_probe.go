package credentialcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/open-cli-collective/cli-common/credstore"

	"github.com/open-cli-collective/codereview-cli/internal/config"
)

const initSecretsBackendDiscoveryEnv = "CR_SECRET_BACKEND_DISCOVERY" // #nosec G101 -- environment variable name, not a credential.

type initExecutableLookPath func(string) (string, error)

type initSecretsBackendDiscoveryMode string

const (
	initSecretsBackendDiscoveryModeFull initSecretsBackendDiscoveryMode = "full"
	initSecretsBackendDiscoveryModeSafe initSecretsBackendDiscoveryMode = "safe"
	initSecretsBackendDiscoveryModeOff  initSecretsBackendDiscoveryMode = "off"
)

type initSecretsProbeStatus string

const (
	initSecretsProbeAvailable   initSecretsProbeStatus = "available"
	initSecretsProbeNotFound    initSecretsProbeStatus = "not found"
	initSecretsProbeSkipped     initSecretsProbeStatus = "skipped"
	initSecretsProbeUnavailable initSecretsProbeStatus = "unavailable"
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
	if p.executableLookPath != nil {
		return p.executableLookPath(name)
	}
	return exec.LookPath(name)
}

func initSecretsProbeStatusForError(err error) initSecretsProbeStatus {
	if err == nil {
		return initSecretsProbeAvailable
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return initSecretsProbeNotFound
	}
	return initSecretsProbeUnavailable
}

func initSecretsProbeDetailForError(err error, status initSecretsProbeStatus) string {
	if status != initSecretsProbeUnavailable {
		return ""
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, os.ErrPermission):
		return "permission denied"
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return "command failed"
	}
	var syntaxErr *json.SyntaxError
	if errors.As(err, &syntaxErr) {
		return "invalid JSON"
	}
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		return "invalid JSON"
	}
	return ""
}

func initSecretsPlural(count int, singular, plural string) string {
	if count == 1 {
		return singular
	}
	return plural
}
