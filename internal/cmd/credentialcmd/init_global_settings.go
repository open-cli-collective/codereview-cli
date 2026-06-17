package credentialcmd

import (
	"fmt"
	"io"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

type initRetentionEditorRunner func(initLinearEditor, io.Reader, io.Writer) (initLinearEditorModel, error)

type bubbleTeaInitRetentionPrompter struct {
	stdin        io.Reader
	stderr       io.Writer
	editorRunner initRetentionEditorRunner
}

const (
	initRetentionFieldMaxAge initLinearFieldID = "retention_max_age"
	initRetentionFieldAction initLinearFieldID = "retention_action"
)

func newBubbleTeaInitRetentionPrompter(opts *root.Options) initRetentionPrompter {
	return bubbleTeaInitRetentionPrompter{stdin: opts.Stdin, stderr: opts.Stderr}
}

func (p bubbleTeaInitRetentionPrompter) EditRetention(prompt initRetentionPrompt) (initRetentionEdit, error) {
	editor := initRetentionEditor(prompt.Retention)
	model, err := p.runEditor(editor)
	if err != nil {
		return initRetentionEdit{}, err
	}
	switch model.resultAction {
	case initDetailActionEdit:
		retention, err := initRetentionFromDocument(prompt.Retention, model.document)
		if err != nil {
			return initRetentionEdit{}, err
		}
		return initRetentionEdit{Apply: true, Retention: retention}, nil
	default:
		return initRetentionEdit{}, errInitNavigateBack
	}
}

func (p bubbleTeaInitRetentionPrompter) runEditor(editor initLinearEditor) (initLinearEditorModel, error) {
	if p.editorRunner != nil {
		return p.editorRunner(editor, p.stdin, p.stderr)
	}
	program := tea.NewProgram(newInitLinearEditorModel(editor, 100, 28), tea.WithInput(p.stdin), tea.WithOutput(p.stderr))
	finalModel, err := program.Run()
	if err != nil {
		return initLinearEditorModel{}, err
	}
	model, ok := finalModel.(initLinearEditorModel)
	if !ok {
		return initLinearEditorModel{}, fmt.Errorf("global settings editor returned %T", finalModel)
	}
	return model, nil
}

func initRetentionEditor(retention config.RetentionConfig) initLinearEditor {
	maxAgeDays := fmt.Sprintf("%d", retention.MaxAgeDaysValue())
	if retention.MaxAgeDays == nil {
		maxAgeDays = fmt.Sprintf("%d", config.DefaultRetentionConfig().MaxAgeDaysValue())
	}
	var document initLinearDocument
	document.addSection("Global settings", "Configure behavior that applies across review profiles.")
	document.addSection("Run data", "Run data is cr's local record of review runs and related artifacts/logs. Retention controls how long old posted-review run data is kept locally.")
	document.addEditableInput(
		initRetentionFieldMaxAge,
		"Maximum run-data age in days",
		"How long cr keeps local run metadata and artifacts from posted reviews. Use 0 to keep posted-review run data indefinitely. Leave blank to reset to 90 days.",
		maxAgeDays,
		validateInteractiveRetentionMaxAgeDaysField,
	)
	document.addEditableSelect(initRetentionFieldAction, "Global settings action", "", []huh.Option[string]{
		huh.NewOption("Stage global settings", initDetailActionEdit),
		huh.NewOption("Back without staging", initDetailActionBack),
	}, initDetailActionEdit)
	return initLinearEditor{
		Document: document,
		OnEnter: func(model *initLinearEditorModel) (bool, tea.Cmd) {
			if model.focused < 0 || model.focused >= len(model.document) {
				return false, nil
			}
			if model.document[model.focused].ID != initRetentionFieldAction {
				return false, nil
			}
			model.document[model.focused].Error = ""
			switch model.document.selectedValue(initRetentionFieldAction) {
			case initDetailActionBack:
				model.resultAction = initDetailActionBack
				return true, tea.Quit
			case initDetailActionEdit:
				if _, err := initRetentionFromDocument(retention, model.document); err != nil {
					model.document[model.focused].Error = err.Error()
					model.relayout()
					model.ensureFocusedVisible()
					return true, nil
				}
				model.resultAction = initDetailActionEdit
				return true, tea.Quit
			default:
				return true, nil
			}
		},
	}
}

func initRetentionFromDocument(retention config.RetentionConfig, document initLinearDocument) (config.RetentionConfig, error) {
	next := config.RetentionConfig{
		Enforcement: retention.Enforcement,
	}
	value := strings.TrimSpace(document.fieldValue(initRetentionFieldMaxAge))
	if value == "" {
		defaultDays := config.DefaultRetentionConfig().MaxAgeDaysValue()
		next.MaxAgeDays = &defaultDays
		return next, nil
	}
	days, err := parseInteractiveRetentionMaxAgeDays(value)
	if err != nil {
		return config.RetentionConfig{}, err
	}
	next.MaxAgeDays = &days
	return next, nil
}
