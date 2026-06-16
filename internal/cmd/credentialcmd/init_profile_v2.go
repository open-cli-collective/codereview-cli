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
			document, err := initProfileV2ReadOnlyDocument(ctx, result.Row.ID)
			if err != nil {
				return initDraft{}, err
			}
			if err := p.runReadOnlyEditor(document); err != nil {
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

func (p bubbleTeaInitProfileV2Prompter) runReadOnlyEditor(document initProfileV2Document) error {
	program := tea.NewProgram(newInitProfileV2ReadOnlyModel(document, 100, 28), tea.WithInput(p.stdin), tea.WithOutput(p.stderr))
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
	viewport viewport.Model
	document initProfileV2Document
	layout   initProfileV2Layout
	focused  int
	quitting bool
}

func newInitProfileV2ReadOnlyModel(document initProfileV2Document, width, height int) initProfileV2ReadOnlyModel {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 28
	}
	vp := viewport.New(width, max(height-2, 1))
	model := initProfileV2ReadOnlyModel{
		viewport: vp,
		document: document,
		focused:  document.firstFocusableField(),
	}
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
	return m.viewport.View() + "\n\nup/down focus - enter next - shift+tab previous - esc back"
}

func initProfileV2ReadOnlyContent(ctx initPromptContext, selection string) (string, error) {
	document, err := initProfileV2ReadOnlyDocument(ctx, selection)
	if err != nil {
		return "", err
	}
	return initProfileV2LayoutDocument(document, 100, document.firstFocusableField()).Content, nil
}

func initProfileV2ReadOnlyDocument(ctx initPromptContext, selection string) (initProfileV2Document, error) {
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
		return nil, err
	}
	gitStorageLabel := initEffectiveStorageLabelValue(draft.GitCredentialRef, standardGitCredentialRef)

	var document initProfileV2Document
	document.addSection("Profile", "")
	document.addInput("Profile name", "", draft.ProfileName)
	initProfileV2AppendRouteSection(&document, routeText)
	if selectedGitScope == initCustomGitScopeSelection {
		initProfileV2AppendCustomGitScopeSection(&document, draft)
	}
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
	return document, nil
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

const (
	initProfileV2FieldSection initProfileV2FieldKind = "section"
	initProfileV2FieldInput   initProfileV2FieldKind = "input"
	initProfileV2FieldSelect  initProfileV2FieldKind = "select"
)

type initProfileV2Document []initProfileV2Field

type initProfileV2Field struct {
	Kind        initProfileV2FieldKind
	Title       string
	Description string
	Value       string
	Options     []initProfileV2Option
	Focusable   bool
	Error       string
}

type initProfileV2Option struct {
	Label    string
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
	document.addInput("Route entries", "", routeText)
}

func initProfileV2AppendCustomGitScopeSection(document *initProfileV2Document, draft initDraft) {
	document.addSection("Git scope", "Custom Git scope settings for this profile. Milestone 1 renders these read-only to preserve v1 parity visibility.")
	document.addInput("Git scope host", "", draft.GitHost)
	document.addInput("Git scope auth mode", "", initGitAuthModeLabel(config.GitAuthMode(draft.GitAuth)))
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
	*d = append(*d, initProfileV2Field{
		Kind:        initProfileV2FieldInput,
		Title:       title,
		Description: description,
		Value:       value,
		Focusable:   true,
	})
}

func initProfileV2AddSelect[T comparable](document *initProfileV2Document, title, description string, options []huh.Option[T], selected T) {
	field := initProfileV2Field{
		Kind:        initProfileV2FieldSelect,
		Title:       title,
		Description: description,
		Focusable:   true,
		Options:     make([]initProfileV2Option, 0, len(options)),
	}
	for _, option := range options {
		field.Options = append(field.Options, initProfileV2Option{
			Label:    option.Key,
			Selected: option.Value == selected,
		})
	}
	*document = append(*document, field)
}

func (d initProfileV2Document) firstFocusableField() int {
	for index, field := range d {
		if field.Focusable {
			return index
		}
	}
	return 0
}

func (d initProfileV2Document) lastFocusableField() int {
	for index := len(d) - 1; index >= 0; index-- {
		if d[index].Focusable {
			return index
		}
	}
	return d.firstFocusableField()
}

func (d initProfileV2Document) nextFocusableField(current int) int {
	for index := current + 1; index < len(d); index++ {
		if d[index].Focusable {
			return index
		}
	}
	return current
}

func (d initProfileV2Document) previousFocusableField(current int) int {
	for index := current - 1; index >= 0; index-- {
		if d[index].Focusable {
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
		valueLines := strings.Split(field.Value, "\n")
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
