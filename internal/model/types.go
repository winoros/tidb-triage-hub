package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

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

const (
	LinkSourceManual   = "manual"
	LinkSourcePRRef    = "pr_ref"
	LinkSourceTimeline = "timeline"

	PRStateUnknown = "UNKNOWN"
)

const (
	RetriageNewFixPR            = "new_fix_pr"
	RetriageFixPRMergedOpen     = "fix_pr_merged_still_open"
	RetriageCustomerReported    = "customer_reported"
	RetriageGHSeverityEscalated = "gh_severity_escalated"
	RetriageLabelScopeChanged   = "label_scope_changed"
	RetriageSubstantiveComments = "substantive_new_comments"
)

type RetriageTrigger struct {
	Code      string `json:"code"`
	Summary   string `json:"summary"`
	Actor     string `json:"actor,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

type LocalizedNote struct {
	ZhCN string `json:"zh-CN"`
	En   string `json:"en"`
}

func (n *LocalizedNote) IsEmpty() bool {
	return n == nil || (n.ZhCN == "" && n.En == "")
}

type LinkedPRRef struct {
	Num             int    `json:"num"`
	Repo            string `json:"repo"`
	Label           string `json:"label"`
	URL             string `json:"url"`
	State           string `json:"state"`
	Title           string `json:"title"`
	IsCherryPick    bool   `json:"is_cherry_pick"`
	UpdatedAt       string `json:"updated_at"`
	MergedAt        string `json:"merged_at,omitempty"`
	AckAt           string `json:"-"`
	Source          string `json:"source,omitempty"`
	HeadOID         string `json:"head_oid,omitempty"`
	BaseOID         string `json:"base_oid,omitempty"`
	CherryPickKnown bool   `json:"cherry_pick_known,omitempty"`
}

func PRKey(repo string, num int) string {
	return fmt.Sprintf("%s#%d", repo, num)
}

// ReviewVersion identifies the facts/classification and notes read by a reviewer.
// The counters are independent and never reset when an issue leaves or re-enters a queue.
// They are opaque to clients: echo the version from the reviewed snapshot unchanged.
type ReviewVersion struct {
	Review int64 `json:"review"`
	Notes  int64 `json:"notes"`
}

type ReviewPrecondition struct {
	ExpectedVersion   *ReviewVersion `json:"expected_version,omitempty"`
	IfUnmodifiedSince string         `json:"if_unmodified_since,omitempty"`
}

type IssueRecord struct {
	Version          *ReviewVersion `json:"version,omitempty"`
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

	// Workbench & dynamic re-triage metadata
	ManualOverride  bool              `json:"manual_override,omitempty"`
	ForceKeep       bool              `json:"force_keep,omitempty"`
	NeedsRetriage   bool              `json:"needs_retriage,omitempty"`
	RetriageReasons []string          `json:"retriage_reasons,omitempty"`
	RetriageDetails []RetriageTrigger `json:"retriage_details,omitempty"`
	TriagedAt       string            `json:"triaged_at,omitempty"`
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
	Viewer        Viewer      `json:"viewer"`
	Permissions   Permissions `json:"permissions"`
	AuthRequired  bool        `json:"auth_required,omitempty"`
	Authenticated bool        `json:"authenticated,omitempty"`
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
	MergedAt           string        `json:"merged_at,omitempty"`
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
	HeadOID            string        `json:"head_oid,omitempty"`
	BaseOID            string        `json:"base_oid,omitempty"`
	CherryPickKnown    bool          `json:"cherry_pick_known,omitempty"`
}

type DeltaClosedIssue struct {
	Number       int    `json:"number"`
	PrevCat      string `json:"prev_cat"`
	PrevSeverity string `json:"prev_severity"`
	ClosedAt     string `json:"closed_at,omitempty"`
	Reason       string `json:"reason"`
}

type DeltaGraduatedIssue struct {
	Number        int      `json:"number"`
	PrevCat       string   `json:"prev_cat"`
	Reason        string   `json:"reason"`
	CurrentLabels []string `json:"-"`
}

type DeltaAddedIssue struct {
	Number    int    `json:"number"`
	Cat       string `json:"cat"`
	Severity  string `json:"severity"`
	Subsystem string `json:"subsystem"`
	Reason    string `json:"reason"`
}

type SnapshotDelta struct {
	PrevSnapshotDate          string                `json:"prev_snapshot_date"`
	ClosedIssuesRemoved       []DeltaClosedIssue    `json:"closed_issues_removed"`
	RelabeledGraduatedRemoved []DeltaGraduatedIssue `json:"relabeled_graduated_removed"`
	NewOpenCandidatesAdded    []DeltaAddedIssue     `json:"new_open_candidates_added"`
}

func (d *SnapshotDelta) HasChanges() bool {
	if d == nil {
		return false
	}
	return len(d.ClosedIssuesRemoved) > 0 || len(d.RelabeledGraduatedRemoved) > 0 || len(d.NewOpenCandidatesAdded) > 0
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
	DeltaSincePrevSnapshot       *SnapshotDelta `json:"delta_since_prev_snapshot,omitempty"`
	PendingCandidateNums         []int          `json:"pending_candidate_nums,omitempty"`
	RetriageIssueNums            []int          `json:"retriage_issue_nums,omitempty"`
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

type PRFileStat struct {
	Path      string `json:"path"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
}

type PendingPRDetail struct {
	Repo              string       `json:"repo"`
	Num               int          `json:"num"`
	Title             string       `json:"title"`
	State             string       `json:"state"`
	UpdatedAt         string       `json:"updated_at,omitempty"`
	MergedAt          string       `json:"merged_at,omitempty"`
	Author            string       `json:"author,omitempty"`
	BaseRefName       string       `json:"base_ref_name,omitempty"`
	IsCherryPick      bool         `json:"is_cherry_pick,omitempty"`
	Additions         int          `json:"additions"`
	Deletions         int          `json:"deletions"`
	ChangedFilesCount int          `json:"changed_files_count"`
	FilesTruncated    bool         `json:"files_truncated,omitempty"`
	NonTestFileCount  int          `json:"non_test_file_count"`
	NonTestAdditions  int          `json:"non_test_additions"`
	NonTestDeletions  int          `json:"non_test_deletions"`
	NonTestFiles      []string     `json:"non_test_files,omitempty"`
	TestFileCount     int          `json:"test_file_count"`
	ChangedFiles      []PRFileStat `json:"changed_files,omitempty"`
	BodyExcerpt       string       `json:"body_excerpt,omitempty"`
	HeadOID           string       `json:"head_oid,omitempty"`
	BaseOID           string       `json:"base_oid,omitempty"`
	CherryPickKnown   bool         `json:"cherry_pick_known,omitempty"`
}

type PendingClues struct {
	CodePaths       []string `json:"code_paths,omitempty"`
	TouchedPackages []string `json:"touched_packages,omitempty"`
	Functions       []string `json:"functions,omitempty"`
	ErrorSignatures []string `json:"error_signatures,omitempty"`
	SQLFeatures     []string `json:"sql_features,omitempty"`
	OriginSignals   []string `json:"origin_signals,omitempty"`
}

type SuggestedTriage struct {
	RecommendedAction string   `json:"recommended_action"` // "accept" | "exclude" | "review_carefully"
	ExcludeReason     string   `json:"exclude_reason,omitempty"`
	Cat               string   `json:"cat"`
	Severity          string   `json:"severity"`
	Subsystem         string   `json:"subsystem"`
	IsBorderlineKept  bool     `json:"is_borderline_kept"`
	Confidence        string   `json:"confidence"` // "high" | "medium" | "low"
	Signals           []string `json:"signals"`
}

type PendingCandidate struct {
	Version         *ReviewVersion    `json:"version,omitempty"`
	Revision        int               `json:"-"`
	SyncWatermark   string            `json:"sync_watermark,omitempty"`
	Number          int               `json:"number"`
	URL             string            `json:"url"`
	Title           string            `json:"title"`
	Author          string            `json:"author"`
	Assignees       []string          `json:"assignees,omitempty"`
	CreatedAt       string            `json:"created_at"`
	UpdatedAt       string            `json:"updated_at"`
	Labels          []string          `json:"labels"`
	MatchedScopes   []string          `json:"matched_scopes"`
	PRs             []LinkedPRRef     `json:"prs"`
	PRDetails       []PendingPRDetail `json:"pr_details,omitempty"`
	Clues           *PendingClues     `json:"clues,omitempty"`
	SuggestedTriage *SuggestedTriage  `json:"suggested_triage,omitempty"`
	Body            string            `json:"body"`
	RecentComments  []PendingComment  `json:"recent_comments"`
}

type RetriageCandidate struct {
	Version          *ReviewVersion    `json:"version,omitempty"`
	Number           int               `json:"number"`
	URL              string            `json:"url"`
	Title            string            `json:"title"`
	Author           string            `json:"author"`
	IsCanonical      bool              `json:"is_canonical"`
	CurrentCat       string            `json:"current_cat"`
	CurrentSeverity  string            `json:"current_severity"`
	CurrentSubsys    string            `json:"current_subsystem"`
	IsBorderlineKept bool              `json:"is_borderline_kept,omitempty"`
	ForceKeep        bool              `json:"force_keep,omitempty"`
	CurrentNote      *LocalizedNote    `json:"current_note,omitempty"`
	Labels           []string          `json:"labels"`
	PRs              []LinkedPRRef     `json:"prs"`
	PRDetails        []PendingPRDetail `json:"pr_details,omitempty"`
	Clues            *PendingClues     `json:"clues,omitempty"`
	SuggestedTriage  *SuggestedTriage  `json:"suggested_triage,omitempty"`
	TriagedAt        string            `json:"triaged_at"`
	Reasons          []string          `json:"retriage_reasons"`
	Details          []RetriageTrigger `json:"retriage_details"`
	Body             string            `json:"body,omitempty"`
	RecentComments   []PendingComment  `json:"recent_comments,omitempty"`
}

type ExcludedIssueRecord struct {
	Version        *ReviewVersion `json:"version,omitempty"`
	Number         int            `json:"number"`
	Title          string         `json:"title,omitempty"`
	Reason         string         `json:"reason"`
	ExcludedLabels []string       `json:"excluded_labels,omitempty"`
	Source         string         `json:"source,omitempty"`
	ExcludedAt     string         `json:"excluded_at"`
}

// FlexIntList unmarshals JSON arrays containing either integers ([73063]) or issue strings (["73063", "#73064"]).
type FlexIntList []int

func (f *FlexIntList) UnmarshalJSON(data []byte) error {
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	out := make([]int, 0, len(raw))
	for _, elem := range raw {
		trimmed := bytes.TrimSpace(elem)
		if len(trimmed) == 0 {
			return fmt.Errorf("empty issue number element")
		}
		if trimmed[0] == '"' {
			var s string
			if err := json.Unmarshal(trimmed, &s); err != nil {
				return fmt.Errorf("invalid issue number %s: %w", string(trimmed), err)
			}
			n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(s), "#"))
			if err != nil || n <= 0 {
				return fmt.Errorf("invalid issue number %q", s)
			}
			out = append(out, n)
			continue
		}
		n, err := strconv.Atoi(string(trimmed))
		if err != nil || n <= 0 {
			return fmt.Errorf("invalid issue number %s", string(trimmed))
		}
		out = append(out, n)
	}
	*f = out
	return nil
}

type TriageConflict struct {
	Issue   int    `json:"issue"`
	Action  string `json:"action"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

type NotePatchRequest struct {
	ZhCN              string         `json:"zh-CN"`
	En                string         `json:"en"`
	ExpectedVersion   *ReviewVersion `json:"expected_version,omitempty"`
	IfUnmodifiedSince string         `json:"if_unmodified_since,omitempty"`
}

func (n NotePatchRequest) LocalizedNote() *LocalizedNote {
	return &LocalizedNote{ZhCN: n.ZhCN, En: n.En}
}

func (n *NotePatchRequest) UnmarshalJSON(data []byte) error {
	var aux struct {
		ZhCN              string         `json:"zh-CN"`
		En                string         `json:"en"`
		Note              *LocalizedNote `json:"note"`
		ExpectedVersion   *ReviewVersion `json:"expected_version,omitempty"`
		IfUnmodifiedSince string         `json:"if_unmodified_since"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	zh, en := aux.ZhCN, aux.En
	if zh == "" && en == "" && aux.Note != nil {
		zh = aux.Note.ZhCN
		en = aux.Note.En
	}
	*n = NotePatchRequest{
		ZhCN:              zh,
		En:                en,
		ExpectedVersion:   aux.ExpectedVersion,
		IfUnmodifiedSince: aux.IfUnmodifiedSince,
	}
	return nil
}

type BatchTriageDecisions struct {
	// ExpectedVersions covers every selected action, including ack/exclude/manual links.
	// A non-nil map opts into the version protocol and requires an entry for each action.
	ExpectedVersions map[string]ReviewVersion      `json:"expected_versions"`
	BaseExportedAt   string                        `json:"base_exported_at,omitempty"`
	NewIssues        map[string]TriagePatchRequest `json:"new_issues,omitempty"`
	ExcludeIssues    map[string]string             `json:"exclude_issues,omitempty"`
	Overrides        map[string]TriagePatchRequest `json:"overrides,omitempty"`
	Notes            map[string]NotePatchRequest   `json:"notes,omitempty"`
	AckRetriage      FlexIntList                   `json:"ack_retriage,omitempty"`
	ManualPRLinks    map[string][]string           `json:"manual_pr_links,omitempty"`
}

type TriagePatchRequest struct {
	Cat               string         `json:"cat"`
	Severity          string         `json:"severity"`
	Subsystem         string         `json:"subsystem"`
	IsBorderlineKept  bool           `json:"is_borderline_kept"`
	ForceKeep         bool           `json:"force_keep"`
	Note              *LocalizedNote `json:"note"`
	ExpectedVersion   *ReviewVersion `json:"expected_version,omitempty"`
	IfUnmodifiedSince string         `json:"if_unmodified_since,omitempty"`

	unmarshaled       bool `json:"-"`
	hasBorderlineKept bool `json:"-"`
	hasForceKeep      bool `json:"-"`
}

func (r *TriagePatchRequest) UnmarshalJSON(data []byte) error {
	var aux struct {
		Cat               string         `json:"cat"`
		Severity          string         `json:"severity"`
		Subsystem         string         `json:"subsystem"`
		IsBorderlineKept  *bool          `json:"is_borderline_kept"`
		ForceKeep         *bool          `json:"force_keep"`
		Note              *LocalizedNote `json:"note"`
		ExpectedVersion   *ReviewVersion `json:"expected_version,omitempty"`
		IfUnmodifiedSince string         `json:"if_unmodified_since"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = TriagePatchRequest{
		Cat:               aux.Cat,
		Severity:          aux.Severity,
		Subsystem:         aux.Subsystem,
		Note:              aux.Note,
		ExpectedVersion:   aux.ExpectedVersion,
		IfUnmodifiedSince: aux.IfUnmodifiedSince,
		unmarshaled:       true,
	}
	if aux.IsBorderlineKept != nil {
		r.IsBorderlineKept = *aux.IsBorderlineKept
		r.hasBorderlineKept = true
	}
	if aux.ForceKeep != nil {
		r.ForceKeep = *aux.ForceKeep
		r.hasForceKeep = true
	}
	return nil
}

func (r TriagePatchRequest) MarshalJSON() ([]byte, error) {
	if !r.unmarshaled || (r.hasBorderlineKept && r.hasForceKeep) {
		type alias TriagePatchRequest
		return json.Marshal(alias(r))
	}
	var bdrPtr *bool
	if r.hasBorderlineKept {
		v := r.IsBorderlineKept
		bdrPtr = &v
	}
	var fkPtr *bool
	if r.hasForceKeep {
		v := r.ForceKeep
		fkPtr = &v
	}
	return json.Marshal(struct {
		Cat               string         `json:"cat"`
		Severity          string         `json:"severity"`
		Subsystem         string         `json:"subsystem"`
		IsBorderlineKept  *bool          `json:"is_borderline_kept,omitempty"`
		ForceKeep         *bool          `json:"force_keep,omitempty"`
		Note              *LocalizedNote `json:"note"`
		ExpectedVersion   *ReviewVersion `json:"expected_version,omitempty"`
		IfUnmodifiedSince string         `json:"if_unmodified_since,omitempty"`
	}{
		Cat:               r.Cat,
		Severity:          r.Severity,
		Subsystem:         r.Subsystem,
		IsBorderlineKept:  bdrPtr,
		ForceKeep:         fkPtr,
		Note:              r.Note,
		ExpectedVersion:   r.ExpectedVersion,
		IfUnmodifiedSince: r.IfUnmodifiedSince,
	})
}

// HasBorderlineField reports whether `is_borderline_kept` was explicitly supplied
// (true for in-memory Go struct literals, or when present in JSON payload).
func (r TriagePatchRequest) HasBorderlineField() bool {
	return !r.unmarshaled || r.hasBorderlineKept
}

// HasForceKeepField reports whether `force_keep` was explicitly supplied.
func (r TriagePatchRequest) HasForceKeepField() bool {
	return !r.unmarshaled || r.hasForceKeep
}

// RFC3339NanoFixed formats UTC timestamps with fixed 9-digit nanoseconds so both Go
// chronological comparison and SQLite lexicographic `ORDER BY` remain strictly monotonic.
const RFC3339NanoFixed = "2006-01-02T15:04:05.000000000Z"

// NowRFC3339Nano returns the current UTC time formatted with fixed-width nanosecond precision.
func NowRFC3339Nano() string {
	return time.Now().UTC().Format(RFC3339NanoFixed)
}

func parseFlexibleRFC3339(ts string) (time.Time, bool) {
	s := strings.TrimSpace(ts)
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// NormalizeRFC3339NanoFixed validates an RFC3339/RFC3339Nano timestamp and normalizes it to UTC RFC3339NanoFixed.
func NormalizeRFC3339NanoFixed(ts string) (string, bool) {
	t, ok := parseFlexibleRFC3339(ts)
	if !ok {
		return "", false
	}
	return t.Format(RFC3339NanoFixed), true
}

// ParseBoolDefault parses common boolean query/env literals ("1","true","yes" / "0","false","no").
func ParseBoolDefault(raw string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return def
	}
}

// ParseIssueNumCSV parses a comma-separated list of positive issue numbers (with optional '#' prefixes) and deduplicates them.
func ParseIssueNumCSV(raw string) ([]int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []int
	seen := make(map[int]bool)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimPrefix(strings.TrimSpace(part), "#")
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid issue number %q", part)
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}

// CompareRFC3339 compares two ISO8601/RFC3339 timestamps chronologically (handling subseconds and offsets).
func CompareRFC3339(a, b string) int {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == b {
		return 0
	}
	if a == "" {
		return -1
	}
	if b == "" {
		return 1
	}
	ta, oka := parseFlexibleRFC3339(a)
	tb, okb := parseFlexibleRFC3339(b)
	if oka && okb {
		if ta.Before(tb) {
			return -1
		}
		if ta.After(tb) {
			return 1
		}
		return 0
	}
	if a < b {
		return -1
	}
	return 1
}

// NormalizeRFC3339UTC normalizes a timestamp to canonical UTC RFC3339(Nano) string.
func NormalizeRFC3339UTC(ts string) string {
	if t, ok := parseFlexibleRFC3339(ts); ok {
		if t.Nanosecond() > 0 {
			return t.Format(time.RFC3339Nano)
		}
		return t.Format("2006-01-02T15:04:05Z")
	}
	return strings.TrimSpace(ts)
}

// MaxRFC3339 returns the chronologically latest timestamp among vals, normalized to UTC.
func MaxRFC3339(vals ...string) string {
	best := ""
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if best == "" || CompareRFC3339(v, best) > 0 {
			best = NormalizeRFC3339UTC(v)
		}
	}
	return best
}

type PendingResolveRequest struct {
	Action            string         `json:"action"` // "accept" or "exclude"
	Cat               string         `json:"cat,omitempty"`
	Severity          string         `json:"severity,omitempty"`
	Subsystem         string         `json:"subsystem,omitempty"`
	IsBorderlineKept  bool           `json:"is_borderline_kept,omitempty"`
	ForceKeep         bool           `json:"force_keep,omitempty"`
	Note              *LocalizedNote `json:"note,omitempty"`
	ExcludeReason     string         `json:"exclude_reason,omitempty"`
	ExpectedVersion   *ReviewVersion `json:"expected_version,omitempty"`
	IfUnmodifiedSince string         `json:"if_unmodified_since,omitempty"`
}

func IsRealBugCategory(cat string) bool {
	return cat == "EASY" || cat == "MEDIUM" || cat == "HARD"
}

func NormalizeSeverityForCategory(cat, sev string, defaultBugSev string) string {
	if !IsRealBugCategory(cat) {
		return "N/A"
	}
	if sev == "" || sev == "N/A" {
		if defaultBugSev != "" {
			return defaultBugSev
		}
		return "MODERATE"
	}
	return sev
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
