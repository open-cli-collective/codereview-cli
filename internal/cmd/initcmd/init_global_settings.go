package initcmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/open-cli-collective/codereview-cli/internal/cmd/root"
	"github.com/open-cli-collective/codereview-cli/internal/config"
)

type bubbleTeaInitRetentionPrompter struct {
	stdin        io.Reader
	stderr       io.Writer
	editorRunner initEditorRunner
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
	model, err := runInitEditor(editor, p.stdin, p.stderr, p.editorRunner, "global settings")
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
		OnEnter: initLinearActionEnterHandler(initRetentionFieldAction, func(model *initLinearEditorModel, action string) (string, error) {
			model.document[model.focused].Error = ""
			switch action {
			case initDetailActionBack:
				return initDetailActionBack, nil
			case initDetailActionEdit:
				if _, err := initRetentionFromDocument(retention, model.document); err != nil {
					return "", err
				}
				return initDetailActionEdit, nil
			default:
				return "", nil
			}
		}),
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
