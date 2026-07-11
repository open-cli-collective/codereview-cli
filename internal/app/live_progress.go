package app

import (
	"context"
	"strconv"

	"github.com/open-cli-collective/codereview-cli/internal/approvaloverride"
	"github.com/open-cli-collective/codereview-cli/internal/ledger"
	"github.com/open-cli-collective/codereview-cli/internal/pipeline"
	"github.com/open-cli-collective/codereview-cli/internal/progress"
	"github.com/open-cli-collective/codereview-cli/internal/reviewrun"
)

type progressPlanner struct {
	planner reviewrun.Planner
	logger  *progress.Logger
	hooks   *hookDispatcher
}

type progressApprovalOverrideClassifier struct {
	classifier approvaloverride.Classifier
	logger     *progress.Logger
}

func withProgressPlanner(logger *progress.Logger, dispatcher *hookDispatcher, planner reviewrun.Planner) reviewrun.Planner {
	if dispatcher != nil && !dispatcher.enabled {
		dispatcher = nil
	}
	if planner == nil || logger == nil && dispatcher == nil {
		return planner
	}
	return progressPlanner{planner: planner, logger: logger, hooks: dispatcher}
}

func withProgressApprovalOverrideClassifier(logger *progress.Logger, classifier approvaloverride.Classifier) approvaloverride.Classifier {
	if classifier == nil || logger == nil {
		return classifier
	}
	return progressApprovalOverrideClassifier{classifier: classifier, logger: logger}
}

func (p progressPlanner) Live(ctx context.Context, req pipeline.Request, run ledger.Run) (pipeline.Result, error) {
	if p.hooks != nil {
		p.hooks.observeRun(run)
	}
	var span *progress.Span
	if p.logger != nil {
		span = p.logger.StartFields("review", "plan_live_review", "pr", progress.Field{Key: "run_id", Value: run.RunID})
	}
	result, err := p.planner.Live(ctx, req, run)
	if err == nil && p.hooks != nil {
		p.hooks.completeReview(result)
	}
	return result, endProgressSpanFields(span, err, progress.Field{Key: "run_id", Value: run.RunID})
}

func (c progressApprovalOverrideClassifier) ClassifyApprovalOverride(ctx context.Context, req approvaloverride.Request) (approvaloverride.Result, error) {
	fields := []progress.Field{{Key: "candidate_count", Value: strconv.Itoa(len(req.Candidates))}}
	span := c.logger.StartFields("review", "classify_approval_override", "pr", fields...)
	result, err := c.classifier.ClassifyApprovalOverride(ctx, req)
	return result, endProgressSpanFieldsResult(span, err, result, fields...)
}

func endProgressSpanFields(span *progress.Span, err error, fields ...progress.Field) error {
	if span != nil {
		span.EndFields(err, fields...)
	}
	return err
}

func endProgressSpanFieldsResult(span *progress.Span, err error, result approvaloverride.Result, fields ...progress.Field) error {
	fields = append(fields, progress.Field{Key: "approve", Value: strconv.FormatBool(result.Approve)})
	return endProgressSpanFields(span, err, fields...)
}
