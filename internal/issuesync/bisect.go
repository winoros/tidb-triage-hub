package issuesync

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/model"
)

const BisectThreshold = 800

type SearchNode interface {
	NodeKey() string
	NodeUpdatedAt() string
}

type searchWindow struct {
	lo              time.Time
	hi              time.Time
	extraQual       string
	hasCreatedRange bool
	createdLo       time.Time
	createdHi       time.Time
}

type gqlSearchEnvelope struct {
	Data *struct {
		RateLimit *ghgql.RateLimitNode `json:"rateLimit"`
		Search    *struct {
			IssueCount int `json:"issueCount"`
			PageInfo   struct {
				HasNextPage bool   `json:"hasNextPage"`
				EndCursor   string `json:"endCursor"`
			} `json:"pageInfo"`
			Nodes []json.RawMessage `json:"nodes"`
		} `json:"search"`
	} `json:"data"`
	Errors []struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"errors"`
}

// SearchWithBisection executes a GitHub GraphQL search over [lo, hi] and recursively splits
// time windows whenever `issueCount > threshold` so GitHub's 1,000-hit cap never drops items.
// When `issueCount <= threshold`, page 1 results are reused directly without redundant queries.
func SearchWithBisection[T SearchNode](
	ctx context.Context,
	runner ghgql.Runner,
	tracker *ghgql.CostTracker,
	gql string,
	baseQuery string,
	lo time.Time,
	hi time.Time,
	threshold int,
	decode func(raw json.RawMessage) (T, bool),
) ([]T, error) {
	if threshold <= 0 || threshold > 950 {
		threshold = BisectThreshold
	}
	lo = lo.UTC().Truncate(time.Second)
	hi = hi.UTC().Truncate(time.Second)
	if hi.Before(lo) {
		hi = lo
	}

	stack := []searchWindow{{lo: lo, hi: hi}}
	dedup := make(map[string]T)
	var order []string

	recordNode := func(item T) {
		k := item.NodeKey()
		if k == "" {
			return
		}
		if prev, exists := dedup[k]; exists {
			if model.CompareRFC3339(item.NodeUpdatedAt(), prev.NodeUpdatedAt()) >= 0 {
				dedup[k] = item
			}
			return
		}
		dedup[k] = item
		order = append(order, k)
	}

	queryPage := func(q string, cursor string) (*gqlSearchEnvelope, error) {
		vars := map[string]string{"q": q}
		if cursor != "" {
			vars["cursor"] = cursor
		}
		rawBytes, err := runner.Query(ctx, gql, vars)
		if err != nil {
			return nil, err
		}
		var env gqlSearchEnvelope
		if err := json.Unmarshal(rawBytes, &env); err != nil {
			return nil, fmt.Errorf("decode search response: %w", err)
		}
		if env.Data != nil && tracker != nil {
			tracker.Observe(env.Data.RateLimit)
		}
		if len(env.Errors) > 0 {
			return nil, fmt.Errorf("graphql search error: %s", env.Errors[0].Message)
		}
		if env.Data == nil || env.Data.Search == nil {
			return nil, fmt.Errorf("graphql search returned empty data")
		}
		return &env, nil
	}

	for len(stack) > 0 {
		win := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		q := fmt.Sprintf(
			"%s updated:%s..%s",
			baseQuery,
			win.lo.Format("2006-01-02T15:04:05Z"),
			win.hi.Format("2006-01-02T15:04:05Z"),
		)
		if win.extraQual != "" {
			q += " " + win.extraQual
		}

		firstPage, err := queryPage(q, "")
		if err != nil {
			return nil, err
		}

		hitCap := threshold
		if win.hasCreatedRange && !win.createdHi.After(win.createdLo) {
			hitCap = 1000
		}

		count := firstPage.Data.Search.IssueCount
		if count > hitCap {
			if win.hi.After(win.lo) {
				spanSec := int64(win.hi.Sub(win.lo) / time.Second)
				mid := win.lo.Add(time.Duration(spanSec/2) * time.Second)
				rightLo := mid.Add(time.Second)
				// Push right first so left is popped first (chronological traversal).
				stack = append(stack,
					searchWindow{
						lo: rightLo, hi: win.hi, extraQual: win.extraQual,
						hasCreatedRange: win.hasCreatedRange, createdLo: win.createdLo, createdHi: win.createdHi,
					},
					searchWindow{
						lo: win.lo, hi: mid, extraQual: win.extraQual,
						hasCreatedRange: win.hasCreatedRange, createdLo: win.createdLo, createdHi: win.createdHi,
					},
				)
				continue
			}
			if !win.hasCreatedRange {
				// Single-second >threshold burst: split orthogonally by creation date first, then bisect recursively if needed.
				splitPivot := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
				tidbEpoch := time.Date(2015, 9, 1, 0, 0, 0, 0, time.UTC)
				hiBound := win.hi
				if hiBound.Before(splitPivot) {
					hiBound = splitPivot
				}
				stack = append(stack,
					searchWindow{
						lo: win.lo, hi: win.hi, extraQual: "created:>=2025-01-01",
						hasCreatedRange: true, createdLo: splitPivot, createdHi: hiBound,
					},
					searchWindow{
						lo: win.lo, hi: win.hi, extraQual: "created:<2025-01-01",
						hasCreatedRange: true, createdLo: tidbEpoch, createdHi: splitPivot.Add(-time.Second),
					},
				)
				continue
			}
			if win.createdHi.After(win.createdLo) {
				cSpan := int64(win.createdHi.Sub(win.createdLo) / time.Second)
				cMid := win.createdLo.Add(time.Duration(cSpan/2) * time.Second)
				cRightLo := cMid.Add(time.Second)
				stack = append(stack,
					searchWindow{
						lo: win.lo, hi: win.hi,
						extraQual:       fmt.Sprintf("created:%s..%s", cRightLo.Format("2006-01-02T15:04:05Z"), win.createdHi.Format("2006-01-02T15:04:05Z")),
						hasCreatedRange: true, createdLo: cRightLo, createdHi: win.createdHi,
					},
					searchWindow{
						lo: win.lo, hi: win.hi,
						extraQual:       fmt.Sprintf("created:%s..%s", win.createdLo.Format("2006-01-02T15:04:05Z"), cMid.Format("2006-01-02T15:04:05Z")),
						hasCreatedRange: true, createdLo: win.createdLo, createdHi: cMid,
					},
				)
				continue
			}
		}

		for _, rawNode := range firstPage.Data.Search.Nodes {
			if item, ok := decode(rawNode); ok {
				recordNode(item)
			}
		}

		hasNext := firstPage.Data.Search.PageInfo.HasNextPage
		cursor := firstPage.Data.Search.PageInfo.EndCursor
		for hasNext && cursor != "" {
			nextPage, err := queryPage(q, cursor)
			if err != nil {
				return nil, err
			}
			for _, rawNode := range nextPage.Data.Search.Nodes {
				if item, ok := decode(rawNode); ok {
					recordNode(item)
				}
			}
			hasNext = nextPage.Data.Search.PageInfo.HasNextPage
			cursor = nextPage.Data.Search.PageInfo.EndCursor
		}
	}

	out := make([]T, 0, len(order))
	for _, k := range order {
		out = append(out, dedup[k])
	}
	return out, nil
}
