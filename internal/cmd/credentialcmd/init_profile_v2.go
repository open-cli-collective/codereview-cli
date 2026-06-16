package credentialcmd

import (
	"errors"
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
	stdin               io.Reader
	stderr              io.Writer
	inventoryRunner     initInventoryRunner
	llmRuntimePrompter  initLLMRuntimePrompter
	profileEditorRunner initProfileV2EditorRunner
}

type initProfileV2EditorRunner func(initProfileV2Editor) (initProfileV2EditorResult, error)

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
			draft, staged, err := p.runProfileEditor(ctx, result.Row.ID)
			if err != nil {
				return initDraft{}, err
			}
			if staged {
				return draft, nil
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

func (p bubbleTeaInitProfileV2Prompter) runProfileEditor(ctx initPromptContext, selection string) (initDraft, bool, error) {
	editorCtx := ctx
	selectedProfileName, _, _ := initProfileV2Selection(ctx, selection)
	for {
		editor, err := initProfileV2ReadOnlyEditor(editorCtx, selection)
		if err != nil {
			return initDraft{}, false, err
		}
		result, err := p.runReadOnlyEditor(editor)
		if err != nil {
			return initDraft{}, false, err
		}
		if result.StageProfile {
			return result.Draft, true, nil
		}
		if !result.BootstrapLLMRuntime {
			return initDraft{}, false, nil
		}
		llmRuntimePrompter := p.llmRuntimePrompter
		if llmRuntimePrompter == nil {
			llmRuntimePrompter = huhInitLLMRuntimePrompter{
				stdin:           p.stdin,
				stderr:          p.stderr,
				checker:         defaultInitLLMRuntimeAvailabilityNote,
				inventoryRunner: p.inventoryRunner,
			}
		}
		runtimePromptCtx := editorCtx
		runtimePromptCtx.LLMRuntimes = maps.Clone(editorCtx.LLMRuntimes)
		if runtimePromptCtx.LLMRuntimes == nil {
			runtimePromptCtx.LLMRuntimes = map[string]initLLMRuntimeDraft{}
		}
		runtimeDraft, err := llmRuntimePrompter.EditLLMRuntime(initLLMRuntimePrompt{Context: runtimePromptCtx})
		if errors.Is(err, errInitNavigateBack) {
			continue
		}
		if err != nil {
			return initDraft{}, false, err
		}
		stagedRuntime := initLLMRuntimeDraftFromSeedDraft(runtimeDraft)
		editorCtx.LLMRuntimes = maps.Clone(editorCtx.LLMRuntimes)
		if editorCtx.LLMRuntimes == nil {
			editorCtx.LLMRuntimes = map[string]initLLMRuntimeDraft{}
		}
		stagedRuntime.Name = uniqueInitLLMRuntimeName(editorCtx.LLMRuntimes, stagedRuntime.suggestedName())
		editorCtx.LLMRuntimes[stagedRuntime.Name] = stagedRuntime
		editorCtx.ProfileLLMRuntimes = maps.Clone(editorCtx.ProfileLLMRuntimes)
		if editorCtx.ProfileLLMRuntimes == nil {
			editorCtx.ProfileLLMRuntimes = map[string]string{}
		}
		editorCtx.ProfileLLMRuntimes[selectedProfileName] = stagedRuntime.Name
	}
}

type initProfileV2EditorResult struct {
	BootstrapLLMRuntime bool
	StageProfile        bool
	Draft               initDraft
}

func (p bubbleTeaInitProfileV2Prompter) runReadOnlyEditor(editor initProfileV2Editor) (initProfileV2EditorResult, error) {
	if p.profileEditorRunner != nil {
		return p.profileEditorRunner(editor)
	}
	program := tea.NewProgram(newInitProfileV2ReadOnlyModel(editor, 100, 28), tea.WithInput(p.stdin), tea.WithOutput(p.stderr))
	finalModel, err := program.Run()
	if err != nil {
		return initProfileV2EditorResult{}, err
	}
	model, ok := finalModel.(initProfileV2ReadOnlyModel)
	if !ok {
		return initProfileV2EditorResult{}, fmt.Errorf("profile v2 editor returned %T", finalModel)
	}
	return model.result, nil
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
	viewport                   viewport.Model
	draft                      initDraft
	gitScopes                  map[string]initGitScopeDraft
	reviewerEntities           map[string]initReviewerEntityDraft
	llmRuntimes                map[string]initLLMRuntimeDraft
	selectedGitScope           string
	initialGitStorageLabel     string
	gitStorageLabelUsesDefault bool
	document                   initProfileV2Document
	layout                     initProfileV2Layout
	focused                    int
	quitting                   bool
	requestLLMRuntimeBootstrap bool
	result                     initProfileV2EditorResult
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
		viewport:                   vp,
		draft:                      editor.Draft,
		gitScopes:                  maps.Clone(editor.GitScopes),
		reviewerEntities:           maps.Clone(editor.ReviewerEntities),
		llmRuntimes:                maps.Clone(editor.LLMRuntimes),
		selectedGitScope:           editor.SelectedGitScope,
		initialGitStorageLabel:     editor.InitialGitStorageLabel,
		gitStorageLabelUsesDefault: editor.GitStorageLabelUsesDefault,
		document:                   editor.Document,
		focused:                    editor.Document.firstFocusableField(),
	}
	model.syncGitScopeFields()
	model.syncModelMapFields()
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
		if m.handleLLMRuntimeBootstrapKey(msg) {
			m.requestLLMRuntimeBootstrap = true
			m.result = initProfileV2EditorResult{BootstrapLLMRuntime: true}
			return m, tea.Quit
		}
		if next, handled, cmd := m.handleProfileActionKey(msg); handled {
			return next, cmd
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
	help := "up/down focus - enter next - shift+tab previous - left/right change select - esc back"
	if m.focused >= 0 && m.focused < len(m.document) && m.document[m.focused].Kind == initProfileV2FieldTextarea {
		help = "up/down focus - enter next - shift+tab previous - ctrl+j newline - esc back"
	}
	return m.viewport.View() + "\n\n" + help
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
	gitStorageLabelUsesDefault := initStorageLabelUsesDefault(gitStorageLabel, standardGitCredentialRef)

	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", draft.ProfileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	initProfileV2AppendGitScopeSection(&document, selectedGitScope, initGitScopeOptions(ctx.GitScopes), draft, selectedGitScope == initCustomGitScopeSelection || len(ctx.GitScopes) > 1)
	document.addEditableSelect(initProfileV2FieldReviewerEntity, "Reviewer entity", reviewerEntitySelectionDescription(), reviewerEntityOptions, selectedReviewerEntity)
	document.addEditableSelect(initProfileV2FieldLLMRuntime, "LLM runtime", "Choose how reviewer agents run for this profile.", llmRuntimeOptions, selectedLLMRuntime)
	document.addEditableSelect(initProfileV2FieldReviewerModelTier, initReviewerModelTierTitle, initReviewerModelTierDescription, initReviewerModelTierOptions(), draft.LLMReviewerModelTier)
	initProfileV2AppendModelMapSection(&document, modelMapLLM, draft.ModelMap)
	initProfileV2AppendAgentSourcesSection(&document, draft.AgentSources)
	initProfileV2AppendReviewPolicySection(&document, draft.ReviewPolicy)
	initProfileV2AppendGitStorageSection(&document, gitStorageLabel)
	initProfileV2AppendProfileActionSection(&document)
	return initProfileV2Editor{
		Draft:                      draft,
		GitScopes:                  maps.Clone(ctx.GitScopes),
		ReviewerEntities:           maps.Clone(ctx.ReviewerEntities),
		LLMRuntimes:                maps.Clone(llmRuntimes),
		SelectedGitScope:           selectedGitScope,
		InitialGitStorageLabel:     gitStorageLabel,
		GitStorageLabelUsesDefault: gitStorageLabelUsesDefault,
		Document:                   document,
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
	initProfileV2FieldSection  initProfileV2FieldKind = "section"
	initProfileV2FieldInput    initProfileV2FieldKind = "input"
	initProfileV2FieldSelect   initProfileV2FieldKind = "select"
	initProfileV2FieldTextarea initProfileV2FieldKind = "textarea"
)

const (
	initProfileV2FieldProfileName       initProfileV2FieldID = "profile_name"
	initProfileV2FieldRoutes            initProfileV2FieldID = "routes"
	initProfileV2FieldGitScope          initProfileV2FieldID = "git_scope"
	initProfileV2FieldGitHost           initProfileV2FieldID = "git_host"
	initProfileV2FieldGitAuth           initProfileV2FieldID = "git_auth"
	initProfileV2FieldReviewerEntity    initProfileV2FieldID = "reviewer_entity"
	initProfileV2FieldLLMRuntime        initProfileV2FieldID = "llm_runtime"
	initProfileV2FieldReviewerModelTier initProfileV2FieldID = "reviewer_model_tier"
	initProfileV2FieldAgentSources      initProfileV2FieldID = "agent_sources"
	initProfileV2FieldReviewMajorEvent  initProfileV2FieldID = "review_major_event"
	initProfileV2FieldSelfApprove       initProfileV2FieldID = "self_approve"
	initProfileV2FieldResolveThreads    initProfileV2FieldID = "resolve_threads"
	initProfileV2FieldResolveAfter      initProfileV2FieldID = "resolve_after"
	initProfileV2FieldGitStorageLabel   initProfileV2FieldID = "git_storage_label"
	initProfileV2FieldProfileAction     initProfileV2FieldID = "profile_action"
)

func initProfileV2FieldModelMap(tier config.ModelTier) initProfileV2FieldID {
	return initProfileV2FieldID("model_map_" + string(tier))
}

type initProfileV2Editor struct {
	Draft                      initDraft
	GitScopes                  map[string]initGitScopeDraft
	ReviewerEntities           map[string]initReviewerEntityDraft
	LLMRuntimes                map[string]initLLMRuntimeDraft
	SelectedGitScope           string
	InitialGitStorageLabel     string
	GitStorageLabelUsesDefault bool
	Document                   initProfileV2Document
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
		document.addEditableInput(initProfileV2FieldModelMap(tier), fmt.Sprintf("%s model", tier), description, value, nil)
	}
}

func initProfileV2AppendAgentSourcesSection(document *initProfileV2Document, sources []string) {
	document.addSection("Additional reviewer-agent directories (optional)", "Add local directories that contain custom reviewer agent definitions for this profile. These profile-specific directories are loaded alongside repo-local agents under <repo>/.codereview/agents and any per-run --agents-dir sources.")
	document.addEditableTextarea(initProfileV2FieldAgentSources, "Additional trusted reviewer-agent directories", "Paths are deduplicated and normalized before save.", strings.Join(sources, "\n"))
}

func initProfileV2AppendReviewPolicySection(document *initProfileV2Document, policy config.ReviewPolicy) {
	if policy.MajorEvent == "" {
		policy.MajorEvent = config.ReviewMajorEventComment
	}
	document.addSection("Review Policy", "")
	document.addEditableSelect(initProfileV2FieldReviewMajorEvent, "Major findings event", "", []huh.Option[string]{
		huh.NewOption("Comment", string(config.ReviewMajorEventComment)),
		huh.NewOption("Request changes", string(config.ReviewMajorEventRequestChanges)),
	}, string(policy.MajorEvent))
	document.addEditableSelect(initProfileV2FieldSelfApprove, "Allow self-approve", "", initReviewPolicySelfApproveOptions(), initReviewPolicySelfApproveChoice(policy.AllowSelfApprove))
	document.addEditableSelect(initProfileV2FieldResolveThreads, "Resolve threads", "", []huh.Option[string]{
		huh.NewOption("Use built-in default", ""),
		huh.NewOption("Auto-resolve", string(config.ResolveThreadsAuto)),
		huh.NewOption("Never resolve", string(config.ResolveThreadsNever)),
	}, string(policy.ResolveThreads))
	document.addEditableInput(initProfileV2FieldResolveAfter, "Resolve-after duration", "Optional. Leave blank to clear. Example: 24h or 30m.", policy.ResolveAfter, validateOptionalDuration)
}

func initProfileV2AppendGitStorageSection(document *initProfileV2Document, gitStorageLabel string) {
	document.addEditableInput(
		initProfileV2FieldGitStorageLabel,
		"Git secrets storage label",
		"Edit only if this profile should use a different Git secret location than the selected Git scope's default. Useful for advanced deployment scenarios. Leave unchanged if you're unsure.",
		gitStorageLabel,
		validateOptionalCredentialRef,
	)
}

func initProfileV2AppendProfileActionSection(document *initProfileV2Document) {
	document.addEditableSelect(initProfileV2FieldProfileAction, "Profile action", "", []huh.Option[string]{
		huh.NewOption("Stage profile settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionEdit)
}

func (d *initProfileV2Document) addSection(title, description string) {
	*d = append(*d, initProfileV2Field{
		Kind:        initProfileV2FieldSection,
		Title:       title,
		Description: description,
	})
}

func (d *initProfileV2Document) addInput(title, description, value string) {
	d.addInputField(initProfileV2FieldInput, "", title, description, value, false, nil, initProfileV2FieldOptions{})
}

func (d *initProfileV2Document) addEditableInput(id initProfileV2FieldID, title, description, value string, validate func(string) error, options ...initProfileV2FieldOptions) {
	d.addInputField(initProfileV2FieldInput, id, title, description, value, true, validate, mergedInitProfileV2FieldOptions(options))
}

func (d *initProfileV2Document) addEditableTextarea(id initProfileV2FieldID, title, description, value string) {
	d.addInputField(initProfileV2FieldTextarea, id, title, description, value, true, nil, initProfileV2FieldOptions{})
}

func (d *initProfileV2Document) addInputField(kind initProfileV2FieldKind, id initProfileV2FieldID, title, description, value string, editable bool, validate func(string) error, options initProfileV2FieldOptions) {
	*d = append(*d, initProfileV2Field{
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
	if (field.Kind != initProfileV2FieldInput && field.Kind != initProfileV2FieldTextarea) || !field.Editable {
		return false
	}
	if field.Kind == initProfileV2FieldTextarea && (msg.String() == "ctrl+j" || msg.String() == "alt+enter") {
		field.Value = initProfileV2InsertRunes(field.Value, field.Cursor, []rune{'\n'})
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

func (m initProfileV2ReadOnlyModel) handleLLMRuntimeBootstrapKey(msg tea.KeyMsg) bool {
	if msg.String() != "enter" || m.focused < 0 || m.focused >= len(m.document) {
		return false
	}
	field := m.document[m.focused]
	return field.ID == initProfileV2FieldLLMRuntime && m.document.selectedValue(initProfileV2FieldLLMRuntime) == initConfigureNewLLMRuntimeSelection
}

func (m initProfileV2ReadOnlyModel) handleProfileActionKey(msg tea.KeyMsg) (initProfileV2ReadOnlyModel, bool, tea.Cmd) {
	if msg.String() != "enter" || m.focused < 0 || m.focused >= len(m.document) {
		return m, false, nil
	}
	field := m.document[m.focused]
	if field.ID != initProfileV2FieldProfileAction {
		return m, false, nil
	}
	m.document[m.focused].Error = ""
	switch m.document.selectedValue(initProfileV2FieldProfileAction) {
	case initDetailActionBack:
		m.result = initProfileV2EditorResult{}
		return m, true, tea.Quit
	case initDetailActionEdit:
		draft, err := m.validatedDraft()
		if err != nil {
			m.document[m.focused].Error = err.Error()
			m.relayout()
			m.ensureFocusedVisible()
			return m, true, nil
		}
		m.result = initProfileV2EditorResult{StageProfile: true, Draft: draft}
		return m, true, tea.Quit
	default:
		return m, true, nil
	}
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
	if index < 0 || index >= len(m.document) {
		return
	}
	switch m.document[index].ID {
	case initProfileV2FieldGitScope:
		m.syncGitScopeFields()
	case initProfileV2FieldLLMRuntime:
		m.syncModelMapFields()
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
	selectedReviewerEntity := m.document.selectedValue(initProfileV2FieldReviewerEntity)
	if selectedReviewerEntity != "" {
		applyReviewerEntityInventorySelection(&draft, selectedReviewerEntity, m.reviewerEntities)
		reviewerMode := string(initReviewerEntityDraftFromSeedDraft(draft).Kind)
		applyReviewerEntitySelection(&draft, reviewerMode)
	}
	selectedLLMRuntime := m.document.selectedValue(initProfileV2FieldLLMRuntime)
	if selectedLLMRuntime == initConfigureNewLLMRuntimeSelection {
		return draft, fmt.Errorf("configure a new LLM runtime first")
	}
	if selectedLLMRuntime != "" {
		applyLLMRuntimeInventorySelection(&draft, selectedLLMRuntime, m.llmRuntimes)
		selectedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
		applyLLMRuntimeSelection(&draft, selectedRuntimePreset)
	}
	if err := m.normalizeStorageLabels(&draft, selectedGitScope, selectedReviewerEntity, selectedLLMRuntime); err != nil {
		return draft, err
	}
	if m.document.fieldIndexByID(initProfileV2FieldReviewerModelTier) >= 0 {
		draft.LLMReviewerModelTier = m.document.selectedValue(initProfileV2FieldReviewerModelTier)
	}
	if initProfileV2HasModelMapFields(m.document) {
		llm := config.LLMConfig{
			Provider: config.LLMProvider(draft.LLMProvider),
			Auth:     config.LLMAuth(draft.LLMAuth),
			Adapter:  config.LLMAdapter(draft.LLMAdapter),
		}
		draft.ModelMapSet = true
		draft.ModelMap = initProfileV2ModelMapFromDocument(llm, m.document)
	}
	if m.document.fieldIndexByID(initProfileV2FieldAgentSources) >= 0 {
		agentSources, err := normalizeInitAgentSources(initProfileV2AgentSourcesFromDocument(m.document))
		if err != nil {
			return draft, err
		}
		draft.AgentSourcesSet = true
		draft.AgentSources = agentSources
	}
	if m.document.fieldIndexByID(initProfileV2FieldReviewMajorEvent) >= 0 {
		reviewPolicy, err := initProfileV2ReviewPolicyFromDocument(m.document)
		if err != nil {
			return draft, err
		}
		draft.ReviewPolicySet = true
		draft.ReviewPolicy = reviewPolicy
	}
	draft.RoutesSet = true
	draft.Routes = routes
	return draft, nil
}

func (m initProfileV2ReadOnlyModel) normalizeStorageLabels(draft *initDraft, selectedGitScope, selectedReviewerEntity, selectedLLMRuntime string) error {
	if m.document.fieldIndexByID(initProfileV2FieldGitStorageLabel) < 0 {
		return nil
	}
	gitValue := m.document.fieldValue(initProfileV2FieldGitStorageLabel)
	if err := validateOptionalCredentialRef(gitValue); err != nil {
		return err
	}
	standardGitRef, err := initStandardGitCredentialRef(draft.ProfileName, selectedGitScope, m.gitScopes)
	if err != nil {
		return err
	}
	gitUsesDefault := initStorageLabelUsesDefault(gitValue, standardGitRef)
	if !gitUsesDefault && m.gitStorageLabelUsesDefault && strings.TrimSpace(gitValue) == strings.TrimSpace(m.initialGitStorageLabel) {
		gitUsesDefault = true
	}
	standardReviewerRef, err := initStandardReviewerCredentialRef(draft.ProfileName, selectedReviewerEntity, m.reviewerEntities)
	if err != nil {
		return err
	}
	standardLLMRef, err := initStandardLLMCredentialRef(draft.ProfileName, selectedLLMRuntime, m.llmRuntimes)
	if err != nil {
		return err
	}
	return normalizeInitProfileStorageLabels(draft, selectedGitScope, selectedReviewerEntity, selectedLLMRuntime, m.gitScopes, m.reviewerEntities, m.llmRuntimes, initStorageLabelNormalizationInput{
		Git: initStorageLabelFieldState{
			Value:       gitValue,
			UsesDefault: gitUsesDefault,
		},
		Reviewer: initStorageLabelFieldState{
			Value:       draft.ReviewerCredentialRef,
			UsesDefault: initStorageLabelUsesDefault(draft.ReviewerCredentialRef, standardReviewerRef),
		},
		LLM: initStorageLabelFieldState{
			Value:       draft.LLMCredentialRef,
			UsesDefault: initStorageLabelUsesDefault(draft.LLMCredentialRef, standardLLMRef),
		},
	})
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

func (m *initProfileV2ReadOnlyModel) syncModelMapFields() {
	if !initProfileV2HasModelMapFields(m.document) {
		return
	}
	selectedLLMRuntime := m.document.selectedValue(initProfileV2FieldLLMRuntime)
	llm := initProfileEditorModelMapLLM(m.draft, selectedLLMRuntime, m.llmRuntimes)
	existing := copyModelMap(m.draft.ModelMap)
	effective := config.EffectiveModelMap(applyModelMapToLLM(llm, existing))
	builtIns := config.BuiltInModelMap(llm.Provider, llm.Adapter)
	for _, tier := range config.ModelTiers() {
		index := m.document.fieldIndexByID(initProfileV2FieldModelMap(tier))
		if index < 0 {
			continue
		}
		value := initEffectiveModelMapInputValue(effective, tier)
		m.document[index].Value = value
		m.document[index].Cursor = len([]rune(value))
		m.document[index].Description = initModelMapInputDescription(tier, strings.TrimSpace(existing[string(tier)]), strings.TrimSpace(builtIns[string(tier)]))
		m.validateField(index)
	}
}

func initProfileV2HasModelMapFields(document initProfileV2Document) bool {
	for _, tier := range config.ModelTiers() {
		if document.fieldIndexByID(initProfileV2FieldModelMap(tier)) >= 0 {
			return true
		}
	}
	return false
}

func initProfileV2ModelMapFromDocument(llm config.LLMConfig, document initProfileV2Document) config.ModelMap {
	values := map[config.ModelTier]*string{}
	for _, tier := range config.ModelTiers() {
		index := document.fieldIndexByID(initProfileV2FieldModelMap(tier))
		if index < 0 {
			continue
		}
		value := document[index].Value
		values[tier] = &value
	}
	return initModelMapFromEditorValues(llm, values)
}

func initProfileV2AgentSourcesFromDocument(document initProfileV2Document) []string {
	value := document.fieldValue(initProfileV2FieldAgentSources)
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return strings.FieldsFunc(value, func(r rune) bool { return r == '\n' || r == '\r' })
}

func initProfileV2ReviewPolicyFromDocument(document initProfileV2Document) (config.ReviewPolicy, error) {
	majorEvent := config.ReviewMajorEvent(document.selectedValue(initProfileV2FieldReviewMajorEvent))
	if majorEvent == "" {
		majorEvent = config.ReviewMajorEventComment
	}
	resolveAfter := strings.TrimSpace(document.fieldValue(initProfileV2FieldResolveAfter))
	if err := validateOptionalDuration(resolveAfter); err != nil {
		return config.ReviewPolicy{}, err
	}
	return config.ReviewPolicy{
		MajorEvent:       majorEvent,
		AllowSelfApprove: initReviewPolicyAllowSelfApprove(document.selectedValue(initProfileV2FieldSelfApprove)),
		ResolveThreads:   config.ResolveThreadsPolicy(strings.TrimSpace(document.selectedValue(initProfileV2FieldResolveThreads))),
		ResolveAfter:     resolveAfter,
	}, nil
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
	case initProfileV2FieldInput, initProfileV2FieldTextarea:
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
