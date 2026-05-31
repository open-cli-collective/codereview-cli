package gate

import (
	"reflect"
	"testing"
)

func TestDecideCrossProduct(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want Decision
	}{
		{
			name: "no matching local or PR state runs fresh",
			req:  requestWithPR(PRSummary{State: PRStateFresh}),
			want: Decision{Kind: DecisionFresh},
		},
		{
			name: "partial marker without local row repairs",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-partial",
				Outcome: PROutcomeApproved,
			}),
			want: Decision{Kind: DecisionRepair, RunID: "run-partial", Outcome: PROutcomeApproved},
		},
		{
			name: "resumable exact row wins before complete PR marker",
			req: requestWithPR(PRSummary{
				State: PRStateCompleteReview,
				RunID: "run-complete",
			}, func(req *Request) {
				req.ExactRuns = []RunSummary{liveRun("run-resume", 1, RunStateRunning)}
			}),
			want: Decision{Kind: DecisionResume, RunID: "run-resume"},
		},
		{
			name: "resumable exact row wins before invalid PR marker",
			req: requestWithPR(PRSummary{
				State: PRState("unknown"),
			}, func(req *Request) {
				req.ExactRuns = []RunSummary{liveRun("run-resume", 1, RunStateRunning)}
			}),
			want: Decision{Kind: DecisionResume, RunID: "run-resume"},
		},
		{
			name: "latest resumable exact row resumes",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.ExactRuns = []RunSummary{
					liveRun("run-old", 1, RunStateIncomplete),
					liveRun("run-new", 3, RunStateRunning),
					liveRun("run-middle", 2, RunStateIncomplete),
				}
			}),
			want: Decision{Kind: DecisionResume, RunID: "run-new"},
		},
		{
			name: "resumable exact row wins before stale-base abort",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.ExactRuns = []RunSummary{liveRun("run-resume", 2, RunStateIncomplete)}
				req.StaleBaseCandidates = []StaleBaseCandidate{{
					Run:       liveRun("run-stale", 1, RunStateRunning),
					LockState: LockStateFree,
				}}
			}),
			want: Decision{Kind: DecisionResume, RunID: "run-resume"},
		},
		{
			name: "partial marker with failed local row stops",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-failed",
				Outcome: PROutcomeRequestChanges,
			}, func(req *Request) {
				req.PartialRun = ptrRun(runWithFailure(liveRun("run-failed", 1, RunStateFailed), FailureClassAuth))
			}),
			want: Decision{
				Kind:         DecisionError,
				RunID:        "run-failed",
				Outcome:      PROutcomeRequestChanges,
				ErrorReason:  ErrorPartialFailed,
				FailureClass: FailureClassAuth,
				Message:      "partial marker run failed; use --retry-posts after fixing the cause or --rerun",
			},
		},
		{
			name: "partial marker with aborted local row is audit-only fresh",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-aborted",
				Outcome: PROutcomeComment,
			}, func(req *Request) {
				req.PartialRun = ptrRun(liveRun("run-aborted", 1, RunStateAborted))
			}),
			want: Decision{
				Kind:    DecisionFresh,
				RunID:   "run-aborted",
				Outcome: PROutcomeComment,
				Message: "partial marker belongs to aborted run; run fresh",
			},
		},
		{
			name: "complete submit review marker exits early",
			req: requestWithPR(PRSummary{
				State: PRStateCompleteReview,
				RunID: "run-review",
			}),
			want: Decision{Kind: DecisionEarlyExit, RunID: "run-review"},
		},
		{
			name: "complete no-diff rollup exits early",
			req: requestWithPR(PRSummary{
				State:   PRStateCompleteNoDiff,
				RunID:   "run-nodiff",
				Outcome: PROutcomeNothingToReview,
			}),
			want: Decision{Kind: DecisionEarlyExit, RunID: "run-nodiff", Outcome: PROutcomeNothingToReview},
		},
		{
			name: "real verdict rollup without submit review is partial",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-rollup",
				Outcome: PROutcomeComment,
			}),
			want: Decision{Kind: DecisionRepair, RunID: "run-rollup", Outcome: PROutcomeComment},
		},
		{
			name: "stale local candidate with free lock aborts stale",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.StaleBaseCandidates = []StaleBaseCandidate{{
					Run:       liveRun("run-stale", 1, RunStateIncomplete),
					LockState: LockStateFree,
				}}
			}),
			want: Decision{Kind: DecisionAbortStale, AbortStaleRunIDs: []string{"run-stale"}},
		},
		{
			name: "stale local candidates aggregate aborts and preserve warnings",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.StaleBaseCandidates = []StaleBaseCandidate{
					{Run: liveRun("run-free-1", 1, RunStateRunning), LockState: LockStateFree},
					{Run: liveRun("run-held", 2, RunStateIncomplete), LockState: LockStateHeld, HeartbeatStale: true},
					{Run: liveRun("run-free-2", 3, RunStateIncomplete), LockState: LockStateFree},
				}
			}),
			want: Decision{
				Kind:             DecisionAbortStale,
				AbortStaleRunIDs: []string{"run-free-1", "run-free-2"},
				Warnings:         []string{"stale-base run run-held is locked and has a stale heartbeat"},
			},
		},
		{
			name: "stale local candidate with held stale heartbeat warns and continues",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.StaleBaseCandidates = []StaleBaseCandidate{{
					Run:            liveRun("run-held", 1, RunStateRunning),
					LockState:      LockStateHeld,
					HeartbeatStale: true,
				}}
			}),
			want: Decision{
				Kind:     DecisionFresh,
				Warnings: []string{"stale-base run run-held is locked and has a stale heartbeat"},
			},
		},
		{
			name: "terminal and dry-run stale candidates do not abort",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.StaleBaseCandidates = []StaleBaseCandidate{
					{Run: liveRun("run-complete", 3, RunStateApproved), LockState: LockStateFree},
					{Run: dryRun("run-dry", 4, RunStateDryRun), LockState: LockStateFree},
				}
			}),
			want: Decision{Kind: DecisionFresh},
		},
		{
			name: "stale-base PR marker is audit-only fresh",
			req: requestWithPR(PRSummary{
				State: PRStateStaleBase,
				RunID: "run-old-base",
			}),
			want: Decision{Kind: DecisionFresh, RunID: "run-old-base", Message: "stale-base marker is audit only; run fresh"},
		},
		{
			name: "rerun supersedes resumable rows and bypasses PR markers",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-partial",
				Outcome: PROutcomeApproved,
			}, func(req *Request) {
				req.Flags.Rerun = true
				req.ExactRuns = []RunSummary{
					liveRun("run-resume", 1, RunStateIncomplete),
					liveRun("run-complete", 2, RunStateApproved),
				}
				req.StaleBaseCandidates = []StaleBaseCandidate{{
					Run:       liveRun("run-stale", 3, RunStateRunning),
					LockState: LockStateFree,
				}}
			}),
			want: Decision{
				Kind:            DecisionFresh,
				SupersedeRunIDs: []string{"run-resume"},
				Message:         "--rerun bypasses gate",
			},
		},
		{
			name: "retry posts selects latest eligible live row",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.Flags.RetryPosts = true
				req.ExactRuns = []RunSummary{
					runWithPending(liveRun("run-old", 1, RunStateFailed), 1, 0),
					runWithPending(dryRun("run-dry", 4, RunStateDryRun), 5, 5),
					runWithPending(liveRun("run-new", 3, RunStateApproved), 0, 1),
				}
			}),
			want: Decision{Kind: DecisionRetryPosts, RunID: "run-new"},
		},
		{
			name: "retry posts wins before resumable exact row and stale-base abort",
			req: requestWithPR(PRSummary{
				State: PRStateCompleteReview,
				RunID: "run-complete",
			}, func(req *Request) {
				req.Flags.RetryPosts = true
				req.ExactRuns = []RunSummary{
					liveRun("run-resume", 1, RunStateIncomplete),
					runWithPending(liveRun("run-retry", 2, RunStateFailed), 1, 0),
				}
				req.StaleBaseCandidates = []StaleBaseCandidate{{
					Run:       liveRun("run-stale", 3, RunStateRunning),
					LockState: LockStateFree,
				}}
			}),
			want: Decision{Kind: DecisionRetryPosts, RunID: "run-retry"},
		},
		{
			name: "retry posts errors when no live row is eligible",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.Flags.RetryPosts = true
				req.ExactRuns = []RunSummary{
					liveRun("run-posted", 1, RunStateApproved),
					runWithPending(dryRun("run-dry", 2, RunStateDryRun), 1, 0),
				}
			}),
			want: Decision{
				Kind:        DecisionError,
				ErrorReason: ErrorRetryPostsIneligible,
				Message:     "no live run has required pending or failed_terminal actions",
			},
		},
		{
			name: "dry-run bypasses complete and resumable state",
			req: requestWithPR(PRSummary{
				State: PRStateCompleteReview,
				RunID: "run-complete",
			}, func(req *Request) {
				req.Flags.DryRun = true
				req.ExactRuns = []RunSummary{liveRun("run-resume", 1, RunStateRunning)}
			}),
			want: Decision{Kind: DecisionFresh, Message: "dry-run bypasses live gate"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Decide(tt.req); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Decide() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestDecideInvalidInputs(t *testing.T) {
	tests := []struct {
		name string
		req  Request
		want ErrorReason
	}{
		{
			name: "mutually exclusive flags",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.Flags.Rerun = true
				req.Flags.RetryPosts = true
			}),
			want: ErrorMutuallyExclusiveFlags,
		},
		{
			name: "unknown exact run state",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.ExactRuns = []RunSummary{liveRun("run-bad", 1, RunState("paused"))}
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "unknown post mode",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.ExactRuns = []RunSummary{{
					RunID:    "run-bad",
					Attempt:  1,
					PostMode: PostMode("posting"),
					State:    RunStateRunning,
				}}
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "non-positive attempt",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.ExactRuns = []RunSummary{{
					RunID:    "run-zero",
					PostMode: PostModeLive,
					State:    RunStateApproved,
				}}
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "unknown failure class",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-failed",
				Outcome: PROutcomeApproved,
			}, func(req *Request) {
				req.PartialRun = ptrRun(runWithFailure(liveRun("run-failed", 1, RunStateFailed), FailureClass("network")))
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "unknown lock state on resumable stale candidate",
			req: requestWithPR(PRSummary{State: PRStateFresh}, func(req *Request) {
				req.StaleBaseCandidates = []StaleBaseCandidate{{
					Run:       liveRun("run-stale", 1, RunStateRunning),
					LockState: LockState("unknown"),
				}}
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "unknown PR state",
			req:  requestWithPR(PRSummary{State: PRState("old")}),
			want: ErrorInvalidInput,
		},
		{
			name: "partial missing run ID",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				Outcome: PROutcomeComment,
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "partial no-diff outcome is invalid",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-no-diff",
				Outcome: PROutcomeNothingToReview,
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "partial local run must match marker run ID",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-marker",
				Outcome: PROutcomeApproved,
			}, func(req *Request) {
				req.PartialRun = ptrRun(liveRun("run-other", 1, RunStateFailed))
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "complete no-diff requires nothing_to_review outcome",
			req: requestWithPR(PRSummary{
				State:   PRStateCompleteNoDiff,
				RunID:   "run-bad",
				Outcome: PROutcomeApproved,
			}),
			want: ErrorInvalidInput,
		},
		{
			name: "partial resumable local row is inconsistent",
			req: requestWithPR(PRSummary{
				State:   PRStatePartial,
				RunID:   "run-resume",
				Outcome: PROutcomeApproved,
			}, func(req *Request) {
				req.PartialRun = ptrRun(liveRun("run-resume", 1, RunStateIncomplete))
			}),
			want: ErrorPartialResumableInconsistent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Decide(tt.req)
			if got.Kind != DecisionError || got.ErrorReason != tt.want {
				t.Fatalf("Decide() = %#v, want error reason %s", got, tt.want)
			}
		})
	}
}

func TestEnumValidity(t *testing.T) {
	for _, value := range []DecisionKind{
		DecisionResume,
		DecisionEarlyExit,
		DecisionRepair,
		DecisionRetryPosts,
		DecisionAbortStale,
		DecisionFresh,
		DecisionError,
	} {
		if !value.Valid() {
			t.Fatalf("DecisionKind(%q).Valid() = false, want true", value)
		}
	}
	if DecisionKind("done").Valid() {
		t.Fatal("DecisionKind(done).Valid() = true, want false")
	}

	for _, value := range []PostMode{PostModeLive, PostModeDryRun} {
		if !value.Valid() {
			t.Fatalf("PostMode(%q).Valid() = false, want true", value)
		}
	}
	if PostMode("posted").Valid() {
		t.Fatal("PostMode(posted).Valid() = true, want false")
	}

	for _, value := range []RunState{
		RunStateRunning,
		RunStateIncomplete,
		RunStateApproved,
		RunStateRequestChanges,
		RunStateComment,
		RunStateNothingToReview,
		RunStateDryRun,
		RunStateAborted,
		RunStateFailed,
	} {
		if !value.Valid() {
			t.Fatalf("RunState(%q).Valid() = false, want true", value)
		}
	}
	if RunState("paused").Valid() {
		t.Fatal("RunState(paused).Valid() = true, want false")
	}

	for _, value := range []FailureClass{FailureClassNone, FailureClassTerminal, FailureClassAuth} {
		if !value.Valid() {
			t.Fatalf("FailureClass(%q).Valid() = false, want true", value)
		}
	}
	if FailureClass("network").Valid() {
		t.Fatal("FailureClass(network).Valid() = true, want false")
	}

	for _, value := range []LockState{LockStateFree, LockStateHeld} {
		if !value.Valid() {
			t.Fatalf("LockState(%q).Valid() = false, want true", value)
		}
	}
	if LockState("unknown").Valid() {
		t.Fatal("LockState(unknown).Valid() = true, want false")
	}

	for _, value := range []PRState{
		PRStateFresh,
		PRStateCompleteReview,
		PRStateCompleteNoDiff,
		PRStatePartial,
		PRStateStaleBase,
	} {
		if !value.Valid() {
			t.Fatalf("PRState(%q).Valid() = false, want true", value)
		}
	}
	if PRState("complete").Valid() {
		t.Fatal("PRState(complete).Valid() = true, want false")
	}

	for _, value := range []PROutcome{
		PROutcomeApproved,
		PROutcomeRequestChanges,
		PROutcomeComment,
		PROutcomeNothingToReview,
	} {
		if !value.Valid() {
			t.Fatalf("PROutcome(%q).Valid() = false, want true", value)
		}
	}
	if PROutcome("rejected").Valid() {
		t.Fatal("PROutcome(rejected).Valid() = true, want false")
	}
	for _, value := range []PROutcome{PROutcomeApproved, PROutcomeRequestChanges, PROutcomeComment} {
		if !value.RealVerdict() {
			t.Fatalf("PROutcome(%q).RealVerdict() = false, want true", value)
		}
	}
	if PROutcomeNothingToReview.RealVerdict() {
		t.Fatal("PROutcomeNothingToReview.RealVerdict() = true, want false")
	}

	for _, value := range []ErrorReason{
		ErrorInvalidInput,
		ErrorMutuallyExclusiveFlags,
		ErrorRetryPostsIneligible,
		ErrorPartialFailed,
		ErrorPartialResumableInconsistent,
	} {
		if !value.Valid() {
			t.Fatalf("ErrorReason(%q).Valid() = false, want true", value)
		}
	}
	if ErrorReason("blocked").Valid() {
		t.Fatal("ErrorReason(blocked).Valid() = true, want false")
	}
}

func requestWithPR(pr PRSummary, mutate ...func(*Request)) Request {
	req := Request{PR: pr}
	for _, fn := range mutate {
		fn(&req)
	}
	return req
}

func liveRun(runID string, attempt int, state RunState) RunSummary {
	return RunSummary{
		RunID:    runID,
		Attempt:  attempt,
		PostMode: PostModeLive,
		State:    state,
	}
}

func dryRun(runID string, attempt int, state RunState) RunSummary {
	return RunSummary{
		RunID:    runID,
		Attempt:  attempt,
		PostMode: PostModeDryRun,
		State:    state,
	}
}

func runWithPending(run RunSummary, pending, failedTerminal int) RunSummary {
	run.RequiredPending = pending
	run.RequiredFailedTerminal = failedTerminal
	return run
}

func runWithFailure(run RunSummary, failureClass FailureClass) RunSummary {
	run.FailureClass = failureClass
	return run
}

func ptrRun(run RunSummary) *RunSummary {
	return &run
}
