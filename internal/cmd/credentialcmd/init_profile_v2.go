package credentialcmd

import (
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

type bubbleTeaInitProfileV2Prompter struct {
	stdin           io.Reader
	stderr          io.Writer
	inventoryRunner initInventoryRunner
}

func newBubbleTeaInitProfileV2Prompter(opts *root.Options) initPrompter {
	return bubbleTeaInitProfileV2Prompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func (p bubbleTeaInitProfileV2Prompter) Run(ctx initPromptContext) (initDraft, error) {
	for {
		result, err := p.runInventory(initInventoryPrompt{
			Title:       "Review Profile",
			Description: "Choose a profile to edit or create.",
			Rows:        initProfileV2InventoryRows(ctx),
			Width:       80,
			Height:      20,
		})
		if err != nil {
			return initDraft{}, err
		}
		switch result.Action {
		case initInventoryActionBack:
			return initDraft{}, errInitNavigateBack
		case initInventoryActionEdit, initInventoryActionCommand:
			editor, err := initProfileV2ReadOnlyEditor(ctx, result.Row.ID)
			if err != nil {
				return initDraft{}, err
			}
			if err := p.runReadOnlyEditor(editor); err != nil {
				return initDraft{}, err
			}
		case initInventoryActionNone:
			continue
		case initInventoryActionRestore, initInventoryActionStageDelete:
			continue
		default:
			return initDraft{}, fmt.Errorf("unsupported profile v2 inventory action %q", result.Action)
		}
	}
}

func (p bubbleTeaInitProfileV2Prompter) runInventory(prompt initInventoryPrompt) (initInventoryResult, error) {
	runner := p.inventoryRunner
	if runner == nil {
		runner = runInitInventory
	}
	return runner(prompt, p.stdin, p.stderr)
}

func (p bubbleTeaInitProfileV2Prompter) runReadOnlyEditor(editor initProfileV2Editor) error {
	program := tea.NewProgram(newInitProfileV2ReadOnlyModel(editor, 100, 28), tea.WithInput(p.stdin), tea.WithOutput(p.stderr))
	_, err := program.Run()
	return err
}

func initProfileV2InventoryRows(ctx initPromptContext) []initInventoryRow {
	rows := initProfileInventoryRows(ctx)
	for i := range rows {
		rows[i].Deletable = false
		rows[i].Restorable = false
	}
	return rows
}

type initProfileV2ReadOnlyModel struct {
	viewport         viewport.Model
	draft            initDraft
	gitScopes        map[string]initGitScopeDraft
	selectedGitScope string
	document         initProfileV2Document
	layout           initProfileV2Layout
	focused          int
	quitting         bool
}

func newInitProfileV2ReadOnlyModel(editor initProfileV2Editor, width, height int) initProfileV2ReadOnlyModel {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 28
	}
	vp := viewport.New(width, max(height-2, 1))
	model := initProfileV2ReadOnlyModel{
		viewport:         vp,
		draft:            editor.Draft,
		gitScopes:        maps.Clone(editor.GitScopes),
		selectedGitScope: editor.SelectedGitScope,
		document:         editor.Document,
		focused:          editor.Document.firstFocusableField(),
	}
	model.syncGitScopeFields()
	model.validateAll()
	model.relayout()
	model.ensureFocusedVisible()
	return model
}

func (m initProfileV2ReadOnlyModel) Init() tea.Cmd {
	return nil
}

func (m initProfileV2ReadOnlyModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
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
		if m.handleFocusedSelectKey(msg) {
			m.relayout()
			m.ensureFocusedVisible()
			return m, nil
		}
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "up", "k", "shift+tab":
			m.focused = m.document.previousFocusableField(m.focused)
			m.relayout()
			m.ensureFocusedVisible()
			return m, nil
		case "down", "j", "tab", "enter":
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

func (m initProfileV2ReadOnlyModel) View() string {
	if m.quitting {
		return ""
	}
	return m.viewport.View() + "\n\nup/down focus - enter next - shift+tab previous - left/right change select - esc back"
}

func initProfileV2ReadOnlyContent(ctx initPromptContext, selection string) (string, error) {
	document, err := initProfileV2ReadOnlyDocument(ctx, selection)
	if err != nil {
		return "", err
	}
	return initProfileV2LayoutDocument(document, 100, document.firstFocusableField()).Content, nil
}

func initProfileV2ReadOnlyDocument(ctx initPromptContext, selection string) (initProfileV2Document, error) {
	editor, err := initProfileV2ReadOnlyEditor(ctx, selection)
	if err != nil {
		return nil, err
	}
	return editor.Document, nil
}

func initProfileV2ReadOnlyEditor(ctx initPromptContext, selection string) (initProfileV2Editor, error) {
	selectedProfileName, selectedExistingProfile, requestedProfileName := initProfileV2Selection(ctx, selection)
	draft := seedInteractiveInitDraft(requestedProfileName, selectedProfileName, ctx.DefaultProfileName, selectedExistingProfile)
	routeText := formatInitRouteSpecs(currentProfileRouteSpecs(ctx.ExistingConfig.RepositoryProfiles, selectedProfileName))

	selectedGitScope := ctx.ProfileGitScopes[selectedProfileName]
	if selectedGitScope == "" {
		selectedGitScope = initCustomGitScopeSelection
	}
	selectedGit := initGitScopeDraft{
		Host:          draft.GitHost,
		AuthMode:      config.GitAuthMode(draft.GitAuth),
		CredentialRef: strings.TrimSpace(draft.GitCredentialRef),
	}
	if scopeName := ctx.ProfileGitScopes[selectedProfileName]; scopeName != "" {
		if scope, ok := ctx.GitScopes[scopeName]; ok {
			selectedGit = scope
		}
	}

	reviewerEntity := initReviewerEntityDraftFromSeedDraft(draft)
	selectedReviewerEntity := ctx.ProfileReviewerEntities[selectedProfileName]
	if selectedReviewerEntity == "" {
		selectedReviewerEntity = string(reviewerEntity.Kind)
	} else {
		selectedReviewerEntity = normalizeReviewerEntitySelectionValue(selectedReviewerEntity, ctx.ReviewerEntities)
	}
	reviewerEntityOptions := initReviewerEntitySelectionOptions(ctx.ReviewerEntities, profileEditorReviewerEntityFallbackLabel(selectedGit, selectedExistingProfile))

	llmRuntimes := maps.Clone(ctx.LLMRuntimes)
	if llmRuntimes == nil {
		llmRuntimes = map[string]initLLMRuntimeDraft{}
	}
	llmRuntimeOptions, selectedLLMRuntime := initProfileEditorLLMRuntimeSelection(llmRuntimes, ctx.ProfileLLMRuntimes[selectedProfileName], draft)
	modelMapLLM := initProfileEditorModelMapLLM(draft, selectedLLMRuntime, llmRuntimes)

	standardGitCredentialRef, err := initStandardGitCredentialRef(draft.ProfileName, selectedGitScope, ctx.GitScopes)
	if err != nil {
		return initProfileV2Editor{}, err
	}
	gitStorageLabel := initEffectiveStorageLabelValue(draft.GitCredentialRef, standardGitCredentialRef)

	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", draft.ProfileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	initProfileV2AppendGitScopeSection(&document, selectedGitScope, initGitScopeOptions(ctx.GitScopes), draft, selectedGitScope == initCustomGitScopeSelection || len(ctx.GitScopes) > 1)
	initProfileV2AddSelect(&document, "Reviewer entity", reviewerEntitySelectionDescription(), reviewerEntityOptions, selectedReviewerEntity)
	initProfileV2AddSelect(&document, "LLM runtime", "Choose how reviewer agents run for this profile.", llmRuntimeOptions, selectedLLMRuntime)
	initProfileV2AddSelect(&document, initReviewerModelTierTitle, initReviewerModelTierDescription, initReviewerModelTierOptions(), draft.LLMReviewerModelTier)
	initProfileV2AppendModelMapSection(&document, modelMapLLM, draft.ModelMap)
	initProfileV2AppendAgentSourcesSection(&document, draft.AgentSources)
	initProfileV2AppendReviewPolicySection(&document, draft.ReviewPolicy)
	initProfileV2AppendGitStorageSection(&document, gitStorageLabel)
	initProfileV2AddSelect(&document, "Profile action", "", []huh.Option[string]{
		huh.NewOption("Stage profile settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionBack)
	return initProfileV2Editor{
		Draft:            draft,
		GitScopes:        maps.Clone(ctx.GitScopes),
		SelectedGitScope: selectedGitScope,
		Document:         document,
	}, nil
}

func initProfileV2Selection(ctx initPromptContext, selection string) (string, *config.Profile, string) {
	if selection == initCreateProfileSentinel {
		return "", nil, initCreateProfileSeedName(ctx)
	}
	profile := ctx.ExistingConfig.Profiles[selection]
	profileCopy := profile
	return selection, &profileCopy, ctx.RequestedProfileName
}

type initProfileV2FieldKind string
type initProfileV2FieldID string

const (
	initProfileV2FieldSection initProfileV2FieldKind = "section"
	initProfileV2FieldInput   initProfileV2FieldKind = "input"
	initProfileV2FieldSelect  initProfileV2FieldKind = "select"
)

const (
	initProfileV2FieldProfileName initProfileV2FieldID = "profile_name"
	initProfileV2FieldRoutes      initProfileV2FieldID = "routes"
	initProfileV2FieldGitScope    initProfileV2FieldID = "git_scope"
	initProfileV2FieldGitHost     initProfileV2FieldID = "git_host"
	initProfileV2FieldGitAuth     initProfileV2FieldID = "git_auth"
)

type initProfileV2Editor struct {
	Draft            initDraft
	GitScopes        map[string]initGitScopeDraft
	SelectedGitScope string
	Document         initProfileV2Document
}

type initProfileV2Document []initProfileV2Field

type initProfileV2Field struct {
	ID          initProfileV2FieldID
	Kind        initProfileV2FieldKind
	Title       string
	Description string
	Value       string
	Cursor      int
	Options     []initProfileV2Option
	Focusable   bool
	Editable    bool
	Hidden      bool
	Error       string
	Validate    func(string) error
}

type initProfileV2FieldOptions struct {
	Hidden bool
}

type initProfileV2Option struct {
	Label    string
	Value    string
	Selected bool
}

type initProfileV2Layout struct {
	Content string
	Bounds  []initProfileV2FieldBounds
	Lines   int
}

type initProfileV2FieldBounds struct {
	Start int
	End   int
}

func initProfileV2AppendRouteSection(document *initProfileV2Document, routeText string) {
	document.addSection("Automatic profile selection", "Routes tell cr when to use this profile automatically. Explicit --profile still wins; otherwise matching routes beat the default profile.")
	document.addSection("Accepted route formats", "host/namespace, host/namespace/repo, host/namespace [repo1, repo2], or a GitHub PR URL. Leave blank to remove all routes for this profile. Examples:\ngithub.com/YourOrg\ngithub.com/YourUsername [RepoA, RepoB] (will not match on RepoC)\ngithub.com/YourOrg/org-repo/pull/123\nSeparate multiple entries with ;.")
	document.addEditableInput(initProfileV2FieldRoutes, "Route entries", "", routeText, validateInitProfileV2RouteText)
}

func initProfileV2AppendGitScopeSection(document *initProfileV2Document, selectedScope string, scopeOptions []huh.Option[string], draft initDraft, visible bool) {
	if !visible {
		return
	}
	customHidden := selectedScope != initCustomGitScopeSelection
	document.addSection("Git scope", "Choose an existing Git scope for this profile or configure custom Git host/auth settings.")
	document.addEditableSelect(initProfileV2FieldGitScope, "Git scope", "", scopeOptions, selectedScope)
	document.addEditableInput(initProfileV2FieldGitHost, "Git scope host", "The Git host this review profile applies to, such as github.com or github.mycompany.com.", draft.GitHost, validateRequiredText("git host is required"), initProfileV2FieldOptions{Hidden: customHidden})
	document.addEditableSelect(initProfileV2FieldGitAuth, "Git scope auth mode", "", []huh.Option[string]{
		huh.NewOption("Personal access token", string(config.GitAuthModePAT)),
		huh.NewOption("GitHub App", string(config.GitAuthModeGitHubApp)),
	}, draft.GitAuth, initProfileV2FieldOptions{Hidden: customHidden})
}

func initProfileV2AppendModelMapSection(document *initProfileV2Document, llm config.LLMConfig, modelMap config.ModelMap) {
	document.addSection("Model tier mapping", "")
	existing := copyModelMap(modelMap)
	effective := config.EffectiveModelMap(applyModelMapToLLM(llm, existing))
	builtIns := config.BuiltInModelMap(llm.Provider, llm.Adapter)
	for _, tier := range config.ModelTiers() {
		value := initEffectiveModelMapInputValue(effective, tier)
		description := initModelMapInputDescription(tier, strings.TrimSpace(existing[string(tier)]), strings.TrimSpace(builtIns[string(tier)]))
		document.addInput(fmt.Sprintf("%s model", tier), description, value)
	}
}

func initProfileV2AppendAgentSourcesSection(document *initProfileV2Document, sources []string) {
	document.addSection("Additional reviewer-agent directories (optional)", "Add local directories that contain custom reviewer agent definitions for this profile. These profile-specific directories are loaded alongside repo-local agents under <repo>/.codereview/agents and any per-run --agents-dir sources.")
	document.addInput("Additional trusted reviewer-agent directories", "Paths are deduplicated and normalized before save.", strings.Join(sources, "\n"))
}

func initProfileV2AppendReviewPolicySection(document *initProfileV2Document, policy config.ReviewPolicy) {
	if policy.MajorEvent == "" {
		policy.MajorEvent = config.ReviewMajorEventComment
	}
	document.addSection("Review Policy", "")
	initProfileV2AddSelect(document, "Major findings event", "", []huh.Option[config.ReviewMajorEvent]{
		huh.NewOption("Comment", config.ReviewMajorEventComment),
		huh.NewOption("Request changes", config.ReviewMajorEventRequestChanges),
	}, policy.MajorEvent)
	initProfileV2AddSelect(document, "Allow self-approve", "", initReviewPolicySelfApproveOptions(), initReviewPolicySelfApproveChoice(policy.AllowSelfApprove))
	initProfileV2AddSelect(document, "Resolve threads", "", []huh.Option[string]{
		huh.NewOption("Use built-in default", ""),
		huh.NewOption("Auto-resolve", string(config.ResolveThreadsAuto)),
		huh.NewOption("Never resolve", string(config.ResolveThreadsNever)),
	}, string(policy.ResolveThreads))
	document.addInput("Resolve-after duration", "Optional. Leave blank to clear. Example: 24h or 30m.", policy.ResolveAfter)
}

func initProfileV2AppendGitStorageSection(document *initProfileV2Document, gitStorageLabel string) {
	document.addInput(
		"Git secrets storage label",
		"Edit only if this profile should use a different Git secret location than the selected Git scope's default. Useful for advanced deployment scenarios. Leave unchanged if you're unsure.",
		gitStorageLabel,
	)
}

func (d *initProfileV2Document) addSection(title, description string) {
	*d = append(*d, initProfileV2Field{
		Kind:        initProfileV2FieldSection,
		Title:       title,
		Description: description,
	})
}

func (d *initProfileV2Document) addInput(title, description, value string) {
	d.addInputField("", title, description, value, false, nil, initProfileV2FieldOptions{})
}

func (d *initProfileV2Document) addEditableInput(id initProfileV2FieldID, title, description, value string, validate func(string) error, options ...initProfileV2FieldOptions) {
	d.addInputField(id, title, description, value, true, validate, mergedInitProfileV2FieldOptions(options))
}

func (d *initProfileV2Document) addInputField(id initProfileV2FieldID, title, description, value string, editable bool, validate func(string) error, options initProfileV2FieldOptions) {
	*d = append(*d, initProfileV2Field{
		Kind:        initProfileV2FieldInput,
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

func mergedInitProfileV2FieldOptions(options []initProfileV2FieldOptions) initProfileV2FieldOptions {
	var merged initProfileV2FieldOptions
	for _, option := range options {
		if option.Hidden {
			merged.Hidden = true
		}
	}
	return merged
}

func initProfileV2AddSelect[T comparable](document *initProfileV2Document, title, description string, options []huh.Option[T], selected T) {
	initProfileV2AddSelectField(document, "", title, description, options, selected, false, initProfileV2FieldOptions{})
}

func (d *initProfileV2Document) addEditableSelect(id initProfileV2FieldID, title, description string, options []huh.Option[string], selected string, fieldOptions ...initProfileV2FieldOptions) {
	initProfileV2AddSelectField(d, id, title, description, options, selected, true, mergedInitProfileV2FieldOptions(fieldOptions))
}

func initProfileV2AddSelectField[T comparable](document *initProfileV2Document, id initProfileV2FieldID, title, description string, options []huh.Option[T], selected T, editable bool, fieldOptions initProfileV2FieldOptions) {
	field := initProfileV2Field{
		Kind:        initProfileV2FieldSelect,
		ID:          id,
		Title:       title,
		Description: description,
		Focusable:   true,
		Editable:    editable,
		Hidden:      fieldOptions.Hidden,
		Options:     make([]initProfileV2Option, 0, len(options)),
	}
	for _, option := range options {
		field.Options = append(field.Options, initProfileV2Option{
			Label:    option.Key,
			Value:    fmt.Sprint(option.Value),
			Selected: option.Value == selected,
		})
	}
	*document = append(*document, field)
}

func (d initProfileV2Document) firstFocusableField() int {
	for index, field := range d {
		if field.Focusable && !field.Hidden {
			return index
		}
	}
	return 0
}

func (d initProfileV2Document) lastFocusableField() int {
	for index := len(d) - 1; index >= 0; index-- {
		if d[index].Focusable && !d[index].Hidden {
			return index
		}
	}
	return d.firstFocusableField()
}

func (d initProfileV2Document) nextFocusableField(current int) int {
	for index := current + 1; index < len(d); index++ {
		if d[index].Focusable && !d[index].Hidden {
			return index
		}
	}
	return current
}

func (d initProfileV2Document) previousFocusableField(current int) int {
	for index := current - 1; index >= 0; index-- {
		if d[index].Focusable && !d[index].Hidden {
			return index
		}
	}
	return current
}

func (d initProfileV2Document) fieldIndexByTitle(title string) int {
	for index, field := range d {
		if field.Title == title {
			return index
		}
	}
	return -1
}

func (d initProfileV2Document) fieldIndexByID(id initProfileV2FieldID) int {
	for index, field := range d {
		if field.ID == id {
			return index
		}
	}
	return -1
}

func (d initProfileV2Document) fieldValue(id initProfileV2FieldID) string {
	index := d.fieldIndexByID(id)
	if index < 0 {
		return ""
	}
	return d[index].Value
}

func (d initProfileV2Document) selectedValue(id initProfileV2FieldID) string {
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

func (m *initProfileV2ReadOnlyModel) handleFocusedInputKey(msg tea.KeyMsg) bool {
	if m.focused < 0 || m.focused >= len(m.document) {
		return false
	}
	field := &m.document[m.focused]
	if field.Kind != initProfileV2FieldInput || !field.Editable {
		return false
	}
	key := tea.Key(msg)
	//nolint:exhaustive // The text input consumes only editing keys; all other keys fall through to form navigation.
	switch key.Type {
	case tea.KeyRunes:
		if msg.Alt {
			return false
		}
		field.Value = initProfileV2InsertRunes(field.Value, field.Cursor, key.Runes)
		field.Cursor += len(key.Runes)
	case tea.KeyBackspace, tea.KeyCtrlH:
		field.Value, field.Cursor = initProfileV2DeleteBeforeCursor(field.Value, field.Cursor)
	case tea.KeyDelete, tea.KeyCtrlD:
		field.Value = initProfileV2DeleteAtCursor(field.Value, field.Cursor)
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
	case tea.KeyCtrlK:
		field.Value = initProfileV2DeleteAfterCursor(field.Value, field.Cursor)
	default:
		return false
	}
	m.afterFieldChange(m.focused)
	return true
}

func (m *initProfileV2ReadOnlyModel) handleFocusedSelectKey(msg tea.KeyMsg) bool {
	if m.focused < 0 || m.focused >= len(m.document) {
		return false
	}
	field := &m.document[m.focused]
	if field.Kind != initProfileV2FieldSelect || !field.Editable || len(field.Options) == 0 {
		return false
	}
	switch msg.String() {
	case "left", "h":
		initProfileV2MoveSelection(field, -1)
	case "right", "l", " ":
		initProfileV2MoveSelection(field, 1)
	default:
		return false
	}
	m.afterFieldChange(m.focused)
	return true
}

func initProfileV2MoveSelection(field *initProfileV2Field, offset int) {
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

func (m *initProfileV2ReadOnlyModel) afterFieldChange(index int) {
	m.validateField(index)
	if index >= 0 && index < len(m.document) && m.document[index].ID == initProfileV2FieldGitScope {
		m.syncGitScopeFields()
	}
}

func (m *initProfileV2ReadOnlyModel) validateAll() {
	for index := range m.document {
		m.validateField(index)
	}
}

func (m *initProfileV2ReadOnlyModel) validateField(index int) {
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

func (m initProfileV2ReadOnlyModel) validatedDraft() (initDraft, error) {
	draft := m.draft
	profileName := m.document.fieldValue(initProfileV2FieldProfileName)
	if err := validateProfileName(profileName); err != nil {
		return draft, err
	}
	routes, err := parseInitRouteSpecs(m.document.fieldValue(initProfileV2FieldRoutes))
	if err != nil {
		return draft, err
	}
	draft.ProfileName = profileName
	selectedGitScope := m.document.selectedValue(initProfileV2FieldGitScope)
	if selectedGitScope == "" {
		selectedGitScope = m.selectedGitScope
	}
	if selectedGitScope == "" {
		selectedGitScope = initCustomGitScopeSelection
	}
	if selectedGitScope == initCustomGitScopeSelection {
		gitHost := m.document.fieldValue(initProfileV2FieldGitHost)
		if strings.TrimSpace(gitHost) != "" {
			draft.GitHost = strings.TrimSpace(gitHost)
		}
		gitAuth := m.document.selectedValue(initProfileV2FieldGitAuth)
		if gitAuth != "" {
			draft.GitAuth = gitAuth
		}
	} else {
		applyGitScopeSelection(&draft, selectedGitScope, m.gitScopes)
	}
	if _, err := applyInitProfileRoutes(nil, draft.ProfileName, draft.GitHost, routes); err != nil {
		return draft, err
	}
	draft.RoutesSet = true
	draft.Routes = routes
	return draft, nil
}

func (m *initProfileV2ReadOnlyModel) syncGitScopeFields() {
	selectedGitScope := m.document.selectedValue(initProfileV2FieldGitScope)
	if selectedGitScope == "" {
		selectedGitScope = m.selectedGitScope
	}
	if selectedGitScope == "" {
		selectedGitScope = initCustomGitScopeSelection
	}
	m.selectedGitScope = selectedGitScope
	custom := selectedGitScope == initCustomGitScopeSelection
	if !custom {
		if scope, ok := m.gitScopes[selectedGitScope]; ok {
			m.setFieldValue(initProfileV2FieldGitHost, scope.Host)
			m.selectFieldValue(initProfileV2FieldGitAuth, string(scope.AuthMode))
		}
	}
	m.setFieldHidden(initProfileV2FieldGitHost, !custom)
	m.setFieldHidden(initProfileV2FieldGitAuth, !custom)
	if m.focused >= 0 && m.focused < len(m.document) && m.document[m.focused].Hidden {
		m.focused = m.document.nextFocusableField(m.focused)
		if m.focused >= len(m.document) || m.document[m.focused].Hidden {
			m.focused = m.document.previousFocusableField(len(m.document))
		}
	}
	m.validateAll()
}

func (m *initProfileV2ReadOnlyModel) setFieldValue(id initProfileV2FieldID, value string) {
	index := m.document.fieldIndexByID(id)
	if index < 0 {
		return
	}
	m.document[index].Value = value
	m.document[index].Cursor = len([]rune(value))
}

func (m *initProfileV2ReadOnlyModel) selectFieldValue(id initProfileV2FieldID, value string) {
	index := m.document.fieldIndexByID(id)
	if index < 0 {
		return
	}
	for optionIndex := range m.document[index].Options {
		m.document[index].Options[optionIndex].Selected = m.document[index].Options[optionIndex].Value == value
	}
}

func (m *initProfileV2ReadOnlyModel) setFieldHidden(id initProfileV2FieldID, hidden bool) {
	index := m.document.fieldIndexByID(id)
	if index < 0 {
		return
	}
	m.document[index].Hidden = hidden
}

func (m *initProfileV2ReadOnlyModel) relayout() {
	m.layout = initProfileV2LayoutDocument(m.document, m.viewport.Width, m.focused)
	m.viewport.SetContent(m.layout.Content)
}

func (m *initProfileV2ReadOnlyModel) ensureFocusedVisible() {
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
	case bounds.End > bottom:
		if bounds.End-bounds.Start >= height {
			m.viewport.SetYOffset(bounds.Start)
			return
		}
		m.viewport.SetYOffset(max(bounds.End-height, 0))
	}
}

func initProfileV2LayoutDocument(document initProfileV2Document, width int, focused int) initProfileV2Layout {
	width = max(width, 20)
	lines := []string{}
	bounds := make([]initProfileV2FieldBounds, len(document))
	for index, field := range document {
		if field.Hidden {
			bounds[index] = initProfileV2FieldBounds{Start: len(lines), End: len(lines)}
			continue
		}
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		start := len(lines)
		initProfileV2AppendFieldLines(&lines, field, index == focused, width)
		bounds[index] = initProfileV2FieldBounds{Start: start, End: len(lines)}
	}
	return initProfileV2Layout{
		Content: strings.TrimRight(strings.Join(lines, "\n"), "\n"),
		Bounds:  bounds,
		Lines:   len(lines),
	}
}

func initProfileV2AppendFieldLines(lines *[]string, field initProfileV2Field, focused bool, width int) {
	titlePrefix := ""
	if field.Focusable {
		titlePrefix = "  "
		if focused {
			titlePrefix = "> "
		}
	}
	initProfileV2AppendWrappedWithPrefix(lines, titlePrefix, field.Title, width)
	initProfileV2AppendWrapped(lines, field.Description, width)
	if strings.TrimSpace(field.Error) != "" {
		initProfileV2AppendWrappedWithPrefix(lines, "! ", field.Error, width)
	}
	switch field.Kind {
	case initProfileV2FieldSection:
	case initProfileV2FieldInput:
		value := field.Value
		if focused && field.Editable {
			value = initProfileV2ValueWithCursor(value, field.Cursor)
		}
		valueLines := strings.Split(value, "\n")
		if len(valueLines) == 0 {
			valueLines = []string{""}
		}
		for index, line := range valueLines {
			prefix := "  "
			if index == 0 {
				prefix = "> "
			}
			initProfileV2AppendWrappedWithPrefix(lines, prefix, line, width)
		}
	case initProfileV2FieldSelect:
		for _, option := range field.Options {
			prefix := "  "
			if option.Selected {
				prefix = "> "
			}
			initProfileV2AppendWrappedWithPrefix(lines, prefix, option.Label, width)
		}
	}
}

func initProfileV2AppendWrapped(lines *[]string, text string, width int) {
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		initProfileV2AppendWrappedWithPrefix(lines, "", strings.TrimSpace(line), width)
	}
}

func initProfileV2AppendWrappedWithPrefix(lines *[]string, prefix string, text string, width int) {
	available := max(width-len(prefix), 1)
	remaining := strings.TrimSpace(text)
	if remaining == "" {
		*lines = append(*lines, prefix)
		return
	}
	for len(remaining) > available {
		cut := strings.LastIndex(remaining[:available+1], " ")
		if cut <= 0 {
			cut = available
		}
		*lines = append(*lines, prefix+strings.TrimSpace(remaining[:cut]))
		remaining = strings.TrimSpace(remaining[cut:])
		prefix = strings.Repeat(" ", len(prefix))
		available = max(width-len(prefix), 1)
	}
	*lines = append(*lines, prefix+remaining)
}

func validateInitProfileV2RouteText(value string) error {
	_, err := parseInitRouteSpecs(value)
	return err
}

func initProfileV2InsertRunes(value string, cursor int, runes []rune) string {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	next := make([]rune, 0, len(existing)+len(runes))
	next = append(next, existing[:cursor]...)
	next = append(next, runes...)
	next = append(next, existing[cursor:]...)
	return string(next)
}

func initProfileV2DeleteBeforeCursor(value string, cursor int) (string, int) {
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

func initProfileV2DeleteAtCursor(value string, cursor int) string {
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

func initProfileV2DeleteAfterCursor(value string, cursor int) string {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	return string(existing[:cursor])
}

func initProfileV2ValueWithCursor(value string, cursor int) string {
	existing := []rune(value)
	cursor = min(max(cursor, 0), len(existing))
	next := make([]rune, 0, len(existing)+1)
	next = append(next, existing[:cursor]...)
	next = append(next, '|')
	next = append(next, existing[cursor:]...)
	return string(next)
}
