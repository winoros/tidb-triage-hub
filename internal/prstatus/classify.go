package prstatus

import (
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

var (
	cherryPickTitleRe = regexp.MustCompile(`\(#\d+\)\s*(\|.*)?$`)
	slashLGTMRe       = regexp.MustCompile(`(?im)^\s*/(lgtm|approve)\b`)
)

var knownBotLogins = map[string]bool{
	"coderabbitai":                  true,
	"ti-chi-bot":                    true,
	"tidb-ci-bot":                   true,
	"sre-bot":                       true,
	"prow":                          true,
	"github-actions":                true,
	"codecov":                       true,
	"codecov-commenter":             true,
	"claassistant":                  true,
	"dependabot":                    true,
	"copilot":                       true,
	"copilot-pull-request-reviewer": true,
	"gemini-code-assist":            true,
	"renovate":                      true,
	"ghost":                         true,
	"mergify":                       true,
}

func IsBotLogin(login string) bool {
	l := strings.ToLower(strings.TrimSpace(login))
	if l == "" {
		return true
	}
	if knownBotLogins[l] {
		return true
	}
	if strings.HasSuffix(l, "[bot]") || strings.HasSuffix(l, "-bot") || strings.HasPrefix(l, "ti-srebot") {
		return true
	}
	return false
}

func isIgnoredInformationalCheck(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" || lower == "tide" {
		return true
	}
	if lower == "coderabbit" || strings.HasPrefix(lower, "codecov/") || strings.HasPrefix(lower, "sonarcloud") {
		return true
	}
	return false
}

func isGateStatusContext(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	return lower == "license/cla" || lower == "check-issue-triage-complete"
}

func shortenCheckName(name string) string {
	s := strings.TrimSpace(name)
	s = strings.TrimPrefix(s, "idc-jenkins-ci-tidb/")
	s = strings.TrimPrefix(s, "idc-jenkins-ci-")
	return s
}

type RawReview struct {
	Author      string
	State       string
	SubmittedAt string
}

type RawThreadComment struct {
	Author    string
	CreatedAt string
}

type RawReviewThread struct {
	IsResolved bool
	IsOutdated bool
	Comments   []RawThreadComment
}

type RawComment struct {
	Author    string
	Body      string
	CreatedAt string
}

type RawCheckItem struct {
	Typename   string // "CheckRun" or "StatusContext"
	Name       string
	Status     string // CheckRun: COMPLETED, IN_PROGRESS, QUEUED; StatusContext: SUCCESS, PENDING, FAILURE, ERROR
	Conclusion string // CheckRun conclusion
	URL        string
}

type RawPRInput struct {
	LabelsComplete     bool
	HeadOID            string
	BaseOID            string
	Repo               string
	Number             int
	Title              string
	URL                string
	Author             string
	BaseRefName        string
	State              string // OPEN, CLOSED, MERGED
	IsDraft            bool
	Mergeable          string // MERGEABLE, CONFLICTING, UNKNOWN
	CreatedAt          string
	UpdatedAt          string
	MergedAt           string
	LastCommitAt       string
	Labels             []string
	RequestedReviewers []string
	OpinionatedReviews []RawReview
	RecentReviews      []RawReview
	ReviewThreads      []RawReviewThread
	RecentComments     []RawComment
	Checks             []RawCheckItem
	TrackSources       []string
}

func IsCherryPickTitle(title string, labels []string) bool {
	lower := strings.ToLower(title)
	if cherryPickTitleRe.MatchString(title) ||
		strings.HasPrefix(lower, "[cherry-pick]") ||
		strings.Contains(lower, "automated cherry pick") {
		return true
	}
	for _, lb := range labels {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(lb)), "type/cherry-pick") {
			return true
		}
	}
	return false
}

// IsCherryPickPR unifies cherry-pick / release-branch backport detection across prstatus, issuesync, and triage.
func IsCherryPickPR(title string, labels []string, baseRefName string) bool {
	if IsCherryPickTitle(title, labels) {
		return true
	}
	base := strings.ToLower(strings.TrimSpace(baseRefName))
	return strings.HasPrefix(base, "release-")
}

func FormatPRLabel(repo string, num int) string {
	if repo == "pingcap/tidb" || repo == "" {
		return "#" + itoa(num)
	}
	parts := strings.Split(repo, "/")
	short := parts[len(parts)-1]
	return short + "#" + itoa(num)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func ToUTC8Display(isoUTC string) string {
	if isoUTC == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, isoUTC)
	if err != nil {
		t, err = time.Parse(time.RFC3339, isoUTC)
		if err != nil {
			return isoUTC
		}
	}
	utc8 := time.FixedZone("UTC+8", 8*3600)
	return t.In(utc8).Format("2006-01-02 15:04")
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func ClassifyPR(in RawPRInput, now time.Time) model.PRStatus {
	authorLower := strings.ToLower(strings.TrimSpace(in.Author))
	isHumanReviewer := func(login string) bool {
		if IsBotLogin(login) {
			return false
		}
		return strings.ToLower(strings.TrimSpace(login)) != authorLower
	}

	isCP := IsCherryPickPR(in.Title, in.Labels, in.BaseRefName)
	state := in.State
	if state == "OPEN" && in.IsDraft {
		state = "DRAFT"
	}

	var (
		hasLGTMLabel     bool
		hasApprovedLabel bool
		needsOKToTest    bool
		sizeLabel        string
		holdLabels       []string
		gateLabels       []string
	)
	for _, lb := range in.Labels {
		lower := strings.ToLower(lb)
		switch {
		case lower == "lgtm" || lower == "status/lgt1" || lower == "status/lgt2":
			hasLGTMLabel = true
		case lower == "approved":
			hasApprovedLabel = true
		case lower == "needs-ok-to-test":
			needsOKToTest = true
		case strings.HasPrefix(lower, "size/"):
			sizeLabel = lb
		case lower == "do-not-merge/hold" || lower == "do-not-merge/work-in-progress" || lower == "status/wip":
			holdLabels = append(holdLabels, lb)
		case strings.HasPrefix(lower, "do-not-merge/") || lower == "needs-rebase":
			gateLabels = append(gateLabels, lb)
		}
	}

	var (
		lgtmBy                   []string
		approvedBy               []string
		reviewedBy               []string
		activeChangesRequestedBy []string
		lastReviewerActivityAt   string
	)
	bumpReviewerActivity := func(ts string) {
		if ts != "" && model.CompareRFC3339(ts, lastReviewerActivityAt) > 0 {
			lastReviewerActivityAt = ts
		}
	}

	for _, rv := range in.OpinionatedReviews {
		if !isHumanReviewer(rv.Author) {
			continue
		}
		reviewedBy = append(reviewedBy, rv.Author)
		bumpReviewerActivity(rv.SubmittedAt)
		switch rv.State {
		case "APPROVED":
			lgtmBy = append(lgtmBy, rv.Author)
			approvedBy = append(approvedBy, rv.Author)
		case "CHANGES_REQUESTED":
			if in.LastCommitAt == "" || rv.SubmittedAt == "" || model.CompareRFC3339(rv.SubmittedAt, in.LastCommitAt) >= 0 {
				activeChangesRequestedBy = append(activeChangesRequestedBy, rv.Author)
			}
		}
	}

	for _, rv := range in.RecentReviews {
		if !isHumanReviewer(rv.Author) {
			continue
		}
		reviewedBy = append(reviewedBy, rv.Author)
		bumpReviewerActivity(rv.SubmittedAt)
	}

	for _, cmt := range in.RecentComments {
		if !isHumanReviewer(cmt.Author) {
			continue
		}
		if m := slashLGTMRe.FindStringSubmatch(cmt.Body); len(m) >= 2 {
			reviewedBy = append(reviewedBy, cmt.Author)
			lgtmBy = append(lgtmBy, cmt.Author)
			if strings.EqualFold(m[1], "approve") {
				approvedBy = append(approvedBy, cmt.Author)
			}
			bumpReviewerActivity(cmt.CreatedAt)
		}
	}

	var (
		unresolvedThreads      int
		unresolvedBy           []string
		hasFreshReviewerThread bool
	)
	for _, th := range in.ReviewThreads {
		if th.IsResolved || th.IsOutdated {
			continue
		}
		unresolvedThreads++
		if len(th.Comments) > 0 {
			last := th.Comments[len(th.Comments)-1]
			if isHumanReviewer(last.Author) {
				unresolvedBy = append(unresolvedBy, last.Author)
				bumpReviewerActivity(last.CreatedAt)
				if in.LastCommitAt == "" || last.CreatedAt == "" || model.CompareRFC3339(last.CreatedAt, in.LastCommitAt) >= 0 {
					hasFreshReviewerThread = true
				}
			}
		}
	}

	var (
		hasTideContext     bool
		failedChecks       []model.FailedCheck
		runningChecksCount int
		passedChecksCount  int
	)
	seenCheck := make(map[string]bool)
	for _, ck := range in.Checks {
		if strings.EqualFold(ck.Name, "tide") {
			hasTideContext = true
			continue
		}
		if isGateStatusContext(ck.Name) {
			st := strings.ToUpper(ck.Status)
			if strings.EqualFold(ck.Name, "license/cla") && st != "SUCCESS" {
				gateLabels = append(gateLabels, "license/cla")
			} else if strings.EqualFold(ck.Name, "check-issue-triage-complete") && (st == "FAILURE" || st == "ERROR") {
				gateLabels = append(gateLabels, "check-issue-triage-complete")
			}
			continue
		}
		if isIgnoredInformationalCheck(ck.Name) {
			continue
		}
		dedupKey := ck.Typename + ":" + ck.Name
		if seenCheck[dedupKey] {
			continue
		}
		seenCheck[dedupKey] = true

		if ck.Typename == "CheckRun" {
			st := strings.ToUpper(ck.Status)
			conc := strings.ToUpper(ck.Conclusion)
			if st != "COMPLETED" {
				runningChecksCount++
			} else if conc == "FAILURE" || conc == "TIMED_OUT" || conc == "ACTION_REQUIRED" || conc == "STARTUP_FAILURE" || conc == "CANCELLED" {
				failedChecks = append(failedChecks, model.FailedCheck{
					Name: shortenCheckName(ck.Name),
					URL:  ck.URL,
				})
			} else {
				passedChecksCount++
			}
		} else {
			st := strings.ToUpper(ck.Status)
			if st == "PENDING" || st == "EXPECTED" {
				runningChecksCount++
			} else if st == "FAILURE" || st == "ERROR" {
				failedChecks = append(failedChecks, model.FailedCheck{
					Name: shortenCheckName(ck.Name),
					URL:  ck.URL,
				})
			} else if st == "SUCCESS" {
				passedChecksCount++
			}
		}
	}

	sort.Slice(failedChecks, func(i, j int) bool {
		return failedChecks[i].Name < failedChecks[j].Name
	})

	reqReviewers := make([]string, 0, len(in.RequestedReviewers))
	for _, r := range in.RequestedReviewers {
		if !IsBotLogin(r) && !strings.EqualFold(r, in.Author) {
			reqReviewers = append(reqReviewers, r)
		}
	}

	lgtmBy = uniqueSorted(lgtmBy)
	approvedBy = uniqueSorted(approvedBy)
	reviewedBy = uniqueSorted(reviewedBy)
	activeChangesRequestedBy = uniqueSorted(activeChangesRequestedBy)
	unresolvedBy = uniqueSorted(unresolvedBy)
	reqReviewers = uniqueSorted(reqReviewers)
	holdLabels = uniqueSorted(holdLabels)
	gateLabels = uniqueSorted(gateLabels)

	fullyApproved := hasApprovedLabel && (hasLGTMLabel || !hasTideContext)
	if !hasTideContext && !hasApprovedLabel && len(approvedBy) > 0 && len(activeChangesRequestedBy) == 0 {
		fullyApproved = true
	}

	var (
		stage     string
		ballOwner string
		subReason string
	)

	switch {
	case in.IsDraft:
		stage = model.StageDraftOrHold
		ballOwner = model.BallHold
		subReason = "draft"
	case len(holdLabels) > 0:
		stage = model.StageDraftOrHold
		ballOwner = model.BallHold
		subReason = "hold_label"
		gateLabels = uniqueSorted(append(gateLabels, holdLabels...))
	case strings.EqualFold(in.Mergeable, "CONFLICTING"):
		stage = model.StageConflict
		ballOwner = model.BallAuthor
		subReason = "merge_conflict"
	case needsOKToTest:
		stage = model.StageNeedsOKToTest
		ballOwner = model.BallReviewer
		subReason = "needs_ok_to_test"
	case len(failedChecks) > 0:
		stage = model.StageCIFailed
		ballOwner = model.BallAuthor
		subReason = "ci_failed"
	case len(activeChangesRequestedBy) > 0:
		stage = model.StageWaitingAuthor
		ballOwner = model.BallAuthor
		subReason = "changes_requested"
	case hasFreshReviewerThread:
		stage = model.StageWaitingAuthor
		ballOwner = model.BallAuthor
		subReason = "unresolved_thread"
	case (hasLGTMLabel || hasApprovedLabel || len(approvedBy) > 0) && len(gateLabels) > 0:
		stage = model.StageGateBlocked
		ballOwner = model.BallAuthor
		subReason = "gate_label"
	case (hasLGTMLabel && !hasApprovedLabel) || (hasTideContext && !hasLGTMLabel && !hasApprovedLabel && len(lgtmBy) == 1):
		stage = model.StageNeedsLGTM2
		ballOwner = model.BallReviewer
		subReason = "needs_second_lgtm"
	case !fullyApproved:
		stage = model.StageWaitingReview
		ballOwner = model.BallReviewer
		if len(reviewedBy) == 0 && unresolvedThreads == 0 {
			subReason = "zero_review"
		} else if in.LastCommitAt != "" && lastReviewerActivityAt != "" && model.CompareRFC3339(in.LastCommitAt, lastReviewerActivityAt) > 0 {
			subReason = "re_review"
		} else {
			subReason = "awaiting_review"
		}
	default:
		stage = model.StageReadyMerging
		ballOwner = model.BallMergeQueue
		if runningChecksCount > 0 {
			subReason = "ci_running"
		} else {
			subReason = "merge_queue"
		}
	}

	waitingSince := in.CreatedAt
	switch ballOwner {
	case model.BallAuthor:
		if lastReviewerActivityAt != "" {
			waitingSince = lastReviewerActivityAt
		} else if in.UpdatedAt != "" {
			waitingSince = in.UpdatedAt
		}
	case model.BallReviewer:
		if in.LastCommitAt != "" && model.CompareRFC3339(in.LastCommitAt, waitingSince) > 0 {
			waitingSince = in.LastCommitAt
		}
	default:
		if in.UpdatedAt != "" {
			waitingSince = in.UpdatedAt
		}
	}

	waitingDays := 0
	if waitingSince != "" {
		if wt, err := time.Parse(time.RFC3339Nano, waitingSince); err == nil && now.After(wt) {
			waitingDays = int(now.Sub(wt).Hours() / 24)
		} else if wt, err := time.Parse(time.RFC3339, waitingSince); err == nil && now.After(wt) {
			waitingDays = int(now.Sub(wt).Hours() / 24)
		}
	}

	sources := in.TrackSources
	if len(sources) == 0 {
		sources = []string{"issue_linked"}
	}

	return model.PRStatus{
		HeadOID: in.HeadOID, BaseOID: in.BaseOID,
		Repo:               in.Repo,
		Num:                in.Number,
		Label:              FormatPRLabel(in.Repo, in.Number),
		URL:                in.URL,
		Title:              in.Title,
		Author:             in.Author,
		State:              state,
		IsDraft:            in.IsDraft,
		IsCherryPick:       isCP,
		CherryPickKnown:    isCP || in.LabelsComplete,
		Mergeable:          in.Mergeable,
		SizeLabel:          sizeLabel,
		CreatedAt:          in.CreatedAt,
		UpdatedAt:          in.UpdatedAt,
		MergedAt:           in.MergedAt,
		UpdatedDisplay:     ToUTC8Display(in.UpdatedAt),
		LastCommitAt:       in.LastCommitAt,
		WaitingSince:       waitingSince,
		WaitingDays:        waitingDays,
		BlockerStage:       stage,
		BallOwner:          ballOwner,
		SubReason:          subReason,
		LGTMBy:             lgtmBy,
		ApprovedBy:         approvedBy,
		ReviewedBy:         reviewedBy,
		ChangesRequestedBy: activeChangesRequestedBy,
		UnresolvedThreads:  unresolvedThreads,
		UnresolvedBy:       unresolvedBy,
		RequestedReviewers: reqReviewers,
		GateLabels:         gateLabels,
		FailedChecks:       failedChecks,
		RunningChecksCount: runningChecksCount,
		PassedChecksCount:  passedChecksCount,
		TrackSources:       sources,
	}
}
