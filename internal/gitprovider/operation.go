// Package gitprovider defines the provider-neutral git host seam.
package gitprovider

// Operation identifies a GitProvider method for fake error injection and
// provider-error metadata.
type Operation string

// GitProvider operation names.
const (
	OperationWhoAmI             Operation = "WhoAmI"
	OperationReviewAuthority    Operation = "ReviewAuthority"
	OperationGetPR              Operation = "GetPR"
	OperationGetDiff            Operation = "GetDiff"
	OperationGetDiffBetweenRefs Operation = "GetDiffBetweenRefs"
	OperationGetFileAtRef       Operation = "GetFileAtRef"
	OperationListTreeAtRef      Operation = "ListTreeAtRef"
	OperationListInlineThreads  Operation = "ListInlineThreads"
	OperationListReviews        Operation = "ListReviews"
	OperationListIssueComments  Operation = "ListIssueComments"
	OperationPostInlineComment  Operation = "PostInlineComment"
	OperationReplyToThread      Operation = "ReplyToThread"
	OperationResolveThread      Operation = "ResolveThread"
	OperationPostIssueComment   Operation = "PostIssueComment"
	OperationSubmitReview       Operation = "SubmitReview"
)
