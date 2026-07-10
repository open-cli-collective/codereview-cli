package initcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
)

const initOnePasswordManualSelection = "__manual_1password__"
const initOnePasswordDiscoveryCommandTimeout = 30 * time.Second

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
	return initOnePasswordDiscovery{run: run, timeout: initOnePasswordDiscoveryCommandTimeout}
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
		d.timeout = initOnePasswordDiscoveryCommandTimeout
	}

	accountOutput, err := d.runWithTimeout(ctx, "op", "account", "list", "--format=json")
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
		vaultOutput, err := d.runWithTimeout(ctx, "op", "vault", "list", "--account", accountArg, "--format=json")
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

func (d initOnePasswordDiscovery) runWithTimeout(ctx context.Context, name string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, d.timeout)
	defer cancel()
	return d.run(commandCtx, name, args...)
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
	sort.SliceStable(vaults, func(i, j int) bool {
		return initOnePasswordVaultLess(vaults[i], vaults[j])
	})
	return vaults, nil
}

func initOnePasswordVaultLess(left, right initOnePasswordDiscoveredVault) bool {
	leftRank := initOnePasswordVaultPriorityRank(left)
	rightRank := initOnePasswordVaultPriorityRank(right)
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

func initOnePasswordVaultPriorityRank(vault initOnePasswordDiscoveredVault) int {
	switch strings.ToLower(strings.TrimSpace(vault.DisplayName())) {
	case "employee":
		return 0
	case "private":
		return 1
	default:
		return 2
	}
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

func (d initOnePasswordDesktopDiscovery) HasAccountChoices() bool {
	return d.AccountChoiceCount() > 0
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
		if account.Label() != "" {
			count++
		}
	}
	return count
}

func (d initOnePasswordDesktopDiscovery) AccountOptions() []huh.Option[string] {
	if !d.HasAccountChoices() {
		return []huh.Option[string]{huh.NewOption("Enter account and vault manually", initOnePasswordManualSelection)}
	}
	options := []huh.Option[string]{}
	for accountIndex, account := range d.Accounts {
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

func (d initOnePasswordDesktopDiscovery) Account(value string) (initOnePasswordDiscoveredAccount, bool) {
	accountIndex, ok := parseInitOnePasswordDesktopAccountSelectionValue(value)
	if !ok || accountIndex < 0 || accountIndex >= len(d.Accounts) {
		return initOnePasswordDiscoveredAccount{}, false
	}
	account := d.Accounts[accountIndex]
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

func (d initOnePasswordDesktopDiscovery) AccountSelectionFor(accountID, accountURL string) string {
	accountID = strings.TrimSpace(accountID)
	accountURL = strings.TrimSpace(accountURL)
	for accountIndex, account := range d.Accounts {
		if accountID != "" && account.ID == accountID {
			return initOnePasswordDesktopAccountSelectionValue(accountIndex)
		}
		if accountURL != "" && account.URL == accountURL {
			return initOnePasswordDesktopAccountSelectionValue(accountIndex)
		}
	}
	if accountID == "" && accountURL == "" {
		for accountIndex, account := range d.Accounts {
			if account.Label() != "" {
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

func (d initOnePasswordDesktopDiscovery) GeneratedDesktopCredentialStoreLabel(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || value == "1Password" {
		return true
	}
	for _, account := range d.Accounts {
		if value == initOnePasswordDesktopCredentialStoreLabel(account) {
			return true
		}
	}
	return false
}

func initOnePasswordDesktopCredentialStoreLabel(account initOnePasswordDiscoveredAccount) string {
	token := initOnePasswordAccountLabelToken(account.URL)
	if token == "" {
		token = initOnePasswordAccountLabelToken(account.DisplayName())
	}
	if token == "" {
		return "1Password"
	}
	if strings.EqualFold(token, "my") {
		token = "Personal"
	}
	return "1Password-" + token
}

func initOnePasswordAccountLabelToken(value string) string {
	host := strings.TrimSpace(value)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "http://")
	if slash := strings.Index(host, "/"); slash >= 0 {
		host = host[:slash]
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if colon := strings.Index(host, ":"); colon >= 0 {
		host = host[:colon]
	}
	if dot := strings.Index(host, "."); dot >= 0 {
		host = host[:dot]
	}
	return strings.TrimSpace(host)
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
