package issuesync

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

type Options struct {
	Full                  bool
	Since                 string // Optional explicit RFC3339 watermark
	OverlapMinutes        int    // Default 15
	MaxIncrementalDays    int    // Default 14
	UntriagedLookbackDays int    // Default 90
	Now                   time.Time
	Tracker               *ghgql.CostTracker
}

type TrackedPRLink struct {
	Repo         string
	Num          int
	State        string
	IsCherryPick bool
	Source       string
}

type TrackedIssueState struct {
	Number      int
	Title       string
	UpdatedAt   string
	Labels      []string
	Cat         string
	Severity    string
	IsCanonical bool
	ForceKeep   bool
	PRs         []TrackedPRLink
}

type ArchivedIssueState struct {
	Number    int
	Cat       string
	ForceKeep bool
}

type SyncState struct {
	PrevMeta          model.SyncMeta
	TrackedIssues     map[int]TrackedIssueState
	ArchivedIssues    map[int]ArchivedIssueState
	PendingScopes     map[int][]string
	PendingCandidates map[int]model.PendingCandidate
	ExcludedLabels    map[int][]string
	ExcludedIssues    map[int]ExcludedIssueState
	RecheckNums       []int
}

type LiveIssueUpdate struct {
	Number            int
	Title             string
	Author            string
	Assignees         []string
	Created           string
	CreatedAt         string
	UpdatedAt         string
	Labels            []string
	IsCanonical       bool
	IsCustomer        bool
	GHSeverity        string
	TimelinePRs       []model.LinkedPRRef
	TimelineTruncated bool
	HasBody           bool
	Body              string
	HasComments       bool
	RecentComments    []model.PendingComment
}

type SyncAppliedOutcome struct {
	ClosedRemovedNums    []int
	GraduatedRemovedNums []int
	RestoredNums         []int
	PendingQueueNums     []int
}

type SyncPlan struct {
	ModeUsed              string
	PlanStartedUTC        string
	SnapshotUTC           string
	SinceUTC              string
	Stats                 ghgql.CallStats
	UpdatedIssues         []LiveIssueUpdate
	RestoredIssues        []LiveIssueUpdate
	ClosedRemovals        []model.DeltaClosedIssue
	ClosedPendingRemovals []model.DeltaClosedIssue
	GraduatedRemovals     []model.DeltaGraduatedIssue
	PendingLiveUpdates    map[int]LiveIssueUpdate
	StreamBPRRefs         map[int][]model.LinkedPRRef
	UnlinkedStreamBPRs    map[int][]string
	DirectPRUpdates       map[string]model.LinkedPRRef
	UpsertPending         []model.PendingCandidate
	DeletePendingNums     []int
	ReadmittedNums        []int
	ClearedRecheckNums    []int
	PRChangedNums         []int
	MetaChangedNums       []int
	Applied               *SyncAppliedOutcome
}

type Engine struct {
	Runner ghgql.Runner
}

func NewEngine(runner ghgql.Runner) *Engine {
	if runner == nil {
		runner = ghgql.DefaultRunner()
	}
	return &Engine{Runner: runner}
}

type gqlIssueSearchNode struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	StateReason string `json:"stateReason"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	ClosedAt    string `json:"closedAt"`
	Author      *struct {
		Login string `json:"login"`
	} `json:"author"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
}

func (n gqlIssueSearchNode) NodeKey() string {
	if n.Number <= 0 {
		return ""
	}
	return strconv.Itoa(n.Number)
}

func (n gqlIssueSearchNode) NodeUpdatedAt() string {
	return n.UpdatedAt
}

func (n gqlIssueSearchNode) labelNames() []string {
	out := make([]string, 0, len(n.Labels.Nodes))
	for _, l := range n.Labels.Nodes {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

func (n gqlIssueSearchNode) authorLogin() string {
	if n.Author != nil && n.Author.Login != "" {
		return n.Author.Login
	}
	return "ghost"
}

type gqlPRSearchNode struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	URL         string `json:"url"`
	BaseRefName string `json:"baseRefName"`
	State       string `json:"state"`
	IsDraft     bool   `json:"isDraft"`
	UpdatedAt   string `json:"updatedAt"`
	MergedAt    string `json:"mergedAt"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Repo struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
	ClosingRefs struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Number int `json:"number"`
			Repo   struct {
				NameWithOwner string `json:"nameWithOwner"`
			} `json:"repository"`
		} `json:"nodes"`
	} `json:"closingIssuesReferences"`
	Commits struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Commit struct {
				MessageHeadline string `json:"messageHeadline"`
				MessageBody     string `json:"messageBody"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
	Comments struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Body string `json:"body"`
		} `json:"nodes"`
	} `json:"comments"`
}

func (p gqlPRSearchNode) hasCompleteRefScan() bool {
	return p.ClosingRefs.TotalCount <= len(p.ClosingRefs.Nodes) &&
		p.Commits.TotalCount <= len(p.Commits.Nodes) &&
		p.Comments.TotalCount <= len(p.Comments.Nodes)
}

func (p gqlPRSearchNode) NodeKey() string {
	if p.Number <= 0 || p.Repo.NameWithOwner == "" {
		return ""
	}
	return model.PRKey(p.Repo.NameWithOwner, p.Number)
}

func (p gqlPRSearchNode) labelNames() []string {
	if len(p.Labels.Nodes) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Labels.Nodes))
	for _, lb := range p.Labels.Nodes {
		if lb.Name != "" {
			out = append(out, lb.Name)
		}
	}
	return out
}

func (p gqlPRSearchNode) NodeUpdatedAt() string {
	return p.UpdatedAt
}

func (p gqlPRSearchNode) extractReferencedTiDBIssues() []int {
	seen := make(map[int]bool)
	for _, ci := range p.ClosingRefs.Nodes {
		if ci.Repo.NameWithOwner == "pingcap/tidb" && ci.Number > 0 {
			seen[ci.Number] = true
		}
	}
	var sb strings.Builder
	sb.WriteString(p.Title)
	sb.WriteByte('\n')
	sb.WriteString(p.Body)
	for _, cm := range p.Commits.Nodes {
		sb.WriteByte('\n')
		sb.WriteString(cm.Commit.MessageHeadline)
		sb.WriteByte('\n')
		sb.WriteString(cm.Commit.MessageBody)
	}
	for _, c := range p.Comments.Nodes {
		sb.WriteByte('\n')
		sb.WriteString(c.Body)
	}
	for _, ref := range ExtractTiDBIssueRefsFromText(p.Repo.NameWithOwner, sb.String()) {
		seen[ref] = true
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

type gqlFullIssueNode struct {
	Number      int    `json:"number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	State       string `json:"state"`
	StateReason string `json:"stateReason"`
	CreatedAt   string `json:"createdAt"`
	UpdatedAt   string `json:"updatedAt"`
	ClosedAt    string `json:"closedAt"`
	Author      *struct {
		Login string `json:"login"`
	} `json:"author"`
	Assignees struct {
		Nodes []struct {
			Login string `json:"login"`
		} `json:"nodes"`
	} `json:"assignees"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	TimelineItems struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Typename string         `json:"__typename"`
			Source   *gqlTimelinePR `json:"source"`
			Subject  *gqlTimelinePR `json:"subject"`
		} `json:"nodes"`
	} `json:"timelineItems"`
	Comments *struct {
		TotalCount int `json:"totalCount"`
		Nodes      []struct {
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			CreatedAt string `json:"createdAt"`
			Body      string `json:"body"`
		} `json:"nodes"`
	} `json:"comments"`
}

type gqlTimelinePR struct {
	Typename    string `json:"__typename"`
	Number      int    `json:"number"`
	Title       string `json:"title"`
	State       string `json:"state"`
	IsDraft     bool   `json:"isDraft"`
	BaseRefName string `json:"baseRefName"`
	URL         string `json:"url"`
	UpdatedAt   string `json:"updatedAt"`
	MergedAt    string `json:"mergedAt"`
	Labels      struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Repo struct {
		NameWithOwner string `json:"nameWithOwner"`
	} `json:"repository"`
}

func (p gqlTimelinePR) labelNames() []string {
	if len(p.Labels.Nodes) == 0 {
		return nil
	}
	out := make([]string, 0, len(p.Labels.Nodes))
	for _, lb := range p.Labels.Nodes {
		if lb.Name != "" {
			out = append(out, lb.Name)
		}
	}
	return out
}

func (it gqlFullIssueNode) toLiveUpdate() LiveIssueUpdate {
	author := "ghost"
	if it.Author != nil && it.Author.Login != "" {
		author = it.Author.Login
	}
	assignees := make([]string, 0, len(it.Assignees.Nodes))
	for _, a := range it.Assignees.Nodes {
		if a.Login != "" {
			assignees = append(assignees, a.Login)
		}
	}
	labels := make([]string, 0, len(it.Labels.Nodes))
	ghSev := "unlabeled"
	hasPlanner := false
	hasBug := false
	isCustomer := false
	for _, l := range it.Labels.Nodes {
		if l.Name == "" {
			continue
		}
		labels = append(labels, l.Name)
		if strings.HasPrefix(l.Name, "severity/") && ghSev == "unlabeled" {
			ghSev = strings.SplitN(l.Name, "/", 2)[1]
		}
		if l.Name == "sig/planner" {
			hasPlanner = true
		}
		if l.Name == "type/bug" {
			hasBug = true
		}
		if l.Name == "report/customer" {
			isCustomer = true
		}
	}

	byURL := make(map[string]model.LinkedPRRef)
	for _, tn := range it.TimelineItems.Nodes {
		pr := tn.Source
		if pr == nil {
			pr = tn.Subject
		}
		if pr == nil || pr.Typename != "PullRequest" || !IsKernelRepo(pr.Repo.NameWithOwner) || pr.Number <= 0 {
			continue
		}
		ref := BuildLinkedPRRefWithMeta(
			pr.Repo.NameWithOwner, pr.Number, pr.Title, pr.State, pr.IsDraft,
			pr.URL, pr.UpdatedAt, pr.labelNames(), pr.BaseRefName, model.LinkSourceTimeline,
		)
		ref.MergedAt = strings.TrimSpace(pr.MergedAt)
		byURL[ref.URL] = ref
	}
	prs := make([]model.LinkedPRRef, 0, len(byURL))
	for _, p := range byURL {
		prs = append(prs, p)
	}
	SortLinkedPRRefs(prs)

	created := it.CreatedAt
	if len(created) >= 10 {
		created = created[:10]
	}

	upd := LiveIssueUpdate{
		Number:            it.Number,
		Title:             it.Title,
		Author:            author,
		Assignees:         assignees,
		Created:           created,
		CreatedAt:         it.CreatedAt,
		UpdatedAt:         it.UpdatedAt,
		Labels:            labels,
		IsCanonical:       hasPlanner && hasBug,
		IsCustomer:        isCustomer,
		GHSeverity:        ghSev,
		TimelinePRs:       prs,
		TimelineTruncated: it.TimelineItems.TotalCount > len(it.TimelineItems.Nodes),
	}
	if it.Comments != nil {
		upd.HasBody = true
		upd.Body = it.Body
		upd.HasComments = true
		cmts := make([]model.PendingComment, 0, len(it.Comments.Nodes))
		for _, c := range it.Comments.Nodes {
			cAuthor := "ghost"
			if c.Author != nil && c.Author.Login != "" {
				cAuthor = c.Author.Login
			}
			cmts = append(cmts, model.PendingComment{
				Author:    cAuthor,
				CreatedAt: c.CreatedAt,
				Body:      c.Body,
			})
		}
		upd.RecentComments = cmts
	}
	return upd
}

func (it gqlFullIssueNode) toPendingCandidate(
	scopes []string,
	extraPRs []model.LinkedPRRef,
	prevPRs []model.LinkedPRRef,
	directUpdates map[string]model.LinkedPRRef,
	unlinkedPRKeys map[string]bool,
) model.PendingCandidate {
	upd := it.toLiveUpdate()
	prMap := make(map[string]model.LinkedPRRef, len(upd.TimelinePRs)+len(extraPRs)+len(prevPRs))
	for _, p := range upd.TimelinePRs {
		p.Source = model.LinkSourceTimeline
		prMap[model.PRKey(p.Repo, p.Num)] = p
	}
	for _, p := range extraPRs {
		k := model.PRKey(p.Repo, p.Num)
		if prev, ok := prMap[k]; ok {
			if model.CompareRFC3339(p.UpdatedAt, prev.UpdatedAt) > 0 {
				p.Source = prev.Source
				if p.MergedAt == "" && prev.MergedAt != "" {
					p.MergedAt = prev.MergedAt
				}
				prMap[k] = p
			} else if prev.MergedAt == "" && p.MergedAt != "" {
				prev.MergedAt = p.MergedAt
				prMap[k] = prev
			}
			continue
		}
		if p.Source == "" {
			p.Source = model.LinkSourcePRRef
		}
		prMap[k] = p
	}
	for _, p := range prevPRs {
		if p.Source != model.LinkSourcePRRef && p.Source != model.LinkSourceManual {
			continue
		}
		k := model.PRKey(p.Repo, p.Num)
		if p.Source == model.LinkSourcePRRef && unlinkedPRKeys[k] {
			continue
		}
		if _, exists := prMap[k]; exists {
			continue
		}
		if d, ok := directUpdates[k]; ok {
			d.Source = p.Source
			prMap[k] = d
		} else {
			prMap[k] = p
		}
	}
	prs := make([]model.LinkedPRRef, 0, len(prMap))
	for _, p := range prMap {
		prs = append(prs, p)
	}
	SortLinkedPRRefs(prs)

	cmts := upd.RecentComments
	if cmts == nil {
		cmts = []model.PendingComment{}
	}

	return model.PendingCandidate{
		Number:         it.Number,
		URL:            fmt.Sprintf("https://github.com/pingcap/tidb/issues/%d", it.Number),
		Title:          upd.Title,
		Author:         upd.Author,
		Assignees:      upd.Assignees,
		CreatedAt:      upd.CreatedAt,
		UpdatedAt:      upd.UpdatedAt,
		Labels:         upd.Labels,
		MatchedScopes:  scopes,
		PRs:            prs,
		Body:           it.Body,
		RecentComments: cmts,
	}
}

func (e *Engine) fetchIssuesBatch(
	ctx context.Context,
	tracker *ghgql.CostTracker,
	nums []int,
	detailed bool,
) (map[int]gqlFullIssueNode, error) {
	if len(nums) == 0 {
		return map[int]gqlFullIssueNode{}, nil
	}
	seen := make(map[int]bool, len(nums))
	ordered := make([]int, 0, len(nums))
	for _, n := range nums {
		if n > 0 && !seen[n] {
			seen[n] = true
			ordered = append(ordered, n)
		}
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] > ordered[j] })

	step := 50
	fragDecl := IssueFieldsFragment
	fragName := "IssueFields"
	if detailed {
		step = 25
		fragDecl = IssueDetailFieldsFragment
		fragName = "IssueDetailFields"
	}

	var chunks [][]int
	for i := 0; i < len(ordered); i += step {
		end := i + step
		if end > len(ordered) {
			end = len(ordered)
		}
		chunks = append(chunks, ordered[i:end])
	}

	var (
		mu       sync.Mutex
		out      = make(map[int]gqlFullIssueNode, len(ordered))
		firstErr error
	)
	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for _, chunk := range chunks {
		chunk := chunk
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				mu.Lock()
				if firstErr == nil {
					firstErr = ctx.Err()
				}
				mu.Unlock()
				return
			}

			var aliases strings.Builder
			for _, n := range chunk {
				fmt.Fprintf(&aliases, "    i%d: issue(number: %d) { ...%s }\n", n, n, fragName)
			}
			gql := fmt.Sprintf(
				"%s\nquery {\n  rateLimit { limit cost remaining used resetAt }\n  repository(owner: \"pingcap\", name: \"tidb\") {\n%s  }\n}",
				fragDecl, aliases.String(),
			)
			rawBytes, err := e.Runner.Query(ctx, gql, nil)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}
			var resp struct {
				Data *struct {
					RateLimit  *ghgql.RateLimitNode         `json:"rateLimit"`
					Repository map[string]*gqlFullIssueNode `json:"repository"`
				} `json:"data"`
				Errors []struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(rawBytes, &resp); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("decode batch issues: %w", err)
				}
				mu.Unlock()
				return
			}
			if resp.Data != nil && tracker != nil {
				tracker.Observe(resp.Data.RateLimit)
			}
			for _, ge := range resp.Errors {
				isNotFound := strings.EqualFold(ge.Type, "NOT_FOUND") ||
					(ge.Type == "" && strings.Contains(ge.Message, "Could not resolve to an Issue"))
				if !isNotFound {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("graphql batch issues error: %s", ge.Message)
					}
					mu.Unlock()
					return
				}
			}
			if resp.Data == nil || resp.Data.Repository == nil {
				mu.Lock()
				if firstErr == nil {
					if len(resp.Errors) > 0 {
						firstErr = fmt.Errorf("graphql batch issues error: %s", resp.Errors[0].Message)
					} else {
						firstErr = fmt.Errorf("graphql batch issues returned empty repository")
					}
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			for _, n := range chunk {
				if item := resp.Data.Repository[fmt.Sprintf("i%d", n)]; item != nil && item.Number > 0 {
					out[n] = *item
				}
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = ctx.Err()
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return out, nil
}

func decodeIssueSearchNode(raw json.RawMessage) (gqlIssueSearchNode, bool) {
	var n gqlIssueSearchNode
	if err := json.Unmarshal(raw, &n); err != nil || n.Number <= 0 {
		return gqlIssueSearchNode{}, false
	}
	return n, true
}

func decodePRSearchNode(raw json.RawMessage) (gqlPRSearchNode, bool) {
	var p gqlPRSearchNode
	if err := json.Unmarshal(raw, &p); err != nil || p.Number <= 0 || p.Repo.NameWithOwner == "" {
		return gqlPRSearchNode{}, false
	}
	return p, true
}

func resolveSyncWindow(prevMeta model.SyncMeta, opts Options) (now, sinceTime time.Time, explicitSince, useFull bool, modeUsed string, err error) {
	now = opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	overlapMin := opts.OverlapMinutes
	if overlapMin <= 0 {
		overlapMin = 15
	}
	maxIncDays := opts.MaxIncrementalDays
	if maxIncDays <= 0 {
		maxIncDays = 14
	}

	var prevSnap time.Time
	if prevMeta.SnapshotUTC != "" {
		if t, pErr := time.Parse(time.RFC3339Nano, prevMeta.SnapshotUTC); pErr == nil {
			prevSnap = t.UTC()
		} else if t, pErr := time.Parse(time.RFC3339, prevMeta.SnapshotUTC); pErr == nil {
			prevSnap = t.UTC()
		}
	}

	explicitSince = strings.TrimSpace(opts.Since) != ""
	if explicitSince {
		rawSince := strings.TrimSpace(opts.Since)
		t, pErr := time.Parse(time.RFC3339Nano, rawSince)
		if pErr != nil {
			t, pErr = time.Parse(time.RFC3339, rawSince)
		}
		if pErr != nil {
			return now, time.Time{}, true, false, "", fmt.Errorf("invalid --since timestamp %q (expected RFC3339 UTC): %w", opts.Since, pErr)
		}
		sinceTime = t.UTC()
	} else if prevSnap.IsZero() {
		sinceTime = now.AddDate(0, 0, -30)
	} else {
		sinceTime = prevSnap.Add(-time.Duration(overlapMin) * time.Minute)
	}

	useFull = opts.Full
	modeUsed = "incremental"
	if useFull {
		modeUsed = "full"
	} else if !explicitSince && (prevSnap.IsZero() || now.Sub(prevSnap) > time.Duration(maxIncDays)*24*time.Hour) {
		useFull = true
		modeUsed = "full-gap-auto"
	}
	return now, sinceTime, explicitSince, useFull, modeUsed, nil
}

// WillPromoteToFull reports whether PlanSync will execute in full mode (explicit --full or >maxIncrementalDays gap).
func WillPromoteToFull(prevMeta model.SyncMeta, opts Options) bool {
	_, _, _, useFull, _, err := resolveSyncWindow(prevMeta, opts)
	if err != nil {
		return opts.Full
	}
	return useFull
}

// PlanSync runs the dual-stream GitHub GraphQL sync without holding any database locks.
func (e *Engine) PlanSync(ctx context.Context, state SyncState, opts Options) (SyncPlan, error) {
	now, sinceTime, explicitSince, useFull, modeUsed, err := resolveSyncWindow(state.PrevMeta, opts)
	if err != nil {
		return SyncPlan{}, err
	}
	lookbackDays := opts.UntriagedLookbackDays
	if lookbackDays <= 0 {
		lookbackDays = DefaultUntriagedLookbackDays
	}

	tracker := opts.Tracker
	if tracker == nil {
		tracker = ghgql.NewCostTracker()
	}

	pendingScopes := make(map[int][]string, len(state.PendingScopes)+len(state.PendingCandidates))
	for n, sc := range state.PendingScopes {
		pendingScopes[n] = sc
	}
	for n, pc := range state.PendingCandidates {
		if _, ok := pendingScopes[n]; !ok {
			pendingScopes[n] = pc.MatchedScopes
		}
	}

	lookupExcluded := func(num int) (ExcludedIssueState, bool) {
		if state.ExcludedIssues != nil {
			if ex, ok := state.ExcludedIssues[num]; ok {
				return ex, true
			}
		}
		if lbs, ok := state.ExcludedLabels[num]; ok {
			return ExcludedIssueState{Labels: lbs, LabelsKnown: len(lbs) > 0}, true
		}
		return ExcludedIssueState{}, false
	}

	// Build reverse maps from currently linked PR -> tracked / pending issue numbers
	prevPRToIssues := make(map[string][]int)
	for issNum, tr := range state.TrackedIssues {
		for _, p := range tr.PRs {
			key := model.PRKey(p.Repo, p.Num)
			prevPRToIssues[key] = append(prevPRToIssues[key], issNum)
		}
	}
	prevPendingPRToIssues := make(map[string][]int)
	for issNum, pc := range state.PendingCandidates {
		for _, p := range pc.PRs {
			key := model.PRKey(p.Repo, p.Num)
			prevPendingPRToIssues[key] = append(prevPendingPRToIssues[key], issNum)
		}
	}

	var repoQuals strings.Builder
	for i, r := range KernelRepos {
		if i > 0 {
			repoQuals.WriteByte(' ')
		}
		repoQuals.WriteString("repo:")
		repoQuals.WriteString(r)
	}
	prBaseQuery := repoQuals.String() + " is:pr"

	// Stream B runs in BOTH incremental and --full modes.
	// In --full mode, ensure we look back AT LEAST 30 days (while preserving even older prevSnap gaps).
	streamBSince := sinceTime
	if useFull && !explicitSince {
		floor30d := now.AddDate(0, 0, -30)
		if streamBSince.After(floor30d) {
			streamBSince = floor30d
		}
	}
	prNodes, err := SearchWithBisection(
		ctx, e.Runner, tracker, SearchPRsGQL, prBaseQuery, streamBSince, now, BisectThreshold, decodePRSearchNode,
	)
	if err != nil {
		return SyncPlan{}, fmt.Errorf("stream B PR search failed: %w", err)
	}

	streamBPRRefs := make(map[int][]model.LinkedPRRef)
	streamBRefsByPR := make(map[string]map[int]bool)
	directPRUpdates := make(map[string]model.LinkedPRRef)
	streamBPRTitlesByIssue := make(map[int][]string)
	dirtyTracked := make(map[int]bool)
	dirtyPending := make(map[int]bool)

	for _, prNode := range prNodes {
		ref := BuildLinkedPRRefWithMeta(
			prNode.Repo.NameWithOwner, prNode.Number, prNode.Title,
			prNode.State, prNode.IsDraft, prNode.URL, prNode.UpdatedAt,
			prNode.labelNames(), prNode.BaseRefName, model.LinkSourcePRRef,
		)
		ref.MergedAt = strings.TrimSpace(prNode.MergedAt)
		prKey := model.PRKey(ref.Repo, ref.Num)
		if prevIssues, ok := prevPRToIssues[prKey]; ok {
			directPRUpdates[prKey] = ref
			for _, issNum := range prevIssues {
				dirtyTracked[issNum] = true
			}
		}
		if prevPendIssues, ok := prevPendingPRToIssues[prKey]; ok {
			directPRUpdates[prKey] = ref
			for _, issNum := range prevPendIssues {
				dirtyPending[issNum] = true
			}
		}
		refs := prNode.extractReferencedTiDBIssues()
		refSet := make(map[int]bool, len(refs))
		for _, refIssNum := range refs {
			refSet[refIssNum] = true
			streamBPRRefs[refIssNum] = append(streamBPRRefs[refIssNum], ref)
			streamBPRTitlesByIssue[refIssNum] = append(streamBPRTitlesByIssue[refIssNum], prNode.Title)
			if _, ok := state.TrackedIssues[refIssNum]; ok {
				dirtyTracked[refIssNum] = true
			}
			if _, ok := pendingScopes[refIssNum]; ok {
				dirtyPending[refIssNum] = true
			}
		}
		if prNode.hasCompleteRefScan() {
			streamBRefsByPR[prKey] = refSet
		}
	}
	for issNum := range streamBPRRefs {
		SortLinkedPRRefs(streamBPRRefs[issNum])
	}

	// Detect `pr_ref`-sourced links whose PR was seen in Stream B and edited to remove the `#issue` reference.
	unlinkedStreamB := make(map[int][]string)
	unlinkedStreamBSet := make(map[int]map[string]bool)
	recordUnlinkedPRRef := func(issNum int, prKey string) {
		if unlinkedStreamBSet[issNum] == nil {
			unlinkedStreamBSet[issNum] = make(map[string]bool)
		}
		if !unlinkedStreamBSet[issNum][prKey] {
			unlinkedStreamBSet[issNum][prKey] = true
			unlinkedStreamB[issNum] = append(unlinkedStreamB[issNum], prKey)
		}
	}
	for issNum, tr := range state.TrackedIssues {
		for _, p := range tr.PRs {
			if p.Source != model.LinkSourcePRRef {
				continue
			}
			prKey := model.PRKey(p.Repo, p.Num)
			if refSet, seenInStreamB := streamBRefsByPR[prKey]; seenInStreamB && !refSet[issNum] {
				recordUnlinkedPRRef(issNum, prKey)
				dirtyTracked[issNum] = true
			}
		}
	}
	for issNum, pc := range state.PendingCandidates {
		for _, p := range pc.PRs {
			if p.Source != model.LinkSourcePRRef {
				continue
			}
			prKey := model.PRKey(p.Repo, p.Num)
			if refSet, seenInStreamB := streamBRefsByPR[prKey]; seenInStreamB && !refSet[issNum] {
				recordUnlinkedPRRef(issNum, prKey)
				dirtyPending[issNum] = true
			}
		}
	}

	// Stream A: search issues
	issueBaseQuery := "repo:pingcap/tidb is:issue"
	streamASince := sinceTime
	if useFull {
		issueBaseQuery = "repo:pingcap/tidb is:issue is:open"
		if !explicitSince {
			floorLookback := now.AddDate(0, 0, -lookbackDays)
			if streamASince.After(floorLookback) {
				streamASince = floorLookback
			}
		}
	}
	issNodes, err := SearchWithBisection(
		ctx, e.Runner, tracker, SearchIssuesGQL, issueBaseQuery, streamASince, now, BisectThreshold, decodeIssueSearchNode,
	)
	if err != nil {
		return SyncPlan{}, fmt.Errorf("stream A issue search failed: %w", err)
	}
	if useFull && !explicitSince {
		tidbEpoch := time.Date(2015, 9, 1, 0, 0, 0, 0, time.UTC)
		if streamASince.After(tidbEpoch) {
			seenIss := make(map[int]bool, len(issNodes))
			for _, n := range issNodes {
				seenIss[n.Number] = true
			}
			histQueries := []string{
				"repo:pingcap/tidb is:issue is:open label:sig/planner -label:type/feature-request -label:type/sub-task",
				"repo:pingcap/tidb is:issue is:open -label:sig/planner -label:type/feature-request -label:type/sub-task (label:component/statistics OR label:component/bindinfo OR label:component/spm OR label:component/mview)",
			}
			for _, hq := range histQueries {
				olderTier1, err := SearchWithBisection(
					ctx, e.Runner, tracker, SearchIssuesGQL,
					hq, tidbEpoch, streamASince, BisectThreshold, decodeIssueSearchNode,
				)
				if err != nil {
					return SyncPlan{}, fmt.Errorf("stream A historical tier-1 full search failed: %w", err)
				}
				for _, n := range olderTier1 {
					if !seenIss[n.Number] {
						seenIss[n.Number] = true
						issNodes = append(issNodes, n)
					}
				}
			}
		}
	}

	discoveredScopes := make(map[int][]string)
	tier2NeedBodySet := make(map[int]bool)
	readmittedSet := make(map[int]bool)
	restoredNumsSet := make(map[int]bool)
	seenInStreamA := make(map[int]bool, len(issNodes))

	for _, node := range issNodes {
		num := node.Number
		seenInStreamA[num] = true
		if _, isTracked := state.TrackedIssues[num]; isTracked {
			dirtyTracked[num] = true
			continue
		}
		if _, isPend := pendingScopes[num]; isPend {
			dirtyPending[num] = true
		}
		if !strings.EqualFold(node.State, "OPEN") {
			continue
		}
		lbs := node.labelNames()

		// Evaluate exclusion BEFORE archived_issues so manually excluded issues are never
		// resurrected via archived_issues unless legitimately re-admitted by label delta.
		var readmitMarkers []string
		if exState, isExcluded := lookupExcluded(num); isExcluded {
			readmitMarkers = EvaluateExcludedReadmission(lbs, exState)
			if len(readmitMarkers) == 0 {
				continue
			}
		}

		// Check if this is a reopened archived issue that still satisfies retention
		if arch, wasArchived := state.ArchivedIssues[num]; wasArchived && len(readmitMarkers) == 0 {
			if keep, _ := ShouldRetainTrackedIssue(lbs, arch.ForceKeep, arch.Cat); keep {
				restoredNumsSet[num] = true
				continue
			}
		}

		candIn := DiscoveryCandidateInput{
			Title:                 node.Title,
			Body:                  node.Body,
			Author:                node.authorLogin(),
			CreatedAt:             node.CreatedAt,
			Labels:                lbs,
			LinkedPRTitles:        streamBPRTitlesByIssue[num],
			Now:                   now,
			UntriagedLookbackDays: lookbackDays,
		}
		scopes := MatchDiscoveryScopes(candIn)
		if len(readmitMarkers) > 0 {
			scopes = append(scopes, readmitMarkers...)
			readmittedSet[num] = true
		}
		if len(scopes) > 0 {
			discoveredScopes[num] = scopes
		} else if node.Body == "" && NeedsTier2BodyInspection(candIn) {
			tier2NeedBodySet[num] = true
		}
	}

	// Stream B PR-driven discovery for untracked issues whose own `updatedAt` did not advance
	// (e.g. a PR referenced `#num` strictly via commit message or closingIssuesReferences without touching `#num`).
	for refIssNum, prTitles := range streamBPRTitlesByIssue {
		if seenInStreamA[refIssNum] {
			continue
		}
		if _, isTracked := state.TrackedIssues[refIssNum]; isTracked {
			continue
		}
		if _, isPend := pendingScopes[refIssNum]; isPend {
			continue
		}
		if _, isExcluded := lookupExcluded(refIssNum); isExcluded {
			continue
		}
		if _, isArchived := state.ArchivedIssues[refIssNum]; isArchived {
			continue
		}
		for _, t := range prTitles {
			if linkedOptPRTitleRe.MatchString(t) {
				tier2NeedBodySet[refIssNum] = true
				break
			}
		}
	}

	// Process manual recheck queue (e.g. issues restored from `excluded_issues` via UI/API)
	recheckManualSet := make(map[int]bool)
	var clearedRecheckNums []int
	for _, n := range state.RecheckNums {
		if n <= 0 {
			continue
		}
		clearedRecheckNums = append(clearedRecheckNums, n)
		if _, isTracked := state.TrackedIssues[n]; isTracked {
			dirtyTracked[n] = true
			continue
		}
		if _, isExcluded := lookupExcluded(n); isExcluded {
			continue
		}
		if _, wasArchived := state.ArchivedIssues[n]; wasArchived {
			restoredNumsSet[n] = true
			continue
		}
		recheckManualSet[n] = true
	}

	// Fetch tracked issues
	liveExisting := make(map[int]gqlFullIssueNode)
	if useFull {
		allTracked := make([]int, 0, len(state.TrackedIssues))
		for num := range state.TrackedIssues {
			allTracked = append(allTracked, num)
		}
		fetched, err := e.fetchIssuesBatch(ctx, tracker, allTracked, false)
		if err != nil {
			return SyncPlan{}, fmt.Errorf("batch fetch tracked issues failed: %w", err)
		}
		liveExisting = fetched

		// Fetch recent comments ONLY for open tracked issues whose updatedAt advanced
		var needComments []int
		for num, node := range liveExisting {
			if strings.EqualFold(node.State, "OPEN") {
				if prev, ok := state.TrackedIssues[num]; ok && model.CompareRFC3339(node.UpdatedAt, prev.UpdatedAt) > 0 {
					needComments = append(needComments, num)
				}
			}
		}
		if len(needComments) > 0 {
			detailedMap, err := e.fetchIssuesBatch(ctx, tracker, needComments, true)
			if err != nil {
				return SyncPlan{}, fmt.Errorf("batch fetch updated issue comments failed: %w", err)
			}
			for num, det := range detailedMap {
				liveExisting[num] = det
			}
		}
	} else if len(dirtyTracked) > 0 {
		dirtyList := make([]int, 0, len(dirtyTracked))
		for num := range dirtyTracked {
			dirtyList = append(dirtyList, num)
		}
		fetched, err := e.fetchIssuesBatch(ctx, tracker, dirtyList, true)
		if err != nil {
			return SyncPlan{}, fmt.Errorf("batch fetch dirty issues failed: %w", err)
		}
		liveExisting = fetched
	}

	// Fetch restored archived issues (detailed)
	var restoredIssues []LiveIssueUpdate
	if len(restoredNumsSet) > 0 {
		rNums := make([]int, 0, len(restoredNumsSet))
		for n := range restoredNumsSet {
			rNums = append(rNums, n)
		}
		rFetched, err := e.fetchIssuesBatch(ctx, tracker, rNums, true)
		if err != nil {
			return SyncPlan{}, fmt.Errorf("batch fetch restored issues failed: %w", err)
		}
		sort.Slice(rNums, func(i, j int) bool { return rNums[i] > rNums[j] })
		for _, n := range rNums {
			if it, ok := rFetched[n]; ok && strings.EqualFold(it.State, "OPEN") {
				arch := state.ArchivedIssues[n]
				upd := it.toLiveUpdate()
				if keep, _ := ShouldRetainTrackedIssue(upd.Labels, arch.ForceKeep, arch.Cat); keep {
					restoredIssues = append(restoredIssues, upd)
				} else if slicesContainsInt(state.RecheckNums, n) {
					delete(restoredNumsSet, n)
					recheckManualSet[n] = true
				}
			}
		}
	}

	// Fetch newly discovered + dirty/full pending candidates + Tier-2 body candidates + manual rechecks (detailed)
	pendingToCheckSet := make(map[int]bool)
	for n := range discoveredScopes {
		if _, tracked := state.TrackedIssues[n]; !tracked && !restoredNumsSet[n] {
			pendingToCheckSet[n] = true
		}
	}
	for n := range tier2NeedBodySet {
		if _, tracked := state.TrackedIssues[n]; !tracked && !restoredNumsSet[n] {
			pendingToCheckSet[n] = true
		}
	}
	for n := range recheckManualSet {
		if _, tracked := state.TrackedIssues[n]; !tracked && !restoredNumsSet[n] {
			pendingToCheckSet[n] = true
		}
	}
	for n := range pendingScopes {
		if _, tracked := state.TrackedIssues[n]; tracked || restoredNumsSet[n] {
			continue
		}
		// In incremental mode, skip re-fetching existing pending candidates that had zero
		// issue or PR activity when full `PendingCandidates` state was loaded from SQLite.
		hasLoadedCandidate := state.PendingCandidates != nil
		if useFull || !hasLoadedCandidate || dirtyPending[n] {
			pendingToCheckSet[n] = true
		}
	}
	pendingToCheck := make([]int, 0, len(pendingToCheckSet))
	for n := range pendingToCheckSet {
		pendingToCheck = append(pendingToCheck, n)
	}
	sort.Slice(pendingToCheck, func(i, j int) bool { return pendingToCheck[i] > pendingToCheck[j] })

	var (
		upsertPending         []model.PendingCandidate
		deletePendingNums     []int
		closedPendingRemovals []model.DeltaClosedIssue
		pendingLiveUpdates    = make(map[int]LiveIssueUpdate)
		readmittedNums        []int
	)
	if len(pendingToCheck) > 0 {
		liveNew, err := e.fetchIssuesBatch(ctx, tracker, pendingToCheck, true)
		if err != nil {
			return SyncPlan{}, fmt.Errorf("batch fetch pending candidates failed: %w", err)
		}
		for _, n := range pendingToCheck {
			it, ok := liveNew[n]
			if !ok || !strings.EqualFold(it.State, "OPEN") {
				if _, wasPending := pendingScopes[n]; wasPending {
					deletePendingNums = append(deletePendingNums, n)
					cr := model.DeltaClosedIssue{
						Number: n,
						Reason: "Transferred or converted (NOT_FOUND on GitHub)",
					}
					if ok {
						reason := it.StateReason
						if reason == "" {
							reason = "COMPLETED"
						}
						cr.ClosedAt = it.ClosedAt
						cr.Reason = fmt.Sprintf("Closed (%s)", reason)
					}
					closedPendingRemovals = append(closedPendingRemovals, cr)
				}
				continue
			}
			upd := it.toLiveUpdate()
			if _, wasPending := pendingScopes[n]; wasPending {
				pendingLiveUpdates[n] = upd
			}
			prTitles := make([]string, 0, len(upd.TimelinePRs)+len(streamBPRTitlesByIssue[n]))
			for _, p := range upd.TimelinePRs {
				prTitles = append(prTitles, p.Title)
			}
			prTitles = append(prTitles, streamBPRTitlesByIssue[n]...)

			scopes := MatchDiscoveryScopes(DiscoveryCandidateInput{
				Title:                 it.Title,
				Body:                  it.Body,
				Author:                upd.Author,
				CreatedAt:             it.CreatedAt,
				Labels:                upd.Labels,
				LinkedPRTitles:        prTitles,
				Now:                   now,
				UntriagedLookbackDays: lookbackDays,
			})
			if len(scopes) == 0 {
				// Preserve previously matched scopes if the candidate was already in pending_candidates
				// unless a scope/retention guard now disqualifies it (e.g. relabeled to enhancement or non-opt).
				if prevScopes, wasPending := pendingScopes[n]; wasPending && !isDisqualifiedFromReadmission(upd.Labels) {
					scopes = prevScopes
				}
			}
			if readmittedSet[n] {
				if exState, ok := lookupExcluded(n); ok {
					for _, m := range EvaluateExcludedReadmission(upd.Labels, exState) {
						scopes = appendUniqueStr(scopes, m)
					}
				}
			}
			if recheckManualSet[n] && len(scopes) == 0 {
				scopes = []string{"restored_manual"}
			}
			if len(scopes) == 0 {
				if _, wasPending := pendingScopes[n]; wasPending {
					deletePendingNums = append(deletePendingNums, n)
				}
				continue
			}
			var prevPRs []model.LinkedPRRef
			var prevDetails []model.PendingPRDetail
			if state.PendingCandidates != nil {
				if prevPC, ok := state.PendingCandidates[n]; ok {
					prevPRs = prevPC.PRs
					prevDetails = prevPC.PRDetails
				}
			}
			pc := it.toPendingCandidate(scopes, streamBPRRefs[n], prevPRs, directPRUpdates, unlinkedStreamBSet[n])
			if len(prevDetails) > 0 {
				pc.PRDetails = prevDetails
			}
			upsertPending = append(upsertPending, pc)
			if readmittedSet[n] {
				readmittedNums = append(readmittedNums, n)
			}
		}
	}
	for n := range restoredNumsSet {
		if _, wasPending := pendingScopes[n]; wasPending {
			deletePendingNums = append(deletePendingNums, n)
		}
	}

	// Evaluate live updates on tracked issues
	trackedNums := make([]int, 0, len(state.TrackedIssues))
	for n := range state.TrackedIssues {
		trackedNums = append(trackedNums, n)
	}
	sort.Slice(trackedNums, func(i, j int) bool { return trackedNums[i] > trackedNums[j] })

	var (
		updatedIssues     []LiveIssueUpdate
		closedRemovals    []model.DeltaClosedIssue
		graduatedRemovals []model.DeltaGraduatedIssue
		prChangedNums     []int
		metaChangedNums   []int
	)

	for _, num := range trackedNums {
		prev := state.TrackedIssues[num]
		liveNode, ok := liveExisting[num]
		if !ok {
			if useFull || dirtyTracked[num] {
				closedRemovals = append(closedRemovals, model.DeltaClosedIssue{
					Number:       num,
					PrevCat:      prev.Cat,
					PrevSeverity: prev.Severity,
					Reason:       "Transferred or converted (NOT_FOUND on GitHub)",
				})
			}
			continue
		}
		if !strings.EqualFold(liveNode.State, "OPEN") {
			reason := liveNode.StateReason
			if reason == "" {
				reason = "COMPLETED"
			}
			closedRemovals = append(closedRemovals, model.DeltaClosedIssue{
				Number:       num,
				PrevCat:      prev.Cat,
				PrevSeverity: prev.Severity,
				ClosedAt:     liveNode.ClosedAt,
				Reason:       fmt.Sprintf("Closed (%s)", reason),
			})
			continue
		}

		upd := liveNode.toLiveUpdate()
		if keep, gradReason := ShouldRetainTrackedIssue(upd.Labels, prev.ForceKeep, prev.Cat); !keep {
			graduatedRemovals = append(graduatedRemovals, model.DeltaGraduatedIssue{
				Number:        num,
				PrevCat:       prev.Cat,
				Reason:        gradReason,
				CurrentLabels: upd.Labels,
			})
			continue
		}

		metaChanged := prev.UpdatedAt != upd.UpdatedAt || prev.Title != upd.Title || !equalStringSet(prev.Labels, upd.Labels)
		if metaChanged {
			metaChangedNums = append(metaChangedNums, num)
		}
		prChanged := hasPRLinkDiff(prev.PRs, upd.TimelinePRs, streamBPRRefs[num], directPRUpdates, unlinkedStreamBSet[num])
		if prChanged {
			prChangedNums = append(prChangedNums, num)
		}
		if metaChanged || prChanged || upd.HasComments {
			updatedIssues = append(updatedIssues, upd)
		}
	}

	// Filter StreamBPRRefs to only active/restored/pending issues so we don't store links for unrelated TiDB issues.
	// Include pendingLiveUpdates and pendingScopes so any pending candidate concurrently accepted into `issues`
	// while PlanSync was running still receives its Stream B PR links when ApplyIssueSync executes.
	relevantIssues := make(map[int]bool, len(state.TrackedIssues)+len(restoredIssues)+len(upsertPending)+len(pendingScopes))
	for n := range state.TrackedIssues {
		relevantIssues[n] = true
	}
	for _, r := range restoredIssues {
		relevantIssues[r.Number] = true
	}
	for _, p := range upsertPending {
		relevantIssues[p.Number] = true
	}
	for n := range pendingLiveUpdates {
		relevantIssues[n] = true
	}
	for n := range pendingScopes {
		relevantIssues[n] = true
	}
	for _, c := range closedRemovals {
		delete(relevantIssues, c.Number)
	}
	for _, cp := range closedPendingRemovals {
		delete(relevantIssues, cp.Number)
	}
	for _, g := range graduatedRemovals {
		delete(relevantIssues, g.Number)
	}
	filteredStreamB := make(map[int][]model.LinkedPRRef)
	for issNum, refs := range streamBPRRefs {
		if relevantIssues[issNum] {
			filteredStreamB[issNum] = refs
		}
	}

	return SyncPlan{
		ModeUsed:              modeUsed,
		SnapshotUTC:           now.Format("2006-01-02T15:04:05Z"),
		SinceUTC:              sinceTime.Format("2006-01-02T15:04:05Z"),
		Stats:                 tracker.Snapshot(),
		UpdatedIssues:         updatedIssues,
		RestoredIssues:        restoredIssues,
		ClosedRemovals:        closedRemovals,
		ClosedPendingRemovals: closedPendingRemovals,
		GraduatedRemovals:     graduatedRemovals,
		PendingLiveUpdates:    pendingLiveUpdates,
		StreamBPRRefs:         filteredStreamB,
		UnlinkedStreamBPRs:    unlinkedStreamB,
		DirectPRUpdates:       directPRUpdates,
		UpsertPending:         upsertPending,
		DeletePendingNums:     deletePendingNums,
		ReadmittedNums:        readmittedNums,
		ClearedRecheckNums:    clearedRecheckNums,
		PRChangedNums:         prChangedNums,
		MetaChangedNums:       metaChangedNums,
	}, nil
}

func slicesContainsInt(s []int, target int) bool {
	for _, v := range s {
		if v == target {
			return true
		}
	}
	return false
}

func appendUniqueStr(slice []string, item string) []string {
	for _, s := range slice {
		if s == item {
			return slice
		}
	}
	return append(slice, item)
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ma := labelSet(a)
	for _, s := range b {
		if !ma[strings.TrimSpace(s)] {
			return false
		}
	}
	return true
}

func hasPRLinkDiff(
	prev []TrackedPRLink,
	timeline []model.LinkedPRRef,
	prRefs []model.LinkedPRRef,
	direct map[string]model.LinkedPRRef,
	unlinkedPRRefKeys map[string]bool,
) bool {
	type sig struct {
		state string
		isCP  bool
	}
	prevMap := make(map[string]sig, len(prev))
	nextMap := make(map[string]sig, len(timeline)+len(prRefs)+len(prev))
	for _, p := range prev {
		k := model.PRKey(p.Repo, p.Num)
		prevMap[k] = sig{state: p.State, isCP: p.IsCherryPick}
		if p.Source == model.LinkSourcePRRef && unlinkedPRRefKeys[k] {
			continue
		}
		if p.Source == model.LinkSourceManual || p.Source == model.LinkSourcePRRef {
			st := p.State
			cp := p.IsCherryPick
			if d, ok := direct[k]; ok {
				st = d.State
				cp = d.IsCherryPick
			}
			nextMap[k] = sig{state: st, isCP: cp}
		}
	}
	for _, p := range timeline {
		nextMap[model.PRKey(p.Repo, p.Num)] = sig{state: p.State, isCP: p.IsCherryPick}
	}
	for _, p := range prRefs {
		nextMap[model.PRKey(p.Repo, p.Num)] = sig{state: p.State, isCP: p.IsCherryPick}
	}
	if len(prevMap) != len(nextMap) {
		return true
	}
	for k, pv := range prevMap {
		if nv, ok := nextMap[k]; !ok || nv != pv {
			return true
		}
	}
	return false
}

// FormatSyncSummary renders a human-readable sync summary matching CLI expectations.
func FormatSyncSummary(plan SyncPlan, prevDs, nextDs model.Dataset) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "=== SYNC SUMMARY (%s MODE · %d GraphQL calls, cost=%d) ===\n",
		strings.ToUpper(plan.ModeUsed), plan.Stats.GraphQLCalls, plan.Stats.GraphQLCost)
	fmt.Fprintf(&sb, "  Prev Snapshot : %s (%s)\n", prevDs.SyncMeta.SnapshotUTC8, prevDs.SyncMeta.SnapshotUTC)
	fmt.Fprintf(&sb, "  New Snapshot  : %s (%s)\n", nextDs.SyncMeta.SnapshotUTC8, nextDs.SyncMeta.SnapshotUTC)
	fmt.Fprintf(&sb, "  Max Updated   : %s (%s)\n", nextDs.SyncMeta.MaxIssueUpdatedUTC8, nextDs.SyncMeta.MaxIssueUpdatedUTC)

	var closedNums, gradNums, restoredNums, pendNums []int
	if plan.Applied != nil {
		closedNums = append(closedNums, plan.Applied.ClosedRemovedNums...)
		gradNums = append(gradNums, plan.Applied.GraduatedRemovedNums...)
		restoredNums = append(restoredNums, plan.Applied.RestoredNums...)
		pendNums = append(pendNums, plan.Applied.PendingQueueNums...)
	} else {
		closedNums = make([]int, 0, len(plan.ClosedRemovals))
		for _, x := range plan.ClosedRemovals {
			closedNums = append(closedNums, x.Number)
		}
		gradNums = make([]int, 0, len(plan.GraduatedRemovals))
		for _, x := range plan.GraduatedRemovals {
			gradNums = append(gradNums, x.Number)
		}
		restoredNums = make([]int, 0, len(plan.RestoredIssues))
		for _, x := range plan.RestoredIssues {
			restoredNums = append(restoredNums, x.Number)
		}
		pendNums = make([]int, 0, len(plan.UpsertPending))
		for _, x := range plan.UpsertPending {
			pendNums = append(pendNums, x.Number)
		}
	}
	if closedNums == nil {
		closedNums = []int{}
	}
	if gradNums == nil {
		gradNums = []int{}
	}
	if pendNums == nil {
		pendNums = []int{}
	}

	fmt.Fprintf(&sb, "  Closed Removed: %v\n", closedNums)
	fmt.Fprintf(&sb, "  Graduated Out : %v\n", gradNums)
	if len(restoredNums) > 0 {
		fmt.Fprintf(&sb, "  Reopened Kept : %v\n", restoredNums)
	}
	fmt.Fprintf(&sb, "  Pending Queue : %v\n", pendNums)
	fmt.Fprintf(&sb, "  PR-Diff Issues: %v\n", plan.PRChangedNums)
	fmt.Fprintf(&sb, "  Meta-Updated  : %v\n", plan.MetaChangedNums)
	fmt.Fprintf(&sb, "  Candidates    : %d -> %d (canonical: %d, real_bugs: %d, retriage: %d)\n",
		prevDs.SyncMeta.TotalCandidates,
		nextDs.SyncMeta.TotalCandidates,
		nextDs.SyncMeta.CanonicalCandidates,
		nextDs.SyncMeta.RealBugs,
		len(nextDs.SyncMeta.RetriageIssueNums),
	)
	return sb.String()
}
