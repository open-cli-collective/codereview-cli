package credentialcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
)

const initOnePasswordManualSelection = "__manual_1password__"

type initOnePasswordCommandRunner func(context.Context, string, ...string) ([]byte, error)

type initOnePasswordDiscovery struct {
	run     initOnePasswordCommandRunner
	timeout time.Duration
}

type initOnePasswordDesktopDiscovery struct {
	Accounts []initOnePasswordDiscoveredAccount
	Err      error
}

type initOnePasswordDiscoveredAccount struct {
	ID        string
	Name      string
	URL       string
	Shorthand string
	Email     string
	Vaults    []initOnePasswordDiscoveredVault
}

type initOnePasswordDiscoveredVault struct {
	ID   string
	Name string
}

type initOnePasswordDesktopSelection struct {
	AccountID  string
	AccountURL string
	VaultID    string
	VaultName  string
}

func newInitOnePasswordDiscovery(run initOnePasswordCommandRunner) initOnePasswordDiscovery {
	if run == nil {
		run = runInitOnePasswordCommand
	}
	return initOnePasswordDiscovery{run: run, timeout: 5 * time.Second}
}

func (p huhInitKeyringBackendPrompter) discoverOnePasswordDesktop() initOnePasswordDesktopDiscovery {
	return newInitOnePasswordDiscovery(p.onePasswordCmdRunner).DiscoverDesktop(context.Background())
}

func runInitOnePasswordCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) // #nosec G204 -- this probe intentionally shells out to the configured 1Password CLI executable.
	return cmd.Output()
}

func (d initOnePasswordDiscovery) DiscoverDesktop(ctx context.Context) initOnePasswordDesktopDiscovery {
	if d.timeout <= 0 {
		d.timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()

	accountOutput, err := d.run(ctx, "op", "account", "list", "--format=json")
	if err != nil {
		return initOnePasswordDesktopDiscovery{Err: err}
	}
	accounts, err := parseInitOnePasswordAccounts(accountOutput)
	if err != nil {
		return initOnePasswordDesktopDiscovery{Err: err}
	}
	for index := range accounts {
		accountArg := accounts[index].CommandValue()
		if accountArg == "" {
			continue
		}
		vaultOutput, err := d.run(ctx, "op", "vault", "list", "--account", accountArg, "--format=json")
		if err != nil {
			if accounts[index].Vaults == nil {
				accounts[index].Vaults = []initOnePasswordDiscoveredVault{}
			}
			continue
		}
		vaults, err := parseInitOnePasswordVaults(vaultOutput)
		if err == nil {
			accounts[index].Vaults = vaults
		}
	}
	return initOnePasswordDesktopDiscovery{Accounts: accounts}
}

func parseInitOnePasswordAccounts(data []byte) ([]initOnePasswordDiscoveredAccount, error) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	accounts := make([]initOnePasswordDiscoveredAccount, 0, len(raw))
	for _, item := range raw {
		account := initOnePasswordDiscoveredAccount{
			ID:        initOnePasswordStringField(item, "account_uuid", "id", "uuid"),
			Name:      initOnePasswordStringField(item, "account_name", "name"),
			URL:       initOnePasswordStringField(item, "url", "account_url", "signin_address"),
			Shorthand: initOnePasswordStringField(item, "shorthand"),
			Email:     initOnePasswordStringField(item, "email"),
		}
		if account.CommandValue() == "" && account.DisplayName() == "" {
			continue
		}
		accounts = append(accounts, account)
	}
	return accounts, nil
}

func parseInitOnePasswordVaults(data []byte) ([]initOnePasswordDiscoveredVault, error) {
	var raw []map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	vaults := make([]initOnePasswordDiscoveredVault, 0, len(raw))
	for _, item := range raw {
		vault := initOnePasswordDiscoveredVault{
			ID:   initOnePasswordStringField(item, "id", "uuid"),
			Name: initOnePasswordStringField(item, "name"),
		}
		if vault.ID == "" && vault.Name == "" {
			continue
		}
		vaults = append(vaults, vault)
	}
	return vaults, nil
}

func initOnePasswordStringField(item map[string]any, names ...string) string {
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

func (a initOnePasswordDiscoveredAccount) CommandValue() string {
	for _, value := range []string{a.ID, a.URL, a.Shorthand, a.Email, a.Name} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (a initOnePasswordDiscoveredAccount) DisplayName() string {
	for _, value := range []string{a.URL, a.Name, a.Shorthand, a.Email, a.ID} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (a initOnePasswordDiscoveredAccount) Label() string {
	name := strings.TrimSpace(a.Name)
	url := strings.TrimSpace(a.URL)
	if name != "" && url != "" && name != url {
		return fmt.Sprintf("%s (%s)", name, url)
	}
	if name != "" {
		return name
	}
	return a.DisplayName()
}

func (v initOnePasswordDiscoveredVault) DisplayName() string {
	for _, value := range []string{v.Name, v.ID} {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func (d initOnePasswordDesktopDiscovery) HasVaultChoices() bool {
	for _, account := range d.Accounts {
		if len(account.Vaults) > 0 {
			return true
		}
	}
	return false
}

func (d initOnePasswordDesktopDiscovery) Counts() (int, int) {
	vaults := 0
	for _, account := range d.Accounts {
		vaults += len(account.Vaults)
	}
	return len(d.Accounts), vaults
}

func (d initOnePasswordDesktopDiscovery) AccountChoiceCount() int {
	count := 0
	for _, account := range d.Accounts {
		if len(account.Vaults) > 0 {
			count++
		}
	}
	return count
}

func (d initOnePasswordDesktopDiscovery) AccountOptions() []huh.Option[string] {
	if !d.HasVaultChoices() {
		return []huh.Option[string]{huh.NewOption("Enter account and vault manually", initOnePasswordManualSelection)}
	}
	options := []huh.Option[string]{}
	for accountIndex, account := range d.Accounts {
		if len(account.Vaults) == 0 {
			continue
		}
		label := account.Label()
		if label == "" {
			continue
		}
		options = append(options, huh.NewOption(label, initOnePasswordDesktopAccountSelectionValue(accountIndex)))
	}
	if len(options) != 1 {
		options = append(options, huh.NewOption("Enter account and vault manually", initOnePasswordManualSelection))
	}
	return options
}

func (d initOnePasswordDesktopDiscovery) LinearAccountOptions(selected string) []initLinearOption {
	selected = d.normalizeAccountSelection(selected)
	options := initLinearOptionsFromHuh(d.AccountOptions(), selected)
	if len(options) == 0 {
		return []initLinearOption{{
			Label:    "Enter account and vault manually",
			Value:    initOnePasswordManualSelection,
			Selected: true,
		}}
	}
	return options
}

func (d initOnePasswordDesktopDiscovery) VaultOptions(accountSelection string) []huh.Option[string] {
	account, ok := d.Account(accountSelection)
	if !ok || len(account.Vaults) == 0 {
		return []huh.Option[string]{huh.NewOption("Enter vault manually", initOnePasswordManualSelection)}
	}
	options := []huh.Option[string]{}
	for vaultIndex, vault := range account.Vaults {
		label := vault.DisplayName()
		if label == "" {
			continue
		}
		options = append(options, huh.NewOption(label, initOnePasswordDesktopVaultSelectionValue(vaultIndex)))
	}
	options = append(options, huh.NewOption("Enter vault manually", initOnePasswordManualSelection))
	return options
}

func (d initOnePasswordDesktopDiscovery) LinearVaultOptions(accountSelection, selected string) []initLinearOption {
	accountSelection = d.normalizeAccountSelection(accountSelection)
	selected = d.normalizeVaultSelection(accountSelection, selected)
	options := initLinearOptionsFromHuh(d.VaultOptions(accountSelection), selected)
	if len(options) == 0 {
		return []initLinearOption{{
			Label:    "Enter vault manually",
			Value:    initOnePasswordManualSelection,
			Selected: true,
		}}
	}
	return options
}

func (d initOnePasswordDesktopDiscovery) Options() []huh.Option[string] {
	if !d.HasVaultChoices() {
		return []huh.Option[string]{huh.NewOption("Enter account and vault manually", initOnePasswordManualSelection)}
	}
	options := []huh.Option[string]{}
	for accountIndex, account := range d.Accounts {
		accountLabel := account.DisplayName()
		for vaultIndex, vault := range account.Vaults {
			vaultLabel := vault.DisplayName()
			if vaultLabel == "" {
				continue
			}
			label := vaultLabel
			if accountLabel != "" {
				label = fmt.Sprintf("%s (%s)", vaultLabel, accountLabel)
			}
			options = append(options, huh.NewOption(label, initOnePasswordDesktopSelectionValue(accountIndex, vaultIndex)))
		}
	}
	options = append(options, huh.NewOption("Enter account and vault manually", initOnePasswordManualSelection))
	return options
}

func (d initOnePasswordDesktopDiscovery) LinearOptions(selected string) []initLinearOption {
	return initLinearOptionsFromHuh(d.Options(), selected)
}

func (d initOnePasswordDesktopDiscovery) Account(value string) (initOnePasswordDiscoveredAccount, bool) {
	accountIndex, ok := parseInitOnePasswordDesktopAccountSelectionValue(value)
	if !ok || accountIndex < 0 || accountIndex >= len(d.Accounts) {
		return initOnePasswordDiscoveredAccount{}, false
	}
	account := d.Accounts[accountIndex]
	if len(account.Vaults) == 0 {
		return initOnePasswordDiscoveredAccount{}, false
	}
	return account, true
}

func (d initOnePasswordDesktopDiscovery) AccountVaultSelection(accountSelection, vaultSelection string) (initOnePasswordDesktopSelection, bool) {
	accountIndex, ok := parseInitOnePasswordDesktopAccountSelectionValue(accountSelection)
	if !ok || accountIndex < 0 || accountIndex >= len(d.Accounts) {
		return initOnePasswordDesktopSelection{}, false
	}
	account := d.Accounts[accountIndex]
	vaultIndex, ok := parseInitOnePasswordDesktopVaultSelectionValue(vaultSelection)
	if !ok || vaultIndex < 0 || vaultIndex >= len(account.Vaults) {
		return initOnePasswordDesktopSelection{}, false
	}
	vault := account.Vaults[vaultIndex]
	return initOnePasswordDesktopSelection{
		AccountID:  account.ID,
		AccountURL: account.URL,
		VaultID:    vault.ID,
		VaultName:  vault.Name,
	}, true
}

func (d initOnePasswordDesktopDiscovery) AccountSelection(accountSelection string) (initOnePasswordDesktopSelection, bool) {
	accountIndex, ok := parseInitOnePasswordDesktopAccountSelectionValue(accountSelection)
	if !ok || accountIndex < 0 || accountIndex >= len(d.Accounts) {
		return initOnePasswordDesktopSelection{}, false
	}
	account := d.Accounts[accountIndex]
	return initOnePasswordDesktopSelection{
		AccountID:  account.ID,
		AccountURL: account.URL,
	}, true
}

func (d initOnePasswordDesktopDiscovery) Selection(value string) (initOnePasswordDesktopSelection, bool) {
	accountIndex, vaultIndex, ok := parseInitOnePasswordDesktopSelectionValue(value)
	if !ok || accountIndex < 0 || accountIndex >= len(d.Accounts) {
		return initOnePasswordDesktopSelection{}, false
	}
	account := d.Accounts[accountIndex]
	if vaultIndex < 0 || vaultIndex >= len(account.Vaults) {
		return initOnePasswordDesktopSelection{}, false
	}
	vault := account.Vaults[vaultIndex]
	return initOnePasswordDesktopSelection{
		AccountID:  account.ID,
		AccountURL: account.URL,
		VaultID:    vault.ID,
		VaultName:  vault.Name,
	}, true
}

func (d initOnePasswordDesktopDiscovery) AccountSelectionFor(accountID, accountURL string) string {
	accountID = strings.TrimSpace(accountID)
	accountURL = strings.TrimSpace(accountURL)
	for accountIndex, account := range d.Accounts {
		if len(account.Vaults) == 0 {
			continue
		}
		if accountID != "" && account.ID == accountID {
			return initOnePasswordDesktopAccountSelectionValue(accountIndex)
		}
		if accountURL != "" && account.URL == accountURL {
			return initOnePasswordDesktopAccountSelectionValue(accountIndex)
		}
	}
	if accountID == "" && accountURL == "" {
		for accountIndex, account := range d.Accounts {
			if len(account.Vaults) > 0 {
				return initOnePasswordDesktopAccountSelectionValue(accountIndex)
			}
		}
	}
	return initOnePasswordManualSelection
}

func (d initOnePasswordDesktopDiscovery) VaultSelectionFor(accountSelection, vaultID, vaultName string) string {
	account, ok := d.Account(accountSelection)
	if !ok {
		return initOnePasswordManualSelection
	}
	vaultID = strings.TrimSpace(vaultID)
	vaultName = strings.TrimSpace(vaultName)
	for vaultIndex, vault := range account.Vaults {
		if vaultID != "" && vault.ID == vaultID {
			return initOnePasswordDesktopVaultSelectionValue(vaultIndex)
		}
		if vaultName != "" && vault.Name == vaultName {
			return initOnePasswordDesktopVaultSelectionValue(vaultIndex)
		}
	}
	if vaultID == "" && vaultName == "" && len(account.Vaults) > 0 {
		return initOnePasswordDesktopVaultSelectionValue(0)
	}
	return initOnePasswordManualSelection
}

func (d initOnePasswordDesktopDiscovery) SelectionFor(accountID, accountURL, vaultID, vaultName string) string {
	accountID = strings.TrimSpace(accountID)
	accountURL = strings.TrimSpace(accountURL)
	vaultID = strings.TrimSpace(vaultID)
	vaultName = strings.TrimSpace(vaultName)
	for accountIndex, account := range d.Accounts {
		accountMatches := accountID == "" && accountURL == ""
		if accountID != "" && account.ID == accountID {
			accountMatches = true
		}
		if accountURL != "" && account.URL == accountURL {
			accountMatches = true
		}
		if !accountMatches {
			continue
		}
		for vaultIndex, vault := range account.Vaults {
			if vaultID != "" && vault.ID == vaultID {
				return initOnePasswordDesktopSelectionValue(accountIndex, vaultIndex)
			}
			if vaultName != "" && vault.Name == vaultName {
				return initOnePasswordDesktopSelectionValue(accountIndex, vaultIndex)
			}
		}
	}
	return initOnePasswordManualSelection
}

func (d initOnePasswordDesktopDiscovery) normalizeAccountSelection(selected string) string {
	if selected == initOnePasswordManualSelection && d.AccountChoiceCount() != 1 {
		return selected
	}
	if _, ok := d.Account(selected); ok {
		return selected
	}
	return d.AccountSelectionFor("", "")
}

func (d initOnePasswordDesktopDiscovery) normalizeVaultSelection(accountSelection, selected string) string {
	if selected == initOnePasswordManualSelection {
		return selected
	}
	if _, ok := d.AccountVaultSelection(accountSelection, selected); ok {
		return selected
	}
	return d.VaultSelectionFor(accountSelection, "", "")
}

func initOnePasswordDesktopSelectionValue(accountIndex, vaultIndex int) string {
	return strconv.Itoa(accountIndex) + ":" + strconv.Itoa(vaultIndex)
}

func initOnePasswordDesktopAccountSelectionValue(accountIndex int) string {
	return strconv.Itoa(accountIndex)
}

func initOnePasswordDesktopVaultSelectionValue(vaultIndex int) string {
	return strconv.Itoa(vaultIndex)
}

func parseInitOnePasswordDesktopAccountSelectionValue(value string) (int, bool) {
	if strings.TrimSpace(value) == "" || value == initOnePasswordManualSelection {
		return 0, false
	}
	accountIndex, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return accountIndex, true
}

func parseInitOnePasswordDesktopVaultSelectionValue(value string) (int, bool) {
	if strings.TrimSpace(value) == "" || value == initOnePasswordManualSelection {
		return 0, false
	}
	vaultIndex, err := strconv.Atoi(value)
	if err != nil {
		return 0, false
	}
	return vaultIndex, true
}

func parseInitOnePasswordDesktopSelectionValue(value string) (int, int, bool) {
	parts := strings.Split(value, ":")
	if len(parts) != 2 {
		return 0, 0, false
	}
	accountIndex, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	vaultIndex, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return accountIndex, vaultIndex, true
}
