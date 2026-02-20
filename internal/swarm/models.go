package swarm

import (
	"fmt"
	"math/rand"
	"sync"
	"time"
)

func init() {
	rand.Seed(time.Now().UnixNano())
}

// Trace event types
const (
	EventWorkerRegistered = "worker_registered"
	EventLockAcquired     = "lock_acquired"
	EventLockReleased     = "lock_released"
	EventLockHeartbeat    = "lock_heartbeat"
	EventLockExpired      = "lock_expired"
	EventLockForced       = "lock_forced"
	EventLockFailed       = "lock_failed"
)

// Issue statuses
const (
	IssueOpen       = "open"
	IssueInProgress = "in_progress"
	IssueDone       = "done"
	IssueCanceled   = "canceled"
)

// Issue task statuses
const (
	IssueTaskOpen       = "open"
	IssueTaskInProgress = "in_progress"
	IssueTaskDone       = "done"
	IssueTaskBlocked    = "blocked"
	IssueTaskCanceled   = "canceled"
)

// Issue task review verdicts
const (
	VerdictApproved = "approved"
	VerdictRejected = "rejected"
)

// IssueEvent types
const (
	EventIssueCreated      = "issue_created"
	EventIssueDelivered    = "issue_delivered"
	EventIssueClosed       = "issue_closed"
	EventIssueReopened     = "issue_reopened"
	EventIssueExpired      = "issue_expired"
	EventIssueTaskCreated  = "issue_task_created"
	EventIssueTaskClaimed  = "issue_task_claimed"
	EventIssueTaskExpired  = "issue_task_expired"
	EventIssueTaskReviewed = "issue_task_reviewed"
	EventIssueTaskResolved = "issue_task_resolved"
	EventIssueTaskMessage  = "issue_task_message"
	EventIssueTaskReset    = "issue_task_reset"
)

// Delivery statuses
const (
	DeliveryOpen     = "open"
	DeliveryInReview = "in_review"
	DeliveryApproved = "approved"
	DeliveryRejected = "rejected"
)

// Submission statuses
const (
	SubmissionOpen     = "open"
	SubmissionApproved = "approved"
	SubmissionRejected = "rejected"
)

// TaskMessage statuses
const (
	MessageOpen     = "open"
	MessageReplied  = "replied"
	MessageResolved = "resolved"
)

// InboxItem types
const (
	InboxTypeSubmission   = "submission"
	InboxTypeQuestion     = "question"
	InboxTypeBlocker      = "blocker"
	InboxTypeDelivery     = "delivery"
	InboxTypeReply        = "reply"
	InboxTypeReviewResult = "review_result"
)

// InboxItem statuses
const (
	InboxPending    = "pending"
	InboxProcessing = "processing"
	InboxDone       = "done"
)

// New event types for entities
const (
	EventSubmissionCreated  = "submission_created"
	EventSubmissionReviewed = "submission_reviewed"
	EventMessageCreated     = "message_created"
	EventMessageReplied     = "message_replied"
)

// Submission is a first-class entity created when a worker submits work.
// Task status does NOT change to "submitted"; submission has its own state machine.
type Submission struct {
	ID              string              `json:"id"`
	IssueID         string              `json:"issue_id"`
	TaskID          string              `json:"task_id"`
	WorkerID        string              `json:"worker_id"`
	Artifacts       SubmissionArtifacts `json:"artifacts"`
	Status          string              `json:"status"` // open/approved/rejected
	Feedback        string              `json:"feedback,omitempty"`
	ReviewArtifacts ReviewArtifacts     `json:"review_artifacts,omitempty"`
	FeedbackDetails []FeedbackDetail    `json:"feedback_details,omitempty"`
	CompletionScore int                 `json:"completion_score,omitempty"`
	NextStepToken   string              `json:"next_step_token,omitempty"`
	ReviewedBy      string              `json:"reviewed_by,omitempty"`
	CreatedAt       string              `json:"created_at"`
	UpdatedAt       string              `json:"updated_at"`
}

// TaskMessage is a first-class entity for worker↔lead Q&A threads.
// It has its own state machine so both sides can track resolution.
type TaskMessage struct {
	ID           string `json:"id"`
	IssueID      string `json:"issue_id"`
	TaskID       string `json:"task_id"`
	SenderID     string `json:"sender_id"`
	Kind         string `json:"kind"` // question/blocker
	Content      string `json:"content"`
	Refs         string `json:"refs"`
	Status       string `json:"status"` // open/replied/resolved
	ReplyContent string `json:"reply_content,omitempty"`
	ReplyBy      string `json:"reply_by,omitempty"`
	RepliedAt    string `json:"replied_at,omitempty"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
}

// InboxItem is a reliable delivery unit in the lead/worker inbox queues.
// It enables single-consumer semantics and prevents duplicate processing.
type InboxItem struct {
	ID               string `json:"id"`
	IssueID          string `json:"issue_id"`
	TaskID           string `json:"task_id"`
	Type             string `json:"type"`   // InboxType* constant
	RefID            string `json:"ref_id"` // submission_id or message_id
	SenderID         string `json:"sender_id"`
	Target           string `json:"target"` // "lead" or worker_id
	Status           string `json:"status"` // pending/processing/done
	ClaimedBy        string `json:"claimed_by,omitempty"`
	ClaimExpiresAtMs int64  `json:"claim_expires_at_ms,omitempty"`
	CreatedAt        string `json:"created_at"`
	UpdatedAt        string `json:"updated_at"`
}

type Worker struct {
	ID        string `json:"id"`
	JoinedAt  string `json:"joined_at"`
	UpdatedAt string `json:"updated_at"`
}

type FileLock struct {
	LeaseID       string `json:"lease_id"`
	Owner         string `json:"owner"`
	TaskID        string `json:"task_id"`
	File          string `json:"file"`
	AcquiredAt    string `json:"acquired_at"`
	ExpiresAt     string `json:"expires_at"`
	LastHeartbeat string `json:"last_heartbeat"`
}

type Lease struct {
	LeaseID       string   `json:"lease_id"`
	Owner         string   `json:"owner"`
	TaskID        string   `json:"task_id"`
	Files         []string `json:"files"`
	AcquiredAt    string   `json:"acquired_at"`
	ExpiresAt     string   `json:"expires_at"`
	LastHeartbeat string   `json:"last_heartbeat"`
}

type TraceEvent struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Actor     string `json:"actor"`
	Subject   string `json:"subject"`
	Detail    string `json:"detail"`
	Timestamp string `json:"timestamp"`
}

type Issue struct {
	ID               string   `json:"id"`
	Subject          string   `json:"subject"`
	Description      string   `json:"description"`
	SharedDocPaths   []string `json:"shared_doc_paths"`
	ProjectDocPaths  []string `json:"project_doc_paths"`
	Docs             []DocRef `json:"docs"`
	Status           string   `json:"status"`
	LeaseExpiresAtMs int64    `json:"lease_expires_at_ms"`
	CreatedAt        string   `json:"created_at"`
	UpdatedAt        string   `json:"updated_at"`
}

type DocRef struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type SubmissionArtifacts struct {
	Summary      string   `json:"summary"`
	ChangedFiles []string `json:"changed_files"`
	Diff         string   `json:"diff"`
	Links        []string `json:"links"`
	TestCases    []string `json:"test_cases"`
	TestResult   string   `json:"test_result"`
	TestOutput   string   `json:"test_output"`
}

type DeliveryArtifacts struct {
	TestResult   string   `json:"test_result"`
	TestCases    []string `json:"test_cases"`
	ChangedFiles []string `json:"changed_files"`
	ReviewedRefs []string `json:"reviewed_refs"`
	TestOutput   string   `json:"test_output"`
	KnownRisks   string   `json:"known_risks"`
}

type CommandResult struct {
	Command  string `json:"command"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code"`
	Output   string `json:"output"`
}

type TestEvidence struct {
	ScriptPath   string `json:"script_path"`
	ScriptCmd    string `json:"script_cmd"`
	ScriptPassed bool   `json:"script_passed"`
	ScriptResult string `json:"script_result"`

	DocPath     string          `json:"doc_path"`
	DocCommands []string        `json:"doc_commands"`
	DocResults  []CommandResult `json:"doc_results"`
	DocPassed   bool            `json:"doc_passed"`
}

type Verification struct {
	ScriptPassed bool   `json:"script_passed"`
	ScriptResult string `json:"script_result"`

	DocPassed  bool            `json:"doc_passed"`
	DocResults []CommandResult `json:"doc_results"`
}

type Delivery struct {
	ID               string            `json:"id"`
	IssueID          string            `json:"issue_id"`
	Summary          string            `json:"summary"`
	Refs             string            `json:"refs"`
	Artifacts        DeliveryArtifacts `json:"artifacts"`
	TestEvidence     TestEvidence      `json:"test_evidence"`
	Verification     Verification      `json:"verification"`
	Status           string            `json:"status"`
	DeliveredBy      string            `json:"delivered_by"`
	ClaimedBy        string            `json:"claimed_by"`
	ReviewedBy       string            `json:"reviewed_by"`
	Feedback         string            `json:"feedback"`
	DeliveredAt      string            `json:"delivered_at"`
	ClaimedAt        string            `json:"claimed_at"`
	ReviewedAt       string            `json:"reviewed_at"`
	LeaseExpiresAtMs int64             `json:"lease_expires_at_ms"`
	UpdatedAt        string            `json:"updated_at"`
}

type ReviewArtifacts struct {
	ReviewSummary string   `json:"review_summary"`
	ReviewedRefs  []string `json:"reviewed_refs"`
}

type FeedbackDetail struct {
	Dimension  string `json:"dimension"`
	Severity   string `json:"severity"`
	FilePath   string `json:"file_path"`
	LineRange  string `json:"line_range"`
	Content    string `json:"content"`
	Suggestion string `json:"suggestion"`
}

type IssueWorkerState struct {
	IssueID              string `json:"issue_id"`
	WorkerID             string `json:"worker_id"`
	TotalPoints          int    `json:"total_points"`
	ConsecutiveLowScores int    `json:"consecutive_low_scores"`
	UpdatedAt            string `json:"updated_at"`
}

type NextStep struct {
	Type   string `json:"type"`
	TaskID string `json:"task_id,omitempty"`
}

type NextStepToken struct {
	Token      string   `json:"token"`
	IssueID    string   `json:"issue_id"`
	Actor      string   `json:"actor"`
	NextStep   NextStep `json:"next_step"`
	Attached   bool     `json:"attached"`
	AttachedAt string   `json:"attached_at"`
	Used       bool     `json:"used"`
	CreatedAt  string   `json:"created_at"`
	UsedAt     string   `json:"used_at"`
}

type IssueTask struct {
	ID                  string              `json:"id"`
	IssueID             string              `json:"issue_id"`
	Subject             string              `json:"subject"`
	Description         string              `json:"description"`
	Difficulty          string              `json:"difficulty"`
	SplitFrom           string              `json:"split_from"`
	SplitReason         string              `json:"split_reason"`
	ImpactScope         string              `json:"impact_scope"`
	ContextTaskIDs      []string            `json:"context_task_ids"`
	SuggestedFiles      []string            `json:"suggested_files"`
	Labels              []string            `json:"labels"`
	DocPaths            []string            `json:"doc_paths"`
	RequiredIssueDocs   []string            `json:"required_issue_docs"`
	RequiredTaskDocs    []string            `json:"required_task_docs"`
	TaskDocs            []DocRef            `json:"task_docs"`
	Points              int                 `json:"points"`
	Status              string              `json:"status"`
	ReservedToken       string              `json:"reserved_token"`
	ReservedUntilMs     int64               `json:"reserved_until_ms"`
	LeaseExpiresAtMs    int64               `json:"lease_expires_at_ms"`
	ClaimedBy           string              `json:"claimed_by"`
	Submitter           string              `json:"submitter"`
	Submission          string              `json:"submission"`
	Refs                string              `json:"refs"`
	SubmissionArtifacts SubmissionArtifacts `json:"submission_artifacts"`
	Verdict             string              `json:"verdict"`
	Feedback            string              `json:"feedback"`
	CompletionScore     int                 `json:"completion_score"`
	ReviewArtifacts     ReviewArtifacts     `json:"review_artifacts"`
	FeedbackDetails     []FeedbackDetail    `json:"feedback_details"`
	NextStepToken       string              `json:"next_step_token"`
	CreatedAt           string              `json:"created_at"`
	UpdatedAt           string              `json:"updated_at"`
}

type IssueEvent struct {
	Seq       int64  `json:"seq"`
	Type      string `json:"type"`
	ParentSeq int64  `json:"parent_seq,omitempty"`
	IssueID   string `json:"issue_id"`
	TaskID    string `json:"task_id"`
	Actor     string `json:"actor"`
	Kind      string `json:"kind"`
	Detail    string `json:"detail"`
	Refs      string `json:"refs"`
	// Entity IDs for threading (new in v2 model)
	SubmissionID        string               `json:"submission_id,omitempty"`
	MessageID           string               `json:"message_id,omitempty"`
	DeliveryArtifacts   *DeliveryArtifacts   `json:"delivery_artifacts,omitempty"`
	SubmissionArtifacts *SubmissionArtifacts `json:"submission_artifacts,omitempty"`
	ReviewArtifacts     *ReviewArtifacts     `json:"review_artifacts,omitempty"`
	FeedbackDetails     []FeedbackDetail     `json:"feedback_details,omitempty"`
	CompletionScore     int                  `json:"completion_score,omitempty"`
	NextStep            *NextStep            `json:"next_step,omitempty"`
	NextStepToken       string               `json:"next_step_token,omitempty"`
	Timestamp           string               `json:"timestamp"`
}

type issueMeta struct {
	NextSeq     int64 `json:"next_seq"`
	NextTaskNum int64 `json:"next_task_num"`
}

type IssueService struct {
	store *Store
	trace *TraceService

	issueTTLSec       int
	taskTTLSec        int
	defaultTimeoutSec int

	mu       sync.Mutex
	cond     *sync.Cond
	versions map[string]int64
}

func NowStr() string {
	return time.Now().UTC().Format(time.RFC3339)
}

func GenID(prefix string) string {
	return fmt.Sprintf("%s_%d_%04x", prefix, time.Now().UnixMilli(), rand.Intn(0xFFFF))
}
