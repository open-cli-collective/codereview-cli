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
			content, err := initProfileV2ReadOnlyContent(ctx, result.Row.ID)
			if err != nil {
				return initDraft{}, err
			}
			if err := p.runReadOnlyEditor(content); err != nil {
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

func (p bubbleTeaInitProfileV2Prompter) runReadOnlyEditor(content string) error {
	program := tea.NewProgram(newInitProfileV2ReadOnlyModel(content, 100, 28), tea.WithInput(p.stdin), tea.WithOutput(p.stderr))
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
	content  string
	quitting bool
}

func newInitProfileV2ReadOnlyModel(content string, width, height int) initProfileV2ReadOnlyModel {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 28
	}
	vp := viewport.New(width, max(height-2, 1))
	vp.SetContent(content)
	return initProfileV2ReadOnlyModel{viewport: vp, content: content}
}

func (m initProfileV2ReadOnlyModel) Init() tea.Cmd {
	return nil
}

func (m initProfileV2ReadOnlyModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.viewport.Width = max(msg.Width, 1)
		m.viewport.Height = max(msg.Height-2, 1)
		m.viewport.SetContent(m.content)
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			m.quitting = true
			return m, tea.Quit
		case "up", "k":
			m.viewport.ScrollUp(1)
		case "down", "j":
			m.viewport.ScrollDown(1)
		case "pgup", "b":
			m.viewport.HalfPageUp()
		case "pgdown", "f", " ":
			m.viewport.HalfPageDown()
		case "home", "g":
			m.viewport.GotoTop()
		case "end", "G":
			m.viewport.GotoBottom()
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
	return m.viewport.View() + "\n\nup/down scroll - esc back"
}

func initProfileV2ReadOnlyContent(ctx initPromptContext, selection string) (string, error) {
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
		return "", err
	}
	gitStorageLabel := initEffectiveStorageLabelValue(draft.GitCredentialRef, standardGitCredentialRef)

	var b strings.Builder
	initProfileV2WriteSection(&b, "Profile")
	initProfileV2WriteInput(&b, "Profile name", draft.ProfileName)
	initProfileV2WriteRouteSection(&b, routeText)
	if selectedGitScope == initCustomGitScopeSelection {
		initProfileV2WriteCustomGitScopeSection(&b, draft)
	}
	initProfileV2WriteSelect(&b, "Reviewer entity", reviewerEntitySelectionDescription(), reviewerEntityOptions, selectedReviewerEntity)
	initProfileV2WriteSelect(&b, "LLM runtime", "Choose how reviewer agents run for this profile.", llmRuntimeOptions, selectedLLMRuntime)
	initProfileV2WriteSelect(&b, initReviewerModelTierTitle, initReviewerModelTierDescription, initReviewerModelTierOptions(), draft.LLMReviewerModelTier)
	initProfileV2WriteModelMapSection(&b, modelMapLLM, draft.ModelMap)
	initProfileV2WriteAgentSourcesSection(&b, draft.AgentSources)
	initProfileV2WriteReviewPolicySection(&b, draft.ReviewPolicy)
	initProfileV2WriteGitStorageSection(&b, gitStorageLabel)
	initProfileV2WriteSelect(&b, "Profile action", "", []huh.Option[string]{
		huh.NewOption("Stage profile settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionBack)
	return strings.TrimRight(b.String(), "\n"), nil
}

func initProfileV2Selection(ctx initPromptContext, selection string) (string, *config.Profile, string) {
	if selection == initCreateProfileSentinel {
		return "", nil, initCreateProfileSeedName(ctx)
	}
	profile := ctx.ExistingConfig.Profiles[selection]
	profileCopy := profile
	return selection, &profileCopy, ctx.RequestedProfileName
}

func initProfileV2WriteRouteSection(b *strings.Builder, routeText string) {
	initProfileV2WriteSection(b, "Automatic profile selection")
	initProfileV2WriteParagraph(b, "Routes tell cr when to use this profile automatically. Explicit --profile still wins; otherwise matching routes beat the default profile.")
	initProfileV2WriteSection(b, "Accepted route formats")
	initProfileV2WriteParagraph(b, "host/namespace, host/namespace/repo, host/namespace [repo1, repo2], or a GitHub PR URL. Leave blank to remove all routes for this profile. Examples:")
	initProfileV2WriteParagraph(b, "github.com/YourOrg\ngithub.com/YourUsername [RepoA, RepoB] (will not match on RepoC)\ngithub.com/YourOrg/org-repo/pull/123\nSeparate multiple entries with ;.")
	initProfileV2WriteInput(b, "Route entries", routeText)
}

func initProfileV2WriteCustomGitScopeSection(b *strings.Builder, draft initDraft) {
	initProfileV2WriteSection(b, "Git scope")
	initProfileV2WriteParagraph(b, "Custom Git scope settings for this profile. Milestone 1 renders these read-only to preserve v1 parity visibility.")
	initProfileV2WriteInput(b, "Git scope host", draft.GitHost)
	initProfileV2WriteInput(b, "Git scope auth mode", initGitAuthModeLabel(config.GitAuthMode(draft.GitAuth)))
}

func initProfileV2WriteModelMapSection(b *strings.Builder, llm config.LLMConfig, modelMap config.ModelMap) {
	initProfileV2WriteSection(b, "Model tier mapping")
	existing := copyModelMap(modelMap)
	effective := config.EffectiveModelMap(applyModelMapToLLM(llm, existing))
	builtIns := config.BuiltInModelMap(llm.Provider, llm.Adapter)
	for _, tier := range config.ModelTiers() {
		value := initEffectiveModelMapInputValue(effective, tier)
		description := initModelMapInputDescription(tier, strings.TrimSpace(existing[string(tier)]), strings.TrimSpace(builtIns[string(tier)]))
		initProfileV2WriteInputWithDescription(b, fmt.Sprintf("%s model", tier), description, value)
	}
}

func initProfileV2WriteAgentSourcesSection(b *strings.Builder, sources []string) {
	initProfileV2WriteSection(b, "Additional reviewer-agent directories (optional)")
	initProfileV2WriteParagraph(b, "Add local directories that contain custom reviewer agent definitions for this profile. These profile-specific directories are loaded alongside repo-local agents under <repo>/.codereview/agents and any per-run --agents-dir sources.")
	initProfileV2WriteInputWithDescription(b, "Additional trusted reviewer-agent directories", "Paths are deduplicated and normalized before save.", strings.Join(sources, "\n"))
}

func initProfileV2WriteReviewPolicySection(b *strings.Builder, policy config.ReviewPolicy) {
	if policy.MajorEvent == "" {
		policy.MajorEvent = config.ReviewMajorEventComment
	}
	initProfileV2WriteSection(b, "Review Policy")
	initProfileV2WriteSelect(b, "Major findings event", "", []huh.Option[config.ReviewMajorEvent]{
		huh.NewOption("Comment", config.ReviewMajorEventComment),
		huh.NewOption("Request changes", config.ReviewMajorEventRequestChanges),
	}, policy.MajorEvent)
	initProfileV2WriteSelect(b, "Allow self-approve", "", initReviewPolicySelfApproveOptions(), initReviewPolicySelfApproveChoice(policy.AllowSelfApprove))
	initProfileV2WriteSelect(b, "Resolve threads", "", []huh.Option[string]{
		huh.NewOption("Use built-in default", ""),
		huh.NewOption("Auto-resolve", string(config.ResolveThreadsAuto)),
		huh.NewOption("Never resolve", string(config.ResolveThreadsNever)),
	}, string(policy.ResolveThreads))
	initProfileV2WriteInputWithDescription(b, "Resolve-after duration", "Optional. Leave blank to clear. Example: 24h or 30m.", policy.ResolveAfter)
}

func initProfileV2WriteGitStorageSection(b *strings.Builder, gitStorageLabel string) {
	initProfileV2WriteInputWithDescription(
		b,
		"Git secrets storage label",
		"Edit only if this profile should use a different Git secret location than the selected Git scope's default. Useful for advanced deployment scenarios. Leave unchanged if you're unsure.",
		gitStorageLabel,
	)
}

func initProfileV2WriteSection(b *strings.Builder, title string) {
	if b.Len() > 0 {
		b.WriteString("\n\n")
	}
	b.WriteString(title)
	b.WriteString("\n")
}

func initProfileV2WriteParagraph(b *strings.Builder, text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	b.WriteString(text)
	b.WriteString("\n")
}

func initProfileV2WriteInput(b *strings.Builder, title, value string) {
	initProfileV2WriteInputWithDescription(b, title, "", value)
}

func initProfileV2WriteInputWithDescription(b *strings.Builder, title, description, value string) {
	initProfileV2WriteSection(b, title)
	initProfileV2WriteParagraph(b, description)
	lines := strings.Split(value, "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	for i, line := range lines {
		if i == 0 {
			b.WriteString("> ")
		} else {
			b.WriteString("  ")
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
}

func initProfileV2WriteSelect[T comparable](b *strings.Builder, title, description string, options []huh.Option[T], selected T) {
	initProfileV2WriteSection(b, title)
	initProfileV2WriteParagraph(b, description)
	for _, option := range options {
		if option.Value == selected {
			b.WriteString("> ")
		} else {
			b.WriteString("  ")
		}
		b.WriteString(option.Key)
		b.WriteString("\n")
	}
}
