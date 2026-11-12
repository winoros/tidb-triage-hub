package model

import "fmt"

const SchemaVersion = 2

var ValidCategories = []string{
	"EASY",
	"MEDIUM",
	"HARD",
	"ENHANCEMENT",
	"FILTER_RANDOM_FUZZ",
	"FILTER_AI_OBSCURE",
	"FILTER_NON_OPT_OR_FIXED",
}

var DiffRank = map[string]int{
	"EASY":                    1,
	"MEDIUM":                  2,
	"HARD":                    3,
	"ENHANCEMENT":             0,
	"FILTER_RANDOM_FUZZ":      0,
	"FILTER_AI_OBSCURE":       0,
	"FILTER_NON_OPT_OR_FIXED": 0,
}

var ValidSeverities = []string{
	"CRITICAL",
	"MAJOR",
	"MODERATE",
	"MINOR",
	"N/A",
}

var SevRank = map[string]int{
	"CRITICAL": 4,
	"MAJOR":    3,
	"MODERATE": 2,
	"MINOR":    1,
	"N/A":      0,
}

var ValidSubsystems = []string{
	"statistics",
	"plan_cache_spm_hint",
	"logical_rewrite",
	"access_path_partition_lock",
	"cbo_cost_join_reorder",
	"name_resolution_group_by",
	"tiflash_mpp",
}

const (
	StageDraftOrHold   = "DRAFT_OR_HOLD"
	StageConflict      = "CONFLICT"
	StageNeedsOKToTest = "NEEDS_OK_TO_TEST"
	StageCIFailed      = "CI_FAILED"
	StageWaitingAuthor = "WAITING_AUTHOR"
	StageGateBlocked   = "GATE_BLOCKED"
	StageNeedsLGTM2    = "NEEDS_LGTM2"
	StageWaitingReview = "WAITING_REVIEWER"
	StageReadyMerging  = "READY_MERGING"
)

var ValidBlockerStages = []string{
	StageDraftOrHold,
	StageConflict,
	StageNeedsOKToTest,
	StageCIFailed,
	StageWaitingAuthor,
	StageGateBlocked,
	StageNeedsLGTM2,
	StageWaitingReview,
	StageReadyMerging,
}

const (
	BallAuthor     = "AUTHOR"
	BallReviewer   = "REVIEWER"
	BallMergeQueue = "MERGE_QUEUE"
	BallHold       = "HOLD"
)

var ValidBallOwners = []string{
	BallReviewer,
	BallAuthor,
	BallMergeQueue,
	BallHold,
}

type LocalizedNote struct {
	ZhCN string `json:"zh-CN"`
	En   string `json:"en"`
}

func (n *LocalizedNote) IsEmpty() bool {
	return n == nil || (n.ZhCN == "" && n.En == "")
}

type LinkedPRRef struct {
	Num          int    `json:"num"`
	Repo         string `json:"repo"`
	Label        string `json:"label"`
	URL          string `json:"url"`
	State        string `json:"state"`
	Title        string `json:"title"`
	IsCherryPick bool   `json:"is_cherry_pick"`
	UpdatedAt    string `json:"updated_at"`
}

func PRKey(repo string, num int) string {
	return fmt.Sprintf("%s#%d", repo, num)
}

type IssueRecord struct {
	Number           int            `json:"number"`
	Title            string         `json:"title"`
	Author           string         `json:"author"`
	Assignees        []string       `json:"assignees"`
	Created          string         `json:"created"`
	CreatedAt        string         `json:"created_at"`
	UpdatedAt        string         `json:"updated_at"`
	UpdatedDisplay   string         `json:"updated_display"`
	Labels           []string       `json:"labels"`
	IsCanonical      bool           `json:"is_canonical"`
	IsCustomer       bool           `json:"is_customer"`
	IsBorderlineKept bool           `json:"is_borderline_kept"`
	Cat              string         `json:"cat"`
	DiffRank         int            `json:"diff_rank"`
	Severity         string         `json:"severity"`
	SevRank          int            `json:"sev_rank"`
	GHSeverity       string         `json:"gh_severity"`
	Subsystem        string         `json:"subsystem"`
	Note             *LocalizedNote `json:"note"`
	PRs              []LinkedPRRef  `json:"prs"`
	HasPR            bool           `json:"has_pr"`
	HasOpenPR        bool           `json:"has_open_pr"`
	HasAnyOpenPR     bool           `json:"has_any_open_pr"`
	HasMergedPR      bool           `json:"has_merged_pr"`

	// Workbench metadata
	ManualOverride bool `json:"manual_override,omitempty"`
	ForceKeep      bool `json:"force_keep,omitempty"`
}

type Viewer struct {
	Login string `json:"login"`
	Role  string `json:"role"`
}

type Permissions struct {
	CanTriage bool `json:"can_triage"`
	CanSync   bool `json:"can_sync"`
}

type SessionInfo struct {
	Viewer      Viewer      `json:"viewer"`
	Permissions Permissions `json:"permissions"`
}

type FailedCheck struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type PRStatus struct {
	Repo               string        `json:"repo"`
	Num                int           `json:"num"`
	Label              string        `json:"label"`
	URL                string        `json:"url"`
	Title              string        `json:"title"`
	Author             string        `json:"author"`
	State              string        `json:"state"`
	IsDraft            bool          `json:"is_draft"`
	IsCherryPick       bool          `json:"is_cherry_pick"`
	Mergeable          string        `json:"mergeable"`
	SizeLabel          string        `json:"size_label,omitempty"`
	CreatedAt          string        `json:"created_at,omitempty"`
	UpdatedAt          string        `json:"updated_at"`
	UpdatedDisplay     string        `json:"updated_display"`
	LastCommitAt       string        `json:"last_commit_at,omitempty"`
	WaitingSince       string        `json:"waiting_since,omitempty"`
	WaitingDays        int           `json:"waiting_days"`
	BlockerStage       string        `json:"blocker_stage"`
	BallOwner          string        `json:"ball_owner"`
	SubReason          string        `json:"sub_reason,omitempty"`
	LGTMBy             []string      `json:"lgtm_by,omitempty"`
	ApprovedBy         []string      `json:"approved_by,omitempty"`
	ReviewedBy         []string      `json:"reviewed_by,omitempty"`
	ChangesRequestedBy []string      `json:"changes_requested_by,omitempty"`
	UnresolvedThreads  int           `json:"unresolved_threads,omitempty"`
	UnresolvedBy       []string      `json:"unresolved_by,omitempty"`
	RequestedReviewers []string      `json:"requested_reviewers,omitempty"`
	GateLabels         []string      `json:"gate_labels,omitempty"`
	FailedChecks       []FailedCheck `json:"failed_checks,omitempty"`
	RunningChecksCount int           `json:"running_checks_count,omitempty"`
	PassedChecksCount  int           `json:"passed_checks_count,omitempty"`
	TrackSources       []string      `json:"track_sources,omitempty"`
}

type SyncMeta struct {
	SchemaVersion                int            `json:"schema_version"`
	SupportedLocales             []string       `json:"supported_locales"`
	SnapshotUTC                  string         `json:"snapshot_utc"`
	SnapshotUTC8                 string         `json:"snapshot_utc8"`
	MaxIssueUpdatedUTC           string         `json:"max_issue_updated_utc"`
	MaxIssueUpdatedUTC8          string         `json:"max_issue_updated_utc8"`
	IncrementalGHSearchQualifier string         `json:"incremental_gh_search_qualifier"`
	TotalCandidates              int            `json:"total_candidates"`
	CanonicalCandidates          int            `json:"canonical_candidates"`
	RealBugs                     int            `json:"real_bugs"`
	PRSnapshotUTC                string         `json:"pr_snapshot_utc,omitempty"`
	PRSnapshotUTC8               string         `json:"pr_snapshot_utc8,omitempty"`
	OpenPRCount                  int            `json:"open_pr_count,omitempty"`
	MainlineOpenPRCount          int            `json:"mainline_open_pr_count,omitempty"`
	DeltaSincePrevSnapshot       map[string]any `json:"delta_since_prev_snapshot,omitempty"`
	PendingCandidateNums         []int          `json:"pending_candidate_nums,omitempty"`
	ExcludedIssueNums            []int          `json:"excluded_issue_nums,omitempty"`
}

type DatasetEnums struct {
	Categories    []string `json:"categories"`
	Severities    []string `json:"severities"`
	Subsystems    []string `json:"subsystems"`
	BlockerStages []string `json:"blocker_stages,omitempty"`
	BallOwners    []string `json:"ball_owners,omitempty"`
}

type Dataset struct {
	SyncMeta SyncMeta            `json:"sync_meta"`
	Enums    DatasetEnums        `json:"enums"`
	Records  []IssueRecord       `json:"records"`
	PRStatus map[string]PRStatus `json:"pr_status,omitempty"`
}

type PendingComment struct {
	Author    string `json:"author"`
	CreatedAt string `json:"created_at"`
	Body      string `json:"body"`
}

type PendingCandidate struct {
	Number         int              `json:"number"`
	URL            string           `json:"url"`
	Title          string           `json:"title"`
	Author         string           `json:"author"`
	CreatedAt      string           `json:"created_at"`
	UpdatedAt      string           `json:"updated_at"`
	Labels         []string         `json:"labels"`
	MatchedScopes  []string         `json:"matched_scopes"`
	PRs            []LinkedPRRef    `json:"prs"`
	Body           string           `json:"body"`
	RecentComments []PendingComment `json:"recent_comments"`
}

type TriagePatchRequest struct {
	Cat              string         `json:"cat"`
	Severity         string         `json:"severity"`
	Subsystem        string         `json:"subsystem"`
	IsBorderlineKept bool           `json:"is_borderline_kept"`
	ForceKeep        bool           `json:"force_keep"`
	Note             *LocalizedNote `json:"note"`
}

type PendingResolveRequest struct {
	Action           string         `json:"action"` // "accept" or "exclude"
	Cat              string         `json:"cat,omitempty"`
	Severity         string         `json:"severity,omitempty"`
	Subsystem        string         `json:"subsystem,omitempty"`
	IsBorderlineKept bool           `json:"is_borderline_kept,omitempty"`
	ForceKeep        bool           `json:"force_keep,omitempty"`
	Note             *LocalizedNote `json:"note,omitempty"`
	ExcludeReason    string         `json:"exclude_reason,omitempty"`
}

func ValidateTriageFields(cat, sev, subsys string) error {
	if _, ok := DiffRank[cat]; !ok {
		return fmt.Errorf("invalid category %q", cat)
	}
	if _, ok := SevRank[sev]; !ok {
		return fmt.Errorf("invalid severity %q", sev)
	}
	validSub := false
	for _, s := range ValidSubsystems {
		if s == subsys {
			validSub = true
			break
		}
	}
	if !validSub {
		return fmt.Errorf("invalid subsystem %q", subsys)
	}
	return nil
}
