package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// DefaultOnePasswordDiscoveryCommandTimeout bounds each 1Password CLI discovery command.
const DefaultOnePasswordDiscoveryCommandTimeout = 30 * time.Second

// OnePasswordCommandRunner executes a 1Password CLI command and returns its stdout.
type OnePasswordCommandRunner func(context.Context, string, ...string) ([]byte, error)

// ExecutableLookPath resolves an executable name.
type ExecutableLookPath func(string) (string, error)

// OnePasswordDiscovery discovers desktop-app accounts and vaults through the 1Password CLI.
type OnePasswordDiscovery struct {
	run     OnePasswordCommandRunner
	timeout time.Duration
}

// OnePasswordDesktopDiscovery contains discovered 1Password accounts or the account-list error.
type OnePasswordDesktopDiscovery struct {
	Accounts []OnePasswordDiscoveredAccount
	Err      error
}

// OnePasswordDiscoveredAccount describes an account returned by the 1Password CLI.
type OnePasswordDiscoveredAccount struct {
	ID        string
	Name      string
	URL       string
	Shorthand string
	Email     string
	Vaults    []OnePasswordDiscoveredVault
}

// OnePasswordDiscoveredVault describes a vault returned by the 1Password CLI.
type OnePasswordDiscoveredVault struct {
	ID   string
	Name string
}

// NewOnePasswordDiscovery builds a 1Password CLI discovery probe.
// A nil runner uses os/exec, and a non-positive timeout uses the default.
func NewOnePasswordDiscovery(run OnePasswordCommandRunner, timeout time.Duration) OnePasswordDiscovery {
	if run == nil {
		run = runOnePasswordCommand
	}
	if timeout <= 0 {
		timeout = DefaultOnePasswordDiscoveryCommandTimeout
	}
	return OnePasswordDiscovery{run: run, timeout: timeout}
}

func runOnePasswordCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- this probe intentionally shells out to the configured 1Password CLI executable.
	return cmd.Output()
}

// FindExecutable resolves an executable with the supplied lookup or os/exec by default.
func FindExecutable(name string, lookPath ExecutableLookPath) (string, error) {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	return lookPath(name)
}

// DiscoverDesktop lists 1Password accounts and the accessible vaults in each account.
func (d OnePasswordDiscovery) DiscoverDesktop(ctx context.Context) OnePasswordDesktopDiscovery {
	accountOutput, err := d.runWithTimeout(ctx, "op", "account", "list", "--format=json")
	if err != nil {
		return OnePasswordDesktopDiscovery{Err: err}
	}
	accounts, err := parseOnePasswordAccounts(accountOutput)
	if err != nil {
		return OnePasswordDesktopDiscovery{Err: err}
	}
	for index := range accounts {
		accountArg := accounts[index].CommandValue()
		if accountArg == "" {
			continue
		}
		vaultOutput, err := d.runWithTimeout(ctx, "op", "vault", "list", "--account", accountArg, "--format=json")
		if err != nil {
			if accounts[index].Vaults == nil {
				accounts[index].Vaults = []OnePasswordDiscoveredVault{}
			}
			continue
		}
		vaults, err := ParseOnePasswordVaults(vaultOutput)
		if err == nil {
			accounts[index].Vaults = vaults
		}
	}
	return OnePasswordDesktopDiscovery{Accounts: accounts}
}

func (d OnePasswordDiscovery) runWithTimeout(ctx context.Context, name string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return d.run(commandCtx, name, args...)
}

func parseOnePasswordAccounts(data []byte) ([]OnePasswordDiscoveredAccount, error) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	accounts := make([]OnePasswordDiscoveredAccount, 0, len(raw))
	for _, item := range raw {
		account := OnePasswordDiscoveredAccount{
			ID:        onePasswordStringField(item, "account_uuid", "id", "uuid"),
			Name:      onePasswordStringField(item, "account_name", "name"),
			URL:       onePasswordStringField(item, "url", "account_url", "signin_address"),
			Shorthand: onePasswordStringField(item, "shorthand"),
			Email:     onePasswordStringField(item, "email"),
		}
		if account.CommandValue() == "" && account.DisplayName() == "" {
			continue
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

// ParseOnePasswordVaults decodes and orders a 1Password CLI vault list.
func ParseOnePasswordVaults(data []byte) ([]OnePasswordDiscoveredVault, error) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	vaults := make([]OnePasswordDiscoveredVault, 0, len(raw))
	for _, item := range raw {
		vault := OnePasswordDiscoveredVault{
			ID:   onePasswordStringField(item, "id", "uuid"),
			Name: onePasswordStringField(item, "name"),
		}
		if vault.ID == "" && vault.Name == "" {
			continue
		}
		vaults = append(vaults, vault)
	}
	sort.SliceStable(vaults, func(i, j int) bool {
		return onePasswordVaultLess(vaults[i], vaults[j])
	})
	return vaults, nil
}

func onePasswordVaultLess(left, right OnePasswordDiscoveredVault) bool {
	leftRank := onePasswordVaultPriorityRank(left)
	rightRank := onePasswordVaultPriorityRank(right)
	if leftRank != rightRank {
		return leftRank < rightRank
	}
	leftName := strings.ToLower(left.DisplayName())
	rightName := strings.ToLower(right.DisplayName())
	if leftName != rightName {
		return leftName < rightName
	}
	return left.DisplayName() < right.DisplayName()
}

func onePasswordVaultPriorityRank(vault OnePasswordDiscoveredVault) int {
	switch strings.ToLower(strings.TrimSpace(vault.DisplayName())) {
	case "employee":
		return 0
	case "private":
		return 1
	default:
		return 2
	}
}

func onePasswordStringField(item map[string]any, names ...string) string {
	for _, name := range names {
		value, ok := item[name]
		if !ok {
			continue
		}
		switch typed := value.(type) {
		case string:
			if trimmed := strings.TrimSpace(typed); trimmed != "" {
				return trimmed
			}
		case fmt.Stringer:
			if trimmed := strings.TrimSpace(typed.String()); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// CommandValue returns the best account identifier for an op --account argument.
func (a OnePasswordDiscoveredAccount) CommandValue() string {
	for _, value := range []string{a.ID, a.URL, a.Shorthand, a.Email, a.Name} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// DisplayName returns the best human-readable account identifier.
func (a OnePasswordDiscoveredAccount) DisplayName() string {
	for _, value := range []string{a.URL, a.Name, a.Shorthand, a.Email, a.ID} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// Label returns the account's display label.
func (a OnePasswordDiscoveredAccount) Label() string {
	return a.DisplayName()
}

// DisplayName returns the vault name, falling back to its ID.
func (v OnePasswordDiscoveredVault) DisplayName() string {
	for _, value := range []string{v.Name, v.ID} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// HasVaultChoices reports whether any account has a discovered vault.
func (d OnePasswordDesktopDiscovery) HasVaultChoices() bool {
	for _, account := range d.Accounts {
		if len(account.Vaults) > 0 {
			return true
		}
	}
	return false
}

// HasAccountChoices reports whether discovery returned a displayable account.
func (d OnePasswordDesktopDiscovery) HasAccountChoices() bool {
	return d.AccountChoiceCount() > 0
}

// Counts returns the discovered account and vault counts.
func (d OnePasswordDesktopDiscovery) Counts() (int, int) {
	vaults := 0
	for _, account := range d.Accounts {
		vaults += len(account.Vaults)
	}
	return len(d.Accounts), vaults
}

// AccountChoiceCount returns the number of displayable discovered accounts.
func (d OnePasswordDesktopDiscovery) AccountChoiceCount() int {
	count := 0
	for _, account := range d.Accounts {
		if account.Label() != "" {
			count++
		}
	}
	return count
}

// ProbeStatus classifies credential-backend discovery outcomes.
type ProbeStatus string

const (
	// ProbeAvailable means a credential backend probe succeeded.
	ProbeAvailable ProbeStatus = "available"
	// ProbeNotFound means the probed executable or resource was not found.
	ProbeNotFound ProbeStatus = "not found"
	// ProbeSkipped means discovery policy disabled the probe.
	ProbeSkipped ProbeStatus = "skipped"
	// ProbeUnavailable means the probe failed for another reason.
	ProbeUnavailable ProbeStatus = "unavailable"
)

// ProbeStatusForError classifies a credential-backend probe error.
func ProbeStatusForError(err error) ProbeStatus {
	if err == nil {
		return ProbeAvailable
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return ProbeNotFound
	}
	return ProbeUnavailable
}

// ProbeDetailForError returns the stable user-facing detail for a probe error.
func ProbeDetailForError(err error, status ProbeStatus) string {
	if status != ProbeUnavailable {
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
