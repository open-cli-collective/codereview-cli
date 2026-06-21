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
	"github.com/open-cli-collective/codereview-cli/internal/credentials"
)

type bubbleTeaInitProfileV2Prompter struct {
	stdin               io.Reader
	stderr              io.Writer
	inventoryRunner     initInventoryRunner
	llmRuntimePrompter  initLLMRuntimePrompter
	profileEditorRunner initProfileV2EditorRunner
}

type initProfileV2EditorRunner func(initProfileV2Editor) (initProfileV2EditorResult, error)

var initProfileV2Theme = initLinearTheme

func newBubbleTeaInitProfileV2Prompter(opts *root.Options) initPrompter {
	return bubbleTeaInitProfileV2Prompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func (p bubbleTeaInitProfileV2Prompter) Run(ctx initPromptContext) (initDraft, error) {
	if initProfileV2ShouldCreateDirectly(ctx) {
		draft, staged, err := p.runProfileEditor(ctx, initCreateProfileSentinel)
		if err != nil {
			return initDraft{}, err
		}
		if staged {
			return draft, nil
		}
		return initDraft{}, errInitNavigateBack
	}

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

func initProfileV2ShouldCreateDirectly(ctx initPromptContext) bool {
	return len(ctx.ExistingProfileNames) == 0 && len(ctx.PendingProfileDeletes) == 0
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
	credentialStoreOptions     []huh.Option[string]
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
		credentialStoreOptions:     append([]huh.Option[string](nil), editor.CredentialStoreOptions...),
		selectedGitScope:           editor.SelectedGitScope,
		initialGitStorageLabel:     editor.InitialGitStorageLabel,
		gitStorageLabelUsesDefault: editor.GitStorageLabelUsesDefault,
		document:                   editor.Document,
		focused:                    editor.Document.firstFocusableField(),
	}
	model.syncGitScopeFields()
	model.syncLLMCredentialFields(false)
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
			m.quitting = true
			return m, tea.Quit
		}
		if next, handled, cmd := m.handleProfileActionKey(msg); handled {
			return next, cmd
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
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(msg)
	return m, cmd
}

func (m initProfileV2ReadOnlyModel) View() string {
	if m.quitting {
		return ""
	}
	help := "tab/enter next - shift+tab previous - up/down change select - esc back"
	if m.focused >= 0 && m.focused < len(m.document) && m.document[m.focused].Kind == initProfileV2FieldTextarea {
		help = "tab/enter next - shift+tab previous - ctrl+j newline - esc back"
	}
	return m.styleVisibleViewport() + "\n\n" + initProfileV2Theme.help.Render(help)
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
	draft := seedInteractiveInitDraft(requestedProfileName, selectedProfileName, selectedExistingProfile)
	routeText := formatInitRouteSpecs(currentProfileRouteSpecs(ctx.ExistingConfig.RepositoryProfiles, selectedProfileName))

	selectedGitScope := ctx.ProfileGitScopes[selectedProfileName]
	if selectedGitScope == "" {
		selectedGitScope = initCustomGitScopeSelection
	}
	selectedGit := initGitScopeDraft{
		Host:            draft.GitHost,
		AuthMode:        config.GitAuthMode(draft.GitAuth),
		CredentialStore: initCredentialStoreDraftValue(draft.GitCredentialStore),
		CredentialRef:   strings.TrimSpace(draft.GitCredentialRef),
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
	standardLLMCredentialRef, err := initStandardLLMCredentialRef(draft.ProfileName, selectedLLMRuntime, llmRuntimes)
	if err != nil {
		return initProfileV2Editor{}, err
	}
	if strings.TrimSpace(draft.LLMCredentialRef) == "" {
		draft.LLMCredentialRef = standardLLMCredentialRef
	}
	storeOptions := initCredentialStoreOptions(ctx.ExistingConfig)

	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addEditableInput(initProfileV2FieldProfileName, "Profile name", "", draft.ProfileName, validateProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	initProfileV2AppendGitScopeSection(&document, selectedGitScope, initGitScopeOptions(ctx.GitScopes), draft, selectedGitScope == initCustomGitScopeSelection || len(ctx.GitScopes) > 1)
	document.addEditableSelect(initProfileV2FieldReviewerEntity, "Reviewer entity", reviewerEntitySelectionDescription(), reviewerEntityOptions, selectedReviewerEntity)
	document.addEditableSelect(initProfileV2FieldLLMRuntime, "LLM runtime", "Choose how reviewer agents run for this profile.", llmRuntimeOptions, selectedLLMRuntime)
	initProfileV2AppendLLMStorageSection(&document, storeOptions, draft.LLMCredentialStore, draft.LLMCredentialRef, !initLLMStorageLabelRelevant(selectedLLMRuntime, llmRuntimes))
	document.addEditableSelect(initProfileV2FieldReviewerModelTier, initReviewerModelTierTitle, initReviewerModelTierDescription, initReviewerModelTierOptions(), draft.LLMReviewerModelTier)
	initProfileV2AppendModelMapSection(&document, modelMapLLM, draft.ModelMap)
	initProfileV2AppendAgentSourcesSection(&document, draft.AgentSources)
	initProfileV2AppendReviewPolicySection(&document, draft.ReviewPolicy)
	initProfileV2AppendGitStorageSection(&document, storeOptions, draft.GitCredentialStore, gitStorageLabel)
	initProfileV2AppendProfileActionSection(&document)
	return initProfileV2Editor{
		Draft:                      draft,
		GitScopes:                  maps.Clone(ctx.GitScopes),
		ReviewerEntities:           maps.Clone(ctx.ReviewerEntities),
		LLMRuntimes:                maps.Clone(llmRuntimes),
		CredentialStoreOptions:     storeOptions,
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

type initProfileV2FieldKind = initLinearFieldKind
type initProfileV2FieldID = initLinearFieldID

const (
	initProfileV2FieldSection  initProfileV2FieldKind = initLinearFieldSection
	initProfileV2FieldInput    initProfileV2FieldKind = initLinearFieldInput
	initProfileV2FieldSelect   initProfileV2FieldKind = initLinearFieldSelect
	initProfileV2FieldTextarea initProfileV2FieldKind = initLinearFieldTextarea
)

const (
	initProfileV2FieldProfileName          initProfileV2FieldID = "profile_name"
	initProfileV2FieldRoutes               initProfileV2FieldID = "routes"
	initProfileV2FieldGitScope             initProfileV2FieldID = "git_scope"
	initProfileV2FieldGitHost              initProfileV2FieldID = "git_host"
	initProfileV2FieldGitAuth              initProfileV2FieldID = "git_auth"
	initProfileV2FieldReviewerEntity       initProfileV2FieldID = "reviewer_entity"
	initProfileV2FieldLLMRuntime           initProfileV2FieldID = "llm_runtime"
	initProfileV2FieldReviewerModelTier    initProfileV2FieldID = "reviewer_model_tier"
	initProfileV2FieldAgentSources         initProfileV2FieldID = "agent_sources"
	initProfileV2FieldReviewMajorEvent     initProfileV2FieldID = "review_major_event"
	initProfileV2FieldSelfApprove          initProfileV2FieldID = "self_approve"
	initProfileV2FieldResolveThreads       initProfileV2FieldID = "resolve_threads"
	initProfileV2FieldResolveAfter         initProfileV2FieldID = "resolve_after"
	initProfileV2FieldGitCredentialStore   initProfileV2FieldID = "git_credential_store" // #nosec G101 -- this is a field ID, not a secret value.
	initProfileV2FieldGitCredentialName    initProfileV2FieldID = "git_credential_name"  // #nosec G101 -- this is a field ID, not a secret value.
	initProfileV2FieldLLMCredentialSection initProfileV2FieldID = "llm_credentials_section"
	initProfileV2FieldLLMCredentialStore   initProfileV2FieldID = "llm_credential_store" // #nosec G101 -- this is a field ID, not a secret value.
	initProfileV2FieldLLMCredentialName    initProfileV2FieldID = "llm_credential_name"  // #nosec G101 -- this is a field ID, not a secret value.
	initProfileV2FieldProfileAction        initProfileV2FieldID = "profile_action"
)

func initProfileV2FieldModelMap(tier config.ModelTier) initProfileV2FieldID {
	return initProfileV2FieldID("model_map_" + string(tier))
}

type initProfileV2Editor struct {
	Draft                      initDraft
	GitScopes                  map[string]initGitScopeDraft
	ReviewerEntities           map[string]initReviewerEntityDraft
	LLMRuntimes                map[string]initLLMRuntimeDraft
	CredentialStoreOptions     []huh.Option[string]
	SelectedGitScope           string
	InitialGitStorageLabel     string
	GitStorageLabelUsesDefault bool
	Document                   initProfileV2Document
}

type initProfileV2Document = initLinearDocument
type initProfileV2Field = initLinearField
type initProfileV2FieldOptions = initLinearFieldOptions
type initProfileV2Layout = initLinearLayout
type initProfileV2FieldBounds = initLinearFieldBounds

func initProfileV2AppendRouteSection(document *initProfileV2Document, routeText string) {
	document.addSection("Automatic profile selection", "Routes tell cr when to use this profile automatically. Explicit --profile still wins; otherwise a matching route is required.")
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

func initProfileV2AppendGitStorageSection(document *initProfileV2Document, storeOptions []huh.Option[string], storeID string, credentialName string) {
	document.addSection("Git credentials", "Choose where this profile's Git credentials are stored. The credential name is the full codereview/... path written to the selected store.")
	document.addEditableSelect(
		initProfileV2FieldGitCredentialStore,
		"Git credential store",
		"",
		storeOptions,
		normalizeInitStringSelectionValue(initCredentialStoreDraftValue(storeID), storeOptions),
	)
	document.addEditableInput(
		initProfileV2FieldGitCredentialName,
		"Git credential name",
		"Full credential name under the selected store.",
		credentialName,
		validateRequiredCredentialRef,
	)
}

func initProfileV2AppendLLMStorageSection(document *initProfileV2Document, storeOptions []huh.Option[string], storeID string, credentialName string, hidden bool) {
	document.addSectionField(initProfileV2FieldLLMCredentialSection, "LLM API key credentials", "Choose where this profile's LLM API key is stored.", initLinearFieldOptions{Hidden: hidden})
	document.addEditableSelect(
		initProfileV2FieldLLMCredentialStore,
		"LLM credential store",
		"",
		storeOptions,
		normalizeInitStringSelectionValue(initCredentialStoreDraftValue(storeID), storeOptions),
		initLinearFieldOptions{Hidden: hidden},
	)
	document.addEditableInput(
		initProfileV2FieldLLMCredentialName,
		"LLM credential name",
		"Full credential name under the selected store.",
		credentialName,
		validateRequiredCredentialRef,
		initLinearFieldOptions{Hidden: hidden},
	)
}

func initProfileV2AppendProfileActionSection(document *initProfileV2Document) {
	document.addEditableSelect(initProfileV2FieldProfileAction, "Profile action", "", []huh.Option[string]{
		huh.NewOption("Stage profile settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionEdit)
}

func initProfileV2AddSelect[T comparable](document *initProfileV2Document, title, description string, options []huh.Option[T], selected T) {
	initLinearAddSelect(document, title, description, options, selected)
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
	case tea.KeyCtrlW:
		field.Value, field.Cursor = initLinearDeleteWordBeforeCursor(field.Value, field.Cursor)
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
	case "up", "k":
		initProfileV2MoveSelection(field, -1)
	case "down", "j", " ":
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
		m.quitting = true
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
		m.quitting = true
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
	id := m.document[index].ID
	if id == initProfileV2FieldGitScope {
		m.syncGitScopeFields()
		return
	}
	if id == initProfileV2FieldLLMRuntime {
		m.syncLLMCredentialFields(true)
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
	if m.document.fieldIndexByID(initProfileV2FieldGitCredentialName) >= 0 {
		gitName := strings.TrimSpace(m.document.fieldValue(initProfileV2FieldGitCredentialName))
		if err := validateRequiredCredentialRef(gitName); err != nil {
			return draft, err
		}
		draft.GitCredentialStore = initCredentialStoreDraftValue(m.document.selectedValue(initProfileV2FieldGitCredentialStore))
		draft.GitCredentialRef = gitName
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
	if config.LLMAuth(draft.LLMAuth) == config.LLMAuthAPIKey {
		llmName := strings.TrimSpace(m.document.fieldValue(initProfileV2FieldLLMCredentialName))
		if err := validateRequiredCredentialRef(llmName); err != nil {
			return draft, err
		}
		draft.LLMCredentialStore = initCredentialStoreDraftValue(m.document.selectedValue(initProfileV2FieldLLMCredentialStore))
		draft.LLMCredentialRef = llmName
	} else {
		draft.LLMCredentialStore = initCredentialStoreDefaultID()
		draft.LLMCredentialRef = ""
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

func (m initProfileV2ReadOnlyModel) normalizeStorageLabels(*initDraft, string, string, string) error {
	return nil
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
			m.selectFieldValue(initProfileV2FieldGitCredentialStore, initCredentialStoreDraftValue(scope.CredentialStore))
			if strings.TrimSpace(scope.CredentialRef) != "" {
				m.setFieldValue(initProfileV2FieldGitCredentialName, scope.CredentialRef)
			}
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

func (m *initProfileV2ReadOnlyModel) syncLLMCredentialFields(reset bool) {
	selectedLLMRuntime := m.document.selectedValue(initProfileV2FieldLLMRuntime)
	draft := m.draft
	if selectedLLMRuntime != "" && selectedLLMRuntime != initConfigureNewLLMRuntimeSelection {
		applyLLMRuntimeInventorySelection(&draft, selectedLLMRuntime, m.llmRuntimes)
		selectedRuntimePreset := string(initLLMRuntimeDraftFromSeedDraft(draft).Preset)
		applyLLMRuntimeSelection(&draft, selectedRuntimePreset)
	}
	needsCredential := config.LLMAuth(draft.LLMAuth) == config.LLMAuthAPIKey
	m.setFieldHidden(initProfileV2FieldLLMCredentialSection, !needsCredential)
	m.setFieldHidden(initProfileV2FieldLLMCredentialStore, !needsCredential)
	m.setFieldHidden(initProfileV2FieldLLMCredentialName, !needsCredential)
	if !needsCredential {
		return
	}
	if reset {
		if strings.TrimSpace(draft.LLMCredentialRef) == "" {
			ref, err := credentials.FormatRef(strings.TrimSpace(m.document.fieldValue(initProfileV2FieldProfileName)) + "-llm")
			if err == nil {
				draft.LLMCredentialRef = ref
			}
		}
		m.selectFieldValue(initProfileV2FieldLLMCredentialStore, initCredentialStoreDraftValue(draft.LLMCredentialStore))
		m.setFieldValue(initProfileV2FieldLLMCredentialName, draft.LLMCredentialRef)
	}
	m.validateField(m.document.fieldIndexByID(initProfileV2FieldLLMCredentialName))
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
	}
	initProfileV2AppendWrappedWithPrefix(lines, titlePrefix, field.Title, width)
	initProfileV2AppendWrappedWithPrefix(lines, titlePrefix, field.Description, width)
	if strings.TrimSpace(field.Error) != "" {
		initProfileV2AppendWrappedWithPrefix(lines, titlePrefix+"! ", field.Error, width)
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
			if focused && index == 0 {
				prefix = "> "
			}
			initProfileV2AppendWrappedWithPrefix(lines, prefix, line, width)
		}
	case initProfileV2FieldSelect:
		for _, option := range field.Options {
			prefix := initSelectOptionPrefix(focused, option.Selected)
			initProfileV2AppendWrappedWithPrefix(lines, prefix, option.Label, width)
		}
	}
}

func initProfileV2AppendWrappedWithPrefix(lines *[]string, prefix string, text string, width int) {
	available := max(width-len([]rune(prefix)), 1)
	remaining := []rune(strings.TrimSpace(text))
	if len(remaining) == 0 {
		*lines = append(*lines, prefix)
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
}

func (m initProfileV2ReadOnlyModel) styleVisibleViewport() string {
	lines := strings.Split(m.viewport.View(), "\n")
	activeStart := -1
	activeEnd := -1
	if m.focused >= 0 && m.focused < len(m.layout.Bounds) {
		bounds := m.layout.Bounds[m.focused]
		activeStart = bounds.Start - m.viewport.YOffset
		activeEnd = bounds.End - m.viewport.YOffset
	}
	for index, line := range lines {
		active := index >= activeStart && index < activeEnd
		lines[index] = initProfileV2StyleViewportLine(line, active)
	}
	return strings.Join(lines, "\n")
}

func initProfileV2StyleViewportLine(line string, active bool) string {
	trimmed := strings.TrimSpace(line)
	switch {
	case trimmed == "":
		return line
	case strings.HasPrefix(trimmed, "! "):
		return initProfileV2Theme.error.Render(line)
	case strings.HasPrefix(trimmed, "> "):
		return initLinearStyleSelectedLine(line)
	case active && initProfileV2LooksLikeHeading(trimmed):
		return initProfileV2Theme.activeTitle.Render(line)
	case initProfileV2LooksLikeHeading(trimmed):
		return initProfileV2Theme.title.Render(line)
	default:
		return line
	}
}

var initProfileV2HeadingSet = func() map[string]bool {
	headings := map[string]bool{
		"Profile":                     true,
		"Profile name":                true,
		"Automatic profile selection": true,
		"Accepted route formats":      true,
		"Route entries":               true,
		"Git scope":                   true,
		"Git scope host":              true,
		"Git scope auth mode":         true,
		"Reviewer entity":             true,
		"LLM runtime":                 true,
		"Minimum reviewer model tier": true,
		"Model tier mapping":          true,
		"Additional reviewer-agent directories (optional)": true,
		"Additional trusted reviewer-agent directories":    true,
		"Review Policy":           true,
		"Major findings event":    true,
		"Allow self-approve":      true,
		"Resolve threads":         true,
		"Resolve-after duration":  true,
		"Git credentials":         true,
		"Git credential store":    true,
		"Git credential name":     true,
		"LLM API key credentials": true,
		"LLM credential store":    true,
		"LLM credential name":     true,
		"Profile action":          true,
	}
	for _, tier := range config.ModelTiers() {
		headings[fmt.Sprintf("%s model", tier)] = true
	}
	return headings
}()

func initProfileV2LooksLikeHeading(line string) bool {
	return initProfileV2HeadingSet[line]
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
