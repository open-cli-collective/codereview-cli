package credentialcmd

import (
	"fmt"
	"io"
	"os"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/term"
)

type initMenuItem struct {
	Action      initMenuAction
	Title       string
	Description string
	Disabled    string
}

type initMenuModel struct {
	items    []initMenuItem
	selected int
	desc     string
	err      string
	result   initMenuAction
	quitting bool
}

func runInitMenu(prompt initMenuPrompt, stdin io.Reader, stderr io.Writer) (initMenuAction, error) {
	model := newInitMenuModel(prompt)
	program := tea.NewProgram(model, tea.WithInput(stdin), tea.WithOutput(stderr))
	finalModel, err := program.Run()
	if err != nil {
		return "", err
	}
	resultModel, ok := finalModel.(initMenuModel)
	if !ok || resultModel.result == "" {
		return "", errInitNavigateBack
	}
	return resultModel.result, nil
}

func initMenuUseAccessibleFallback(stdin io.Reader, stderr io.Writer) bool {
	if strings.EqualFold(os.Getenv("TERM"), "dumb") {
		return true
	}
	stdinFile, ok := stdin.(*os.File)
	if !ok {
		return true
	}
	stderrFile, ok := stderr.(*os.File)
	if !ok {
		return true
	}
	return !term.IsTerminal(int(stdinFile.Fd())) || !term.IsTerminal(int(stderrFile.Fd()))
}

func newInitMenuModel(prompt initMenuPrompt) initMenuModel {
	items := initMenuItems(prompt)
	selected := initMenuSelectedIndex(items, initMenuInitialAction(prompt))
	return initMenuModel{
		items:    items,
		selected: selected,
		desc:     initMenuDescription(prompt),
	}
}

func initMenuSelectedIndex(items []initMenuItem, action initMenuAction) int {
	for index, item := range items {
		if item.Action == action {
			return index
		}
	}
	return 0
}

func initMenuItems(prompt initMenuPrompt) []initMenuItem {
	items := []initMenuItem{
		{
			Action:      initMenuActionSecretsManagement,
			Title:       "Configure secrets storage",
			Description: "Credential stores for tokens and keys",
		},
		{
			Action:      initMenuActionLLMRuntimes,
			Title:       "Configure LLM runtimes",
			Description: initMenuConfiguredDescription(prompt.LLMRuntimeCount, "runtime", "runtimes", "Model providers and runtime credentials"),
		},
		{
			Action:      initMenuActionReviewerEntities,
			Title:       "Configure reviewer entities",
			Description: initMenuConfiguredDescription(prompt.ReviewerEntityCount, "reviewer entity", "reviewer entities", "Reviewer identities and posting credentials"),
		},
		{
			Action:      initMenuActionReviewProfiles,
			Title:       "Configure review profiles",
			Description: initMenuConfiguredDescription(prompt.ReviewProfileCount, "profile", "profiles", "Repository routing and review composition"),
		},
		{
			Action:      initMenuActionGlobalSettings,
			Title:       "Configure global settings",
			Description: "Data retention and global defaults",
		},
		{
			Action:      initMenuActionSave,
			Title:       "Commit staged changes and exit",
			Description: "Write staged config and credential changes",
		},
		{
			Action:      initMenuActionExit,
			Title:       "Discard staged changes and exit",
			Description: "Leave without writing staged changes",
		},
	}
	for index := range items {
		items[index].Disabled = initMenuDisabledReason(prompt, items[index].Action)
		if items[index].Disabled != "" {
			items[index].Description = "Prerequisite: " + items[index].Disabled
		}
	}
	return items
}

func initMenuConfiguredDescription(count int, singular, plural, zero string) string {
	if count == 0 {
		return zero
	}
	return initMenuCountDescription(count, singular, plural)
}

func initMenuCountDescription(count int, singular, plural string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s configured", singular)
	}
	return fmt.Sprintf("%d %s configured", count, plural)
}

func initMenuDisabledReason(prompt initMenuPrompt, action initMenuAction) string {
	switch action {
	case initMenuActionReviewerEntities:
		if !prompt.CanConfigureReviewer {
			return "configure a review profile before editing reviewer entities"
		}
	case initMenuActionSave:
		if !prompt.CanSave {
			return "stage changes before committing"
		}
	case initMenuActionSecretsManagement, initMenuActionReviewProfiles, initMenuActionGlobalSettings, initMenuActionExit:
	}
	return ""
}

func (m initMenuModel) Init() tea.Cmd {
	return nil
}

func (m initMenuModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	keyMsg, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	switch keyMsg.String() {
	case "q":
		m.result = initMenuActionExit
		m.quitting = true
		return m, tea.Quit
	case "up", "k", "shift+tab":
		m.move(-1)
	case "down", "j", "tab":
		m.move(1)
	case "enter":
		if len(m.items) == 0 {
			return m, nil
		}
		item := m.items[m.selected]
		if item.Disabled != "" {
			m.err = item.Disabled
			return m, nil
		}
		m.result = item.Action
		m.quitting = true
		return m, tea.Quit
	}
	return m, nil
}

func (m *initMenuModel) move(delta int) {
	if len(m.items) == 0 {
		return
	}
	m.err = ""
	for step := 0; step < len(m.items); step++ {
		next := (m.selected + delta + len(m.items)) % len(m.items)
		m.selected = next
		if m.items[next].Disabled == "" {
			return
		}
	}
}

func (m initMenuModel) View() string {
	if m.quitting {
		return ""
	}
	var lines []string
	lines = append(lines, initLinearTheme.title.Render("cr init"))
	if strings.TrimSpace(m.desc) != "" {
		lines = append(lines, initLinearTheme.help.Render(m.desc))
	}
	lines = append(lines, "", initLinearTheme.title.Render("Actions"))
	for index, item := range m.items {
		lines = append(lines, m.renderItem(index, item)...)
	}
	if strings.TrimSpace(m.err) != "" {
		lines = append(lines, "", initLinearTheme.error.Render("! "+m.err))
	}
	lines = append(lines, "", initLinearTheme.help.Render("up/k previous - down/j next - enter select - q discard"))
	return strings.Join(lines, "\n")
}

func (m initMenuModel) renderItem(index int, item initMenuItem) []string {
	selected := index == m.selected
	prefix := "  [ ] "
	if selected {
		prefix = "> [x] "
	}
	title := prefix + item.Title
	if item.Disabled != "" {
		title = initLinearTheme.help.Render(title)
	} else if selected {
		title = initLinearStyleSelectedLine(title)
	}
	return []string{
		title,
		"      " + initLinearTheme.help.Render(item.Description),
	}
}
