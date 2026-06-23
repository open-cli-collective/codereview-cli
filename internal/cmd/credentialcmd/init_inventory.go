package credentialcmd

import (
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

type initInventoryRowKind string

const (
	initInventoryRowKindActive  initInventoryRowKind = "active"
	initInventoryRowKindPending initInventoryRowKind = "pending"
	initInventoryRowKindCommand initInventoryRowKind = "command"
)

type initInventoryAction string

const (
	initInventoryActionNone        initInventoryAction = ""
	initInventoryActionEdit        initInventoryAction = "edit"
	initInventoryActionCommand     initInventoryAction = "command"
	initInventoryActionStageDelete initInventoryAction = "stage_delete"
	initInventoryActionRestore     initInventoryAction = "restore"
	initInventoryActionBack        initInventoryAction = "back"
)

type initInventoryMode string

const (
	initInventoryModeProgram       initInventoryMode = "program"
	initInventoryModeDeterministic initInventoryMode = "deterministic"
)

type initInventoryRow struct {
	ID            string
	Title         string
	Description   string
	FilterValue   string
	Kind          initInventoryRowKind
	PrimaryAction initInventoryAction
	Selectable    bool
	Deletable     bool
	Restorable    bool
}

type initInventoryResult struct {
	Action initInventoryAction
	Row    initInventoryRow
}

type initInventoryPrompt struct {
	Title       string
	Description string
	Rows        []initInventoryRow
	Width       int
	Height      int
	Mode        initInventoryMode
	Messages    []tea.Msg
}

type initInventoryRunner func(initInventoryPrompt, io.Reader, io.Writer) (initInventoryResult, error)

type initInventoryItem struct {
	row initInventoryRow
}

type initInventoryDelegate struct {
	delegate list.DefaultDelegate
}

func (i initInventoryItem) Title() string {
	return i.row.Title
}

func (i initInventoryItem) Description() string {
	return i.row.Description
}

func (i initInventoryItem) FilterValue() string {
	if strings.TrimSpace(i.row.FilterValue) != "" {
		return i.row.FilterValue
	}
	parts := []string{i.row.Title, i.row.Description, i.row.ID}
	return strings.TrimSpace(strings.Join(parts, " "))
}

func (d initInventoryDelegate) Height() int {
	return d.delegate.Height()
}

func (d initInventoryDelegate) Spacing() int {
	return d.delegate.Spacing()
}

func (d initInventoryDelegate) Update(msg tea.Msg, m *list.Model) tea.Cmd {
	return d.delegate.Update(msg, m)
}

func (d initInventoryDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	if inventoryItem, ok := item.(initInventoryItem); ok && strings.TrimSpace(inventoryItem.row.Description) == "" {
		delegate := d.delegate
		delegate.ShowDescription = false
		delegate.Render(w, m, index, item)
		return
	}
	d.delegate.Render(w, m, index, item)
}

func (d initInventoryDelegate) ShortHelp() []key.Binding {
	return d.delegate.ShortHelp()
}

func (d initInventoryDelegate) FullHelp() [][]key.Binding {
	return d.delegate.FullHelp()
}

type initInventoryKeyMap struct {
	Select  key.Binding
	Delete  key.Binding
	Restore key.Binding
	Back    key.Binding
}

func defaultInitInventoryKeyMap() initInventoryKeyMap {
	return initInventoryKeyMap{
		Select: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "select"),
		),
		Delete: key.NewBinding(
			key.WithKeys("d"),
			key.WithHelp("d", "delete"),
		),
		Restore: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "restore"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "back"),
		),
	}
}

type initInventoryModel struct {
	title  string
	desc   string
	list   list.Model
	rows   []initInventoryRow
	keys   initInventoryKeyMap
	result initInventoryResult
	quits  bool
}

func newInitInventoryModel(prompt initInventoryPrompt) initInventoryModel {
	rows := orderInitInventoryRows(prompt.Rows)
	items := make([]list.Item, 0, len(rows))
	for _, row := range rows {
		items = append(items, initInventoryItem{row: row})
	}
	defaultDelegate := list.NewDefaultDelegate()
	defaultDelegate.ShowDescription = initInventoryRowsHaveDescriptions(rows)
	if defaultDelegate.ShowDescription {
		defaultDelegate.SetHeight(2)
	} else {
		defaultDelegate.SetHeight(1)
	}
	defaultDelegate.SetSpacing(0)
	delegate := initInventoryDelegate{delegate: defaultDelegate}

	l := list.New(items, delegate, prompt.Width, prompt.Height)
	l.Title = prompt.Title
	l.DisableQuitKeybindings()
	l.SetShowStatusBar(false)
	l.SetShowPagination(false)
	l.SetStatusBarItemName("row", "rows")
	l.Filter = func(term string, _ []string) []list.Rank {
		return initInventoryFilterRanks(term, rows)
	}

	if prompt.Width == 0 {
		l.SetWidth(80)
	}
	if prompt.Height == 0 {
		l.SetHeight(14)
	}
	keys := defaultInitInventoryKeyMap()

	return initInventoryModel{
		title: prompt.Title,
		desc:  strings.TrimSpace(prompt.Description),
		list:  l,
		rows:  rows,
		keys:  keys,
	}
}

func orderInitInventoryRows(rows []initInventoryRow) []initInventoryRow {
	grouped := map[initInventoryRowKind][]initInventoryRow{
		initInventoryRowKindActive:  {},
		initInventoryRowKindPending: {},
		initInventoryRowKindCommand: {},
	}
	for _, row := range rows {
		grouped[row.Kind] = append(grouped[row.Kind], row)
	}
	ordered := make([]initInventoryRow, 0, len(rows))
	ordered = append(ordered, grouped[initInventoryRowKindActive]...)
	ordered = append(ordered, grouped[initInventoryRowKindCommand]...)
	ordered = append(ordered, grouped[initInventoryRowKindPending]...)
	return ordered
}

func initInventoryRowsHaveDescriptions(rows []initInventoryRow) bool {
	for _, row := range rows {
		if strings.TrimSpace(row.Description) != "" {
			return true
		}
	}
	return false
}

func initInventoryFilterRanks(term string, rows []initInventoryRow) []list.Rank {
	normalized := strings.ToLower(strings.TrimSpace(term))
	ranks := make([]list.Rank, 0, len(rows))
	for i, row := range rows {
		switch row.Kind {
		case initInventoryRowKindActive:
			if normalized == "" || strings.Contains(strings.ToLower(initInventoryItem{row: row}.FilterValue()), normalized) {
				ranks = append(ranks, list.Rank{Index: i})
			}
		case initInventoryRowKindPending, initInventoryRowKindCommand:
			ranks = append(ranks, list.Rank{Index: i})
		}
	}
	return ranks
}

func (m initInventoryModel) Init() tea.Cmd {
	return nil
}

func (m initInventoryModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if keyMsg, ok := msg.(tea.KeyMsg); ok {
		if m.list.FilterState() != list.Filtering {
			switch {
			case key.Matches(keyMsg, m.keys.Back):
				m.result = initInventoryResult{Action: initInventoryActionBack}
				m.quits = true
				return m, tea.Quit
			case key.Matches(keyMsg, m.keys.Select):
				if row, ok := m.selectedRow(); ok && row.Selectable {
					m.result = initInventoryResult{Action: row.primaryAction(), Row: row}
					m.quits = true
					return m, tea.Quit
				}
			case key.Matches(keyMsg, m.keys.Delete):
				if row, ok := m.selectedRow(); ok && row.Deletable {
					m.result = initInventoryResult{Action: initInventoryActionStageDelete, Row: row}
					m.quits = true
					return m, tea.Quit
				}
			case key.Matches(keyMsg, m.keys.Restore):
				if row, ok := m.selectedRow(); ok && row.Restorable {
					m.result = initInventoryResult{Action: initInventoryActionRestore, Row: row}
					m.quits = true
					return m, tea.Quit
				}
			}
		}
	}
	updatedList, cmd := m.list.Update(msg)
	m.list = updatedList
	return m, cmd
}

func (m initInventoryModel) View() string {
	if m.quits {
		// Let Bubble Tea's final render erase the inventory frame before the next
		// prompt starts, rather than appending the next form below stale content.
		return ""
	}
	var lines []string
	lines = append(lines, initLinearTheme.title.Render(m.title))
	if m.desc != "" {
		var descLines []string
		initLinearAppendWrappedWithPrefix(&descLines, "", m.desc, initInventoryViewWidth(m))
		for _, line := range descLines {
			lines = append(lines, initLinearTheme.help.Render(line))
		}
	}
	if filter := strings.TrimSpace(m.list.FilterValue()); filter != "" {
		lines = append(lines, "", initLinearTheme.title.Render("Filter"), initLinearTheme.help.Render(filter))
	}
	lines = append(lines, m.renderRows()...)
	lines = append(lines, "", initLinearTheme.help.Render(m.helpLine()))
	return strings.Join(lines, "\n")
}

func (m initInventoryModel) renderRows() []string {
	items := m.list.VisibleItems()
	if len(items) == 0 {
		return []string{"", initLinearTheme.help.Render("No matching entries")}
	}
	selectedIndex := m.list.Index()
	var lines []string
	currentSection := initInventoryRowKind("")
	for index, item := range items {
		inventoryItem, ok := item.(initInventoryItem)
		if !ok {
			continue
		}
		row := inventoryItem.row
		if row.Kind != currentSection {
			currentSection = row.Kind
			lines = append(lines, "")
			lines = append(lines, initLinearTheme.title.Render(initInventorySectionTitle(row.Kind)))
		}
		lines = append(lines, m.renderRow(row, index == selectedIndex)...)
	}
	return lines
}

func initInventorySectionTitle(kind initInventoryRowKind) string {
	switch kind {
	case initInventoryRowKindActive:
		return "Configured"
	case initInventoryRowKindPending:
		return "Staged changes"
	case initInventoryRowKindCommand:
		return "Actions"
	default:
		return "Actions"
	}
}

func (m initInventoryModel) renderRow(row initInventoryRow, selected bool) []string {
	prefix := "  [ ] "
	if selected {
		prefix = "> [x] "
	}
	var titleLines []string
	initLinearAppendWrappedWithPrefix(&titleLines, prefix, row.Title, initInventoryViewWidth(m))
	title := strings.Join(titleLines, "\n")
	switch {
	case selected && initInventoryRowActionable(row):
		title = initLinearStyleSelectedLine(title)
	case !initInventoryRowActionable(row):
		title = initLinearTheme.help.Render(title)
	}
	lines := []string{title}
	if description := strings.TrimSpace(row.Description); description != "" {
		var descriptionLines []string
		initLinearAppendWrappedWithPrefix(&descriptionLines, "      ", description, initInventoryViewWidth(m))
		for _, line := range descriptionLines {
			lines = append(lines, initLinearTheme.help.Render(line))
		}
	}
	return lines
}

func initInventoryViewWidth(m initInventoryModel) int {
	if width := m.list.Width(); width > 0 {
		return width
	}
	return 80
}

func initInventoryRowActionable(row initInventoryRow) bool {
	return row.Selectable || row.Deletable || row.Restorable
}

func (m initInventoryModel) helpLine() string {
	parts := []string{"up/k previous", "down/j next", "enter select"}
	if row, ok := m.selectedRow(); ok {
		if row.Deletable {
			parts = append(parts, "d delete")
		}
		if row.Restorable {
			parts = append(parts, "r restore")
		}
	}
	parts = append(parts, "esc back")
	return strings.Join(parts, " - ")
}

func (m initInventoryModel) selectedRow() (initInventoryRow, bool) {
	selected := m.list.SelectedItem()
	if selected == nil {
		return initInventoryRow{}, false
	}
	item, ok := selected.(initInventoryItem)
	if !ok {
		return initInventoryRow{}, false
	}
	return item.row, true
}

func (r initInventoryRow) primaryAction() initInventoryAction {
	if r.PrimaryAction != initInventoryActionNone {
		return r.PrimaryAction
	}
	if r.Kind == initInventoryRowKindCommand {
		return initInventoryActionCommand
	}
	return initInventoryActionEdit
}

func (m initInventoryModel) Result() initInventoryResult {
	return m.result
}

func (m initInventoryModel) QuitRequested() bool {
	return m.quits
}

func runInitInventory(prompt initInventoryPrompt, stdin io.Reader, stdout io.Writer) (initInventoryResult, error) {
	if prompt.Mode == initInventoryModeDeterministic {
		return runInitInventoryDeterministic(prompt), nil
	}
	model := newInitInventoryModel(prompt)
	program := tea.NewProgram(model, tea.WithInput(stdin), tea.WithOutput(stdout))
	finalModel, err := program.Run()
	if err != nil {
		return initInventoryResult{}, err
	}
	resultModel, ok := finalModel.(initInventoryModel)
	if !ok {
		return initInventoryResult{}, nil
	}
	return resultModel.Result(), nil
}

func runInitInventoryDeterministic(prompt initInventoryPrompt) initInventoryResult {
	model := newInitInventoryModel(prompt)
	for _, msg := range prompt.Messages {
		nextModel, _ := model.Update(msg)
		model = nextModel.(initInventoryModel)
		if model.QuitRequested() {
			break
		}
	}
	return model.Result()
}
