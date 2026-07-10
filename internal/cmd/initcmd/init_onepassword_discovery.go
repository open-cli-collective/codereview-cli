package initcmd

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"

	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

const initOnePasswordManualSelection = "__manual_1password__"

type initOnePasswordCommandRunner = credentials.OnePasswordCommandRunner
type initOnePasswordDiscoveredAccount = credentials.OnePasswordDiscoveredAccount
type initOnePasswordDiscoveredVault = credentials.OnePasswordDiscoveredVault

type initOnePasswordDiscovery struct {
	run     initOnePasswordCommandRunner
	timeout time.Duration
}

type initOnePasswordDesktopDiscovery struct {
	Accounts []initOnePasswordDiscoveredAccount
	Err      error
}

type initOnePasswordDesktopSelection struct {
	AccountID  string
	AccountURL string
	VaultID    string
	VaultName  string
}

func newInitOnePasswordDiscovery(run initOnePasswordCommandRunner) initOnePasswordDiscovery {
	return initOnePasswordDiscovery{run: run, timeout: credentials.DefaultOnePasswordDiscoveryCommandTimeout}
}

func (p huhInitKeyringBackendPrompter) discoverOnePasswordDesktop() initOnePasswordDesktopDiscovery {
	return newInitOnePasswordDiscovery(p.onePasswordCmdRunner).DiscoverDesktop(context.Background())
}

func (d initOnePasswordDiscovery) DiscoverDesktop(ctx context.Context) initOnePasswordDesktopDiscovery {
	result := credentials.NewOnePasswordDiscovery(d.run, d.timeout).DiscoverDesktop(ctx)
	return initOnePasswordDesktopDiscovery{Accounts: result.Accounts, Err: result.Err}
}

func (d initOnePasswordDesktopDiscovery) HasVaultChoices() bool {
	return d.domain().HasVaultChoices()
}

func (d initOnePasswordDesktopDiscovery) HasAccountChoices() bool {
	return d.domain().HasAccountChoices()
}

func (d initOnePasswordDesktopDiscovery) Counts() (int, int) {
	return d.domain().Counts()
}

func (d initOnePasswordDesktopDiscovery) AccountChoiceCount() int {
	return d.domain().AccountChoiceCount()
}

func (d initOnePasswordDesktopDiscovery) domain() credentials.OnePasswordDesktopDiscovery {
	return credentials.OnePasswordDesktopDiscovery{Accounts: d.Accounts, Err: d.Err}
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
