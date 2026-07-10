package credentialcmd

import (
	"fmt"
	"io"
	"strings"
	"unicode"

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
type initLinearDeleteHandler func(*initLinearEditorModel, int) (bool, tea.Cmd)
type initEditorRunner func(initLinearEditor, io.Reader, io.Writer) (initLinearEditorModel, error)

const (
	initLinearResultActionDelete  = "delete"
	initLinearResultActionRestore = "restore"
)

type initLinearEditor struct {
	Document      initLinearDocument
	OnEnter       initLinearEnterHandler
	OnFieldChange initLinearChangeHandler
	OnDelete      initLinearDeleteHandler
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
	onDelete      initLinearDeleteHandler
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
	Secret      bool
	AutoManaged bool
	Error       string
	Validate    func(string) error
}

type initLinearFieldOptions struct {
	Hidden bool
	Secret bool
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

func runInitEditor(editor initLinearEditor, stdin io.Reader, stderr io.Writer, runner initEditorRunner, name string) (initLinearEditorModel, error) {
	if runner != nil {
		return runner(editor, stdin, stderr)
	}
	program := tea.NewProgram(newInitLinearEditorModel(editor, 100, 28), tea.WithInput(stdin), tea.WithOutput(stderr))
	finalModel, err := program.Run()
	if err != nil {
		return initLinearEditorModel{}, err
	}
	model, ok := finalModel.(initLinearEditorModel)
	if !ok {
		return initLinearEditorModel{}, fmt.Errorf("%s editor returned %T", name, finalModel)
	}
	return model, nil
}

func initLinearActionEnterHandler(fieldID initLinearFieldID, handle func(*initLinearEditorModel, string) (string, error)) initLinearEnterHandler {
	return func(model *initLinearEditorModel) (bool, tea.Cmd) {
		if model.focused < 0 || model.focused >= len(model.document) {
			return false, nil
		}
		if model.document[model.focused].ID != fieldID {
			return false, nil
		}
		resultAction, err := handle(model, model.document.selectedValue(fieldID))
		if err != nil {
			model.document[model.focused].Error = err.Error()
			model.relayout()
			model.ensureFocusedVisible()
			return true, nil
		}
		if resultAction != "" {
			model.resultAction = resultAction
			return true, tea.Quit
		}
		return true, nil
	}
}

func initLinearRestoreSelection(family, name string) string {
	return "__restore_" + family + "__:" + name
}

func initLinearRestoreSelectionName(family, selection string) (string, bool) {
	prefix := initLinearRestoreSelection(family, "")
	if !strings.HasPrefix(selection, prefix) {
		return "", false
	}
	return strings.TrimPrefix(selection, prefix), true
}

func initLinearSetSelectionOptions(model *initLinearEditorModel, fieldID initLinearFieldID, options []huh.Option[string], selected string, deletable, restorable func(string) bool) {
	index := model.document.fieldIndexByID(fieldID)
	if index < 0 {
		return
	}
	linearOptions := initLinearOptionsFromHuh(options, selected)
	for index := range linearOptions {
		linearOptions[index].Deletable = deletable(linearOptions[index].Value)
		linearOptions[index].Restorable = restorable(linearOptions[index].Value)
	}
	model.document[index].Options = linearOptions
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
		onDelete:      editor.OnDelete,
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
			m.setYOffset(m.viewport.YOffset - max(m.viewport.Height/2, 1))
			return m, nil
		case "pgdown", "f", " ":
			m.setYOffset(m.viewport.YOffset + max(m.viewport.Height/2, 1))
			return m, nil
		case "up", "down", "j", "k":
			// Up/Down only changes the focused select. Inputs should not leak
			// these keys to the viewport and scroll the whole form.
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
	return m, nil
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

func (d *initLinearDocument) addEditableInput(id initLinearFieldID, title, description, value string, validate func(string) error, options ...initLinearFieldOptions) {
	d.addInputField(initLinearFieldInput, id, title, description, value, true, validate, mergedInitLinearFieldOptions(options))
}

func (d *initLinearDocument) addEditableSecretInput(id initLinearFieldID, title, description, value string, validate func(string) error, options ...initLinearFieldOptions) {
	merged := mergedInitLinearFieldOptions(options)
	merged.Secret = true
	d.addInputField(initLinearFieldInput, id, title, description, value, true, validate, merged)
}

func (d *initLinearDocument) addEditableTextarea(id initLinearFieldID, title, description, value string) {
	d.addInputField(initLinearFieldTextarea, id, title, description, value, true, nil, initLinearFieldOptions{})
}

func (d *initLinearDocument) addEditableSecretTextarea(id initLinearFieldID, title, description, value string) {
	d.addInputField(initLinearFieldTextarea, id, title, description, value, true, nil, initLinearFieldOptions{Secret: true})
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
		Secret:      options.Secret,
		Validate:    validate,
	})
}

func mergedInitLinearFieldOptions(options []initLinearFieldOptions) initLinearFieldOptions {
	var merged initLinearFieldOptions
	for _, option := range options {
		if option.Hidden {
			merged.Hidden = true
		}
		if option.Secret {
			merged.Secret = true
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
	case tea.KeySpace:
		if msg.Alt {
			return false
		}
		field.Value = initLinearInsertRunes(field.Value, field.Cursor, []rune{' '})
		field.Cursor++
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
			if m.onDelete != nil {
				if handled, cmd := m.onDelete(m, m.focused); handled {
					return true, cmd
				}
			}
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

func (m *initLinearEditorModel) setFieldTitle(id initLinearFieldID, title string) {
	index := m.document.fieldIndexByID(id)
	if index < 0 {
		return
	}
	m.document[index].Title = title
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
	m.setYOffset(m.viewport.YOffset)
}

func (m *initLinearEditorModel) setYOffset(offset int) {
	m.viewport.YOffset = min(max(offset, 0), m.maxYOffset())
}

func (m initLinearEditorModel) maxYOffset() int {
	return max(m.layout.Lines-max(m.viewport.Height, 1), 0)
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
		m.setYOffset(bounds.Start)
	case bounds.Start >= bottom:
		m.setYOffset(bounds.Start)
	case bounds.End > bottom:
		if bounds.End-bounds.Start >= height {
			m.setYOffset(bounds.Start)
			return
		}
		m.setYOffset(max(bounds.End-height, 0))
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
		if field.Secret {
			value = initLinearMaskedSecretValue(field.Value, field.Cursor, focused && field.Editable)
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
			prefix := initSelectOptionPrefix(focused, option.Selected)
			initLinearAppendWrappedWithPrefixMarked(lines, selectedLines, prefix, option.Label, width, option.Selected)
		}
	}
}

func initLinearMaskedSecretValue(value string, cursor int, focused bool) string {
	lines := strings.Split(value, "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	cursorLine := 0
	cursorColumn := cursor
	seen := 0
	if focused {
		for index, line := range lines {
			lineLen := len([]rune(line))
			if cursor <= seen+lineLen {
				cursorLine = index
				cursorColumn = max(cursor-seen, 0)
				break
			}
			seen += lineLen + 1
			cursorLine = index
			cursorColumn = lineLen
		}
	}
	for index, line := range lines {
		masked := strings.Repeat("*", len([]rune(line)))
		if focused && index == cursorLine {
			masked = initLinearValueWithCursor(masked, min(cursorColumn, len([]rune(masked))))
		}
		lines[index] = masked
	}
	return strings.Join(lines, "\n")
}

func initSelectOptionPrefix(focused bool, selected bool) string {
	focusMarker := "  "
	if focused && selected {
		focusMarker = "> "
	}
	selectedMarker := "[ ] "
	if selected {
		selectedMarker = "[x] "
	}
	return focusMarker + selectedMarker
}

func initLinearAppendWrappedWithPrefix(lines *[]string, prefix string, text string, width int) {
	initLinearAppendWrappedWithPrefixMarked(lines, nil, prefix, text, width, false)
}

func initLinearAppendWrappedWithPrefixMarked(lines *[]string, selectedLines map[int]bool, prefix string, text string, width int, selected bool) {
	start := len(*lines)
	for _, rawLine := range strings.Split(text, "\n") {
		initLinearAppendWrappedLineWithPrefix(lines, prefix, rawLine, width)
	}
	markInitLinearSelectedLines(selectedLines, selected, start, len(*lines))
}

func initLinearAppendWrappedLineWithPrefix(lines *[]string, prefix string, text string, width int) {
	remaining := trimInitLinearSpaceRunes([]rune(text))
	if len(remaining) == 0 {
		*lines = append(*lines, prefix)
		return
	}
	linePrefix := prefix
	for len(remaining) > 0 {
		available := max(width-len([]rune(linePrefix)), 1)
		cut := len(remaining)
		if cut > available {
			cut = available
			for index := cut - 1; index > 0; index-- {
				if remaining[index] == ' ' {
					cut = index
					break
				}
			}
		}
		if cut <= 0 {
			cut = min(available, len(remaining))
		}
		segmentRunes := trimInitLinearSpaceRunes(remaining[:cut])
		if len(segmentRunes) == 0 {
			segmentRunes = remaining[:cut]
		}
		*lines = append(*lines, linePrefix+string(segmentRunes))
		remaining = trimInitLinearSpaceRunes(remaining[cut:])
		linePrefix = strings.Repeat(" ", len([]rune(prefix)))
	}
}

func trimInitLinearSpaceRunes(value []rune) []rune {
	start := 0
	for start < len(value) && unicode.IsSpace(value[start]) {
		start++
	}
	end := len(value)
	for end > start && unicode.IsSpace(value[end-1]) {
		end--
	}
	return value[start:end]
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
	lines := m.visibleViewportLines()
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

func (m initLinearEditorModel) visibleViewportLines() []string {
	if m.layout.Content == "" {
		return []string{""}
	}
	lines := strings.Split(m.layout.Content, "\n")
	top := min(max(m.viewport.YOffset, 0), len(lines))
	bottom := min(top+max(m.viewport.Height, 1), len(lines))
	return append([]string(nil), lines[top:bottom]...)
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
