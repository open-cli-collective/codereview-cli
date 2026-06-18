package credentialcmd

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
)

type initLinearFieldKind string
type initLinearFieldID string

const (
	initLinearFieldSection  initLinearFieldKind = "section"
	initLinearFieldInput    initLinearFieldKind = "input"
	initLinearFieldSelect   initLinearFieldKind = "select"
	initLinearFieldTextarea initLinearFieldKind = "textarea"
)

var initLinearTheme = struct {
	title       lipgloss.Style
	selected    lipgloss.Style
	caret       lipgloss.Style
	activeTitle lipgloss.Style
	error       lipgloss.Style
	help        lipgloss.Style
}{
	title:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63")),
	selected:    lipgloss.NewStyle().Foreground(lipgloss.Color("42")),
	caret:       lipgloss.NewStyle().Foreground(lipgloss.Color("201")),
	activeTitle: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63")),
	error:       lipgloss.NewStyle().Foreground(lipgloss.Color("9")),
	help:        lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
}

type initLinearEnterHandler func(*initLinearEditorModel) (bool, tea.Cmd)
type initLinearChangeHandler func(*initLinearEditorModel, int)

const (
	initLinearResultActionDelete  = "delete"
	initLinearResultActionRestore = "restore"
)

type initLinearEditor struct {
	Document      initLinearDocument
	OnEnter       initLinearEnterHandler
	OnFieldChange initLinearChangeHandler
	Help          string
	TextareaHelp  string
}

type initLinearEditorModel struct {
	viewport      viewport.Model
	document      initLinearDocument
	layout        initLinearLayout
	focused       int
	quitting      bool
	resultAction  string
	onEnter       initLinearEnterHandler
	onFieldChange initLinearChangeHandler
	help          string
	textareaHelp  string
}

type initLinearDocument []initLinearField

type initLinearField struct {
	ID          initLinearFieldID
	Kind        initLinearFieldKind
	Title       string
	Description string
	Value       string
	Cursor      int
	Options     []initLinearOption
	Focusable   bool
	Editable    bool
	Hidden      bool
	Error       string
	Validate    func(string) error
}

type initLinearFieldOptions struct {
	Hidden bool
}

type initLinearOption struct {
	Label      string
	Value      string
	Selected   bool
	Deletable  bool
	Restorable bool
}

type initLinearLayout struct {
	Content       string
	Bounds        []initLinearFieldBounds
	Lines         int
	SelectedLines map[int]bool
}

type initLinearFieldBounds struct {
	Start int
	End   int
}

func newInitLinearEditorModel(editor initLinearEditor, width, height int) initLinearEditorModel {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 28
	}
	help := editor.Help
	if help == "" {
		help = "tab/enter next - shift+tab previous - up/down change select - esc back"
	}
	textareaHelp := editor.TextareaHelp
	if textareaHelp == "" {
		textareaHelp = "tab/enter next - shift+tab previous - ctrl+j newline - esc back"
	}
	model := initLinearEditorModel{
		viewport:      viewport.New(width, max(height-2, 1)),
		document:      editor.Document,
		focused:       editor.Document.firstFocusableField(),
		onEnter:       editor.OnEnter,
		onFieldChange: editor.OnFieldChange,
		help:          help,
		textareaHelp:  textareaHelp,
	}
	model.validateAll()
	model.relayout()
	model.ensureFocusedVisible()
	return model
}

func (m initLinearEditorModel) Init() tea.Cmd {
	return nil
}

func (m initLinearEditorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = max(msg.Width, 1)
		m.viewport.Height = max(msg.Height-2, 1)
		m.relayout()
		m.ensureFocusedVisible()
		return m, nil
	case tea.KeyMsg:
		if m.handleFocusedInputKey(msg) {
			m.relayout()
			m.ensureFocusedVisible()
			return m, nil
		}
		if handled, cmd := m.handleFocusedSelectKey(msg); handled {
			m.relayout()
			m.ensureFocusedVisible()
			if cmd != nil {
				m.quitting = true
			}
			return m, cmd
		}
		if m.onEnter != nil && msg.String() == "enter" {
			handled, cmd := m.onEnter(&m)
			if handled {
				if cmd != nil {
					m.quitting = true
				}
				return m, cmd
			}
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "shift+tab":
			m.focused = m.document.previousFocusableField(m.focused)
			m.relayout()
			m.ensureFocusedVisible()
			return m, nil
		case "tab", "enter":
			m.focused = m.document.nextFocusableField(m.focused)
			m.relayout()
			m.ensureFocusedVisible()
			return m, nil
		case "pgup", "b":
			m.viewport.HalfPageUp()
			return m, nil
		case "pgdown", "f", " ":
			m.viewport.HalfPageDown()
			return m, nil
		case "home", "g":
			m.focused = m.document.firstFocusableField()
			m.relayout()
			m.ensureFocusedVisible()
			return m, nil
		case "end", "G":
			m.focused = m.document.lastFocusableField()
			m.relayout()
			m.ensureFocusedVisible()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m initLinearEditorModel) View() string {
	if m.quitting {
		return ""
	}
	help := m.currentHelp()
	return m.styleVisibleViewport() + "\n\n" + initLinearTheme.help.Render(help)
}

func (m initLinearEditorModel) currentHelp() string {
	help := m.help
	if m.focused >= 0 && m.focused < len(m.document) && m.document[m.focused].Kind == initLinearFieldTextarea {
		help = m.textareaHelp
	}
	if m.focused >= 0 && m.focused < len(m.document) && m.document[m.focused].Kind == initLinearFieldSelect {
		if selected := initLinearSelectedOption(&m.document[m.focused]); selected != nil {
			if selected.Deletable && !strings.Contains(help, "d delete") {
				help += " - d delete"
			}
			if selected.Restorable && !strings.Contains(help, "r restore") {
				help += " - r restore"
			}
		}
	}
	return help
}

func (d *initLinearDocument) addSection(title, description string) {
	d.addSectionField("", title, description)
}

func (d *initLinearDocument) addSectionField(id initLinearFieldID, title, description string, options ...initLinearFieldOptions) {
	*d = append(*d, initLinearField{
		ID:          id,
		Kind:        initLinearFieldSection,
		Title:       title,
		Description: description,
		Hidden:      mergedInitLinearFieldOptions(options).Hidden,
	})
}

func (d *initLinearDocument) addInput(title, description, value string) {
	d.addInputField(initLinearFieldInput, "", title, description, value, false, nil, initLinearFieldOptions{})
}

func (d *initLinearDocument) addEditableInput(id initLinearFieldID, title, description, value string, validate func(string) error, options ...initLinearFieldOptions) {
	d.addInputField(initLinearFieldInput, id, title, description, value, true, validate, mergedInitLinearFieldOptions(options))
}

func (d *initLinearDocument) addEditableTextarea(id initLinearFieldID, title, description, value string) {
	d.addInputField(initLinearFieldTextarea, id, title, description, value, true, nil, initLinearFieldOptions{})
}

func (d *initLinearDocument) addInputField(kind initLinearFieldKind, id initLinearFieldID, title, description, value string, editable bool, validate func(string) error, options initLinearFieldOptions) {
	*d = append(*d, initLinearField{
		Kind:        kind,
		ID:          id,
		Title:       title,
		Description: description,
		Value:       value,
		Cursor:      len([]rune(value)),
		Focusable:   true,
		Editable:    editable,
		Hidden:      options.Hidden,
		Validate:    validate,
	})
}

func mergedInitLinearFieldOptions(options []initLinearFieldOptions) initLinearFieldOptions {
	var merged initLinearFieldOptions
	for _, option := range options {
		if option.Hidden {
			merged.Hidden = true
		}
	}
	return merged
}

func initLinearAddSelect[T comparable](document *initLinearDocument, title, description string, options []huh.Option[T], selected T) {
	initLinearAddSelectField(document, "", title, description, options, selected, false, initLinearFieldOptions{})
}

func (d *initLinearDocument) addEditableSelect(id initLinearFieldID, title, description string, options []huh.Option[string], selected string, fieldOptions ...initLinearFieldOptions) {
	initLinearAddSelectField(d, id, title, description, options, selected, true, mergedInitLinearFieldOptions(fieldOptions))
}

func initLinearAddSelectField[T comparable](document *initLinearDocument, id initLinearFieldID, title, description string, options []huh.Option[T], selected T, editable bool, fieldOptions initLinearFieldOptions) {
	field := initLinearField{
		Kind:        initLinearFieldSelect,
		ID:          id,
		Title:       title,
		Description: description,
		Focusable:   true,
		Editable:    editable,
		Hidden:      fieldOptions.Hidden,
		Options:     make([]initLinearOption, 0, len(options)),
	}
	for _, option := range options {
		field.Options = append(field.Options, initLinearOption{
			Label:    option.Key,
			Value:    fmt.Sprint(option.Value),
			Selected: option.Value == selected,
		})
	}
	*document = append(*document, field)
}

func (d initLinearDocument) firstFocusableField() int {
	for index, field := range d {
		if field.Focusable && !field.Hidden {
			return index
		}
	}
	return 0
}

func (d initLinearDocument) lastFocusableField() int {
	for index := len(d) - 1; index >= 0; index-- {
		if d[index].Focusable && !d[index].Hidden {
			return index
		}
	}
	return d.firstFocusableField()
}

func (d initLinearDocument) nextFocusableField(current int) int {
	for index := current + 1; index < len(d); index++ {
		if d[index].Focusable && !d[index].Hidden {
			return index
		}
	}
	return current
}

func (d initLinearDocument) previousFocusableField(current int) int {
	for index := current - 1; index >= 0; index-- {
		if d[index].Focusable && !d[index].Hidden {
			return index
		}
	}
	return current
}

func (d initLinearDocument) fieldIndexByTitle(title string) int {
	for index, field := range d {
		if field.Title == title {
			return index
		}
	}
	return -1
}

func (d initLinearDocument) fieldIndexByID(id initLinearFieldID) int {
	for index, field := range d {
		if field.ID == id {
			return index
		}
	}
	return -1
}

func (d initLinearDocument) fieldValue(id initLinearFieldID) string {
	index := d.fieldIndexByID(id)
	if index < 0 {
		return ""
	}
	return d[index].Value
}

func (d initLinearDocument) fieldHidden(id initLinearFieldID) bool {
	index := d.fieldIndexByID(id)
	if index < 0 {
		return true
	}
	return d[index].Hidden
}

func (d initLinearDocument) selectedValue(id initLinearFieldID) string {
	index := d.fieldIndexByID(id)
	if index < 0 {
		return ""
	}
	for _, option := range d[index].Options {
		if option.Selected {
			return option.Value
		}
	}
	return ""
}

func (m *initLinearEditorModel) handleFocusedInputKey(msg tea.KeyMsg) bool {
	if m.focused < 0 || m.focused >= len(m.document) {
		return false
	}
	field := &m.document[m.focused]
	if (field.Kind != initLinearFieldInput && field.Kind != initLinearFieldTextarea) || !field.Editable {
		return false
	}
	if field.Kind == initLinearFieldTextarea && (msg.String() == "ctrl+j" || msg.String() == "alt+enter") {
		field.Value = initLinearInsertRunes(field.Value, field.Cursor, []rune{'\n'})
		field.Cursor++
		m.afterFieldChange(m.focused)
		return true
	}
	key := tea.Key(msg)
	//nolint:exhaustive // The text input consumes only editing keys; all other keys fall through to form navigation.
	switch key.Type {
	case tea.KeyRunes:
		if msg.Alt {
			return false
		}
		field.Value = initLinearInsertRunes(field.Value, field.Cursor, key.Runes)
		field.Cursor += len(key.Runes)
	case tea.KeyBackspace, tea.KeyCtrlH:
		field.Value, field.Cursor = initLinearDeleteBeforeCursor(field.Value, field.Cursor)
	case tea.KeyDelete, tea.KeyCtrlD:
		field.Value = initLinearDeleteAtCursor(field.Value, field.Cursor)
	case tea.KeyLeft, tea.KeyCtrlB:
		field.Cursor = max(field.Cursor-1, 0)
	case tea.KeyRight, tea.KeyCtrlF:
		field.Cursor = min(field.Cursor+1, len([]rune(field.Value)))
	case tea.KeyCtrlA:
		field.Cursor = 0
	case tea.KeyCtrlE:
		field.Cursor = len([]rune(field.Value))
	case tea.KeyCtrlU:
		field.Value = ""
		field.Cursor = 0
	case tea.KeyCtrlW:
		field.Value, field.Cursor = initLinearDeleteWordBeforeCursor(field.Value, field.Cursor)
	case tea.KeyCtrlK:
		field.Value = initLinearDeleteAfterCursor(field.Value, field.Cursor)
	default:
		return false
	}
	m.afterFieldChange(m.focused)
	return true
}

func (m *initLinearEditorModel) handleFocusedSelectKey(msg tea.KeyMsg) (bool, tea.Cmd) {
	if m.focused < 0 || m.focused >= len(m.document) {
		return false, nil
	}
	field := &m.document[m.focused]
	if field.Kind != initLinearFieldSelect || !field.Editable || len(field.Options) == 0 {
		return false, nil
	}
	switch msg.String() {
	case "up", "k":
		initLinearMoveSelection(field, -1)
	case "down", "j", " ":
		initLinearMoveSelection(field, 1)
	case "d":
		if selected := initLinearSelectedOption(field); selected != nil && selected.Deletable {
			m.resultAction = initLinearResultActionDelete
			return true, tea.Quit
		}
		return false, nil
	case "r":
		if selected := initLinearSelectedOption(field); selected != nil && selected.Restorable {
			m.resultAction = initLinearResultActionRestore
			return true, tea.Quit
		}
		return false, nil
	default:
		return false, nil
	}
	m.afterFieldChange(m.focused)
	return true, nil
}

func initLinearSelectedOption(field *initLinearField) *initLinearOption {
	if field == nil {
		return nil
	}
	for index := range field.Options {
		if field.Options[index].Selected {
			return &field.Options[index]
		}
	}
	return nil
}

func initLinearMoveSelection(field *initLinearField, offset int) {
	if len(field.Options) == 0 {
		return
	}
	selectedIndex := 0
	for index, option := range field.Options {
		if option.Selected {
			selectedIndex = index
			break
		}
	}
	next := (selectedIndex + offset) % len(field.Options)
	if next < 0 {
		next += len(field.Options)
	}
	for index := range field.Options {
		field.Options[index].Selected = index == next
	}
}

func (m *initLinearEditorModel) afterFieldChange(index int) {
	m.validateField(index)
	if m.onFieldChange != nil {
		m.onFieldChange(m, index)
	}
}

func (m *initLinearEditorModel) validateAll() {
	for index := range m.document {
		m.validateField(index)
	}
}

func (m *initLinearEditorModel) validateField(index int) {
	if index < 0 || index >= len(m.document) {
		return
	}
	field := &m.document[index]
	field.Error = ""
	if field.Validate == nil {
		return
	}
	if err := field.Validate(field.Value); err != nil {
		field.Error = err.Error()
	}
}

func (m *initLinearEditorModel) setFieldValue(id initLinearFieldID, value string) {
	index := m.document.fieldIndexByID(id)
	if index < 0 {
		return
	}
	m.document[index].Value = value
	m.document[index].Cursor = len([]rune(value))
}

func (m *initLinearEditorModel) setFieldDescription(id initLinearFieldID, description string) {
	index := m.document.fieldIndexByID(id)
	if index < 0 {
		return
	}
	m.document[index].Description = description
}

func (m *initLinearEditorModel) selectFieldValue(id initLinearFieldID, value string) {
	index := m.document.fieldIndexByID(id)
	if index < 0 {
		return
	}
	for optionIndex := range m.document[index].Options {
		m.document[index].Options[optionIndex].Selected = m.document[index].Options[optionIndex].Value == value
	}
}

func (m *initLinearEditorModel) setFieldHidden(id initLinearFieldID, hidden bool) {
	index := m.document.fieldIndexByID(id)
	if index < 0 {
		return
	}
	m.document[index].Hidden = hidden
}

func (m *initLinearEditorModel) relayout() {
	m.layout = initLinearLayoutDocument(m.document, m.viewport.Width, m.focused)
	m.viewport.SetContent(m.layout.Content)
}

func (m *initLinearEditorModel) ensureFocusedVisible() {
	if m.focused < 0 || m.focused >= len(m.layout.Bounds) {
		return
	}
	bounds := m.layout.Bounds[m.focused]
	height := max(m.viewport.Height, 1)
	top := m.viewport.YOffset
	bottom := top + height
	switch {
	case bounds.Start < top:
		m.viewport.SetYOffset(bounds.Start)
	case bounds.Start >= bottom:
		m.viewport.SetYOffset(bounds.Start)
	case bounds.End > bottom:
		if bounds.End-bounds.Start >= height {
			m.viewport.SetYOffset(bounds.Start)
			return
		}
		m.viewport.SetYOffset(max(bounds.End-height, 0))
	}
}

func initLinearLayoutDocument(document initLinearDocument, width int, focused int) initLinearLayout {
	width = max(width, 20)
	lines := []string{}
	bounds := make([]initLinearFieldBounds, len(document))
	selectedLines := map[int]bool{}
	for index, field := range document {
		if field.Hidden {
			bounds[index] = initLinearFieldBounds{Start: len(lines), End: len(lines)}
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		start := len(lines)
		initLinearAppendFieldLines(&lines, selectedLines, field, index == focused, width)
		bounds[index] = initLinearFieldBounds{Start: start, End: len(lines)}
	}
	return initLinearLayout{
		Content:       strings.TrimRight(strings.Join(lines, "\n"), "\n"),
		Bounds:        bounds,
		Lines:         len(lines),
		SelectedLines: selectedLines,
	}
}

func initLinearAppendFieldLines(lines *[]string, selectedLines map[int]bool, field initLinearField, focused bool, width int) {
	titlePrefix := ""
	initLinearAppendWrappedWithPrefix(lines, titlePrefix, field.Title, width)
	initLinearAppendWrappedWithPrefix(lines, titlePrefix, field.Description, width)
	if strings.TrimSpace(field.Error) != "" {
		initLinearAppendWrappedWithPrefix(lines, titlePrefix+"! ", field.Error, width)
	}
	switch field.Kind {
	case initLinearFieldSection:
	case initLinearFieldInput, initLinearFieldTextarea:
		value := field.Value
		if focused && field.Editable {
			value = initLinearValueWithCursor(value, field.Cursor)
		}
		valueLines := strings.Split(value, "\n")
		if len(valueLines) == 0 {
			valueLines = []string{""}
		}
		for index, line := range valueLines {
			prefix := "  "
			if focused && index == 0 {
				prefix = "> "
			}
			initLinearAppendWrappedWithPrefix(lines, prefix, line, width)
		}
	case initLinearFieldSelect:
		for _, option := range field.Options {
			prefix := "  "
			if focused && option.Selected {
				prefix = "> "
			}
			initLinearAppendWrappedWithPrefixMarked(lines, selectedLines, prefix, option.Label, width, option.Selected)
		}
	}
}

func initLinearAppendWrappedWithPrefix(lines *[]string, prefix string, text string, width int) {
	initLinearAppendWrappedWithPrefixMarked(lines, nil, prefix, text, width, false)
}

func initLinearAppendWrappedWithPrefixMarked(lines *[]string, selectedLines map[int]bool, prefix string, text string, width int, selected bool) {
	start := len(*lines)
	available := max(width-len([]rune(prefix)), 1)
	remaining := []rune(strings.TrimSpace(text))
	if len(remaining) == 0 {
		*lines = append(*lines, prefix)
		markInitLinearSelectedLines(selectedLines, selected, start, len(*lines))
		return
	}
	for len(remaining) > available {
		cut := available
		for cut > 0 && remaining[cut] != ' ' {
			cut--
		}
		if cut <= 0 {
			cut = available
		}
		*lines = append(*lines, prefix+strings.TrimSpace(string(remaining[:cut])))
		remaining = []rune(strings.TrimSpace(string(remaining[cut:])))
		prefix = strings.Repeat(" ", len([]rune(prefix)))
		available = max(width-len([]rune(prefix)), 1)
	}
	*lines = append(*lines, prefix+string(remaining))
	markInitLinearSelectedLines(selectedLines, selected, start, len(*lines))
}

func markInitLinearSelectedLines(selectedLines map[int]bool, selected bool, start int, end int) {
	if !selected || selectedLines == nil {
		return
	}
	for index := start; index < end; index++ {
		selectedLines[index] = true
	}
}

func (m initLinearEditorModel) styleVisibleViewport() string {
	lines := strings.Split(m.viewport.View(), "\n")
	activeStart := -1
	activeEnd := -1
	if m.focused >= 0 && m.focused < len(m.layout.Bounds) {
		bounds := m.layout.Bounds[m.focused]
		activeStart = bounds.Start - m.viewport.YOffset
		activeEnd = bounds.End - m.viewport.YOffset
	}
	for index, line := range lines {
		selected := m.layout.SelectedLines[m.viewport.YOffset+index]
		active := index >= activeStart && index < activeEnd
		lines[index] = m.styleViewportLine(line, active, selected)
	}
	return strings.Join(lines, "\n")
}

func (m initLinearEditorModel) styleViewportLine(line string, active bool, selected bool) string {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		return line
	case strings.HasPrefix(trimmed, "! "):
		return initLinearTheme.error.Render(line)
	case strings.HasPrefix(trimmed, "> "):
		return initLinearStyleSelectedLine(line)
	case selected:
		return initLinearTheme.selected.Render(line)
	case active && m.looksLikeHeading(trimmed):
		return initLinearTheme.activeTitle.Render(line)
	case m.looksLikeHeading(trimmed):
		return initLinearTheme.title.Render(line)
	default:
		return line
	}
}

func initLinearStyleSelectedLine(line string) string {
	caretIndex := strings.Index(line, ">")
	if caretIndex < 0 {
		return initLinearTheme.selected.Render(line)
	}
	return line[:caretIndex] +
		initLinearTheme.caret.Render(">") +
		initLinearTheme.selected.Render(line[caretIndex+1:])
}

func (m initLinearEditorModel) looksLikeHeading(line string) bool {
	for _, field := range m.document {
		if !field.Hidden && field.Title == line {
			return true
		}
	}
	return false
}

func initLinearInsertRunes(value string, cursor int, runes []rune) string {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	next := make([]rune, 0, len(existing)+len(runes))
	next = append(next, existing[:cursor]...)
	next = append(next, runes...)
	next = append(next, existing[cursor:]...)
	return string(next)
}

func initLinearDeleteBeforeCursor(value string, cursor int) (string, int) {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	if cursor == 0 {
		return value, cursor
	}
	next := make([]rune, 0, len(existing)-1)
	next = append(next, existing[:cursor-1]...)
	next = append(next, existing[cursor:]...)
	return string(next), cursor - 1
}

func initLinearDeleteWordBeforeCursor(value string, cursor int) (string, int) {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	index := cursor
	for index > 0 && existing[index-1] == ' ' {
		index--
	}
	for index > 0 && existing[index-1] != ' ' {
		index--
	}
	next := make([]rune, 0, len(existing)-(cursor-index))
	next = append(next, existing[:index]...)
	next = append(next, existing[cursor:]...)
	return string(next), index
}

func initLinearDeleteAtCursor(value string, cursor int) string {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	if cursor >= len(existing) {
		return value
	}
	next := make([]rune, 0, len(existing)-1)
	next = append(next, existing[:cursor]...)
	next = append(next, existing[cursor+1:]...)
	return string(next)
}

func initLinearDeleteAfterCursor(value string, cursor int) string {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	return string(existing[:cursor])
}

func initLinearValueWithCursor(value string, cursor int) string {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	next := make([]rune, 0, len(existing)+1)
	next = append(next, existing[:cursor]...)
	next = append(next, '|')
	next = append(next, existing[cursor:]...)
	return string(next)
}
