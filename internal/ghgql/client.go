package ghgql

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// Runner executes a single GitHub GraphQL query with optional string variables.
type Runner interface {
	Query(ctx context.Context, gql string, vars map[string]string) ([]byte, error)
}

// CLIRunner invokes `gh api graphql` with automatic retries on transient failures.
type CLIRunner struct {
	MaxAttempts int
	BaseBackoff time.Duration
}

func DefaultRunner() *CLIRunner {
	return &CLIRunner{
		MaxAttempts: 3,
		BaseBackoff: 1200 * time.Millisecond,
	}
}

func (r *CLIRunner) Query(ctx context.Context, gql string, vars map[string]string) ([]byte, error) {
	attempts := r.MaxAttempts
	if attempts <= 0 {
		attempts = 3
	}
	backoff := r.BaseBackoff
	if backoff <= 0 {
		backoff = 1200 * time.Millisecond
	}

	args := []string{"api", "graphql", "-f", "query=" + gql}
	if len(vars) > 0 {
		keys := make([]string, 0, len(vars))
		for k, v := range vars {
			if v != "" {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			args = append(args, "-f", k+"="+vars[k])
		}
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		cmd := exec.CommandContext(ctx, "gh", args...)
		out, err := cmd.Output()
		if err == nil {
			return out, nil
		}
		if ee, ok := err.(*exec.ExitError); ok {
			// `gh api graphql` exits with status 1 whenever top-level `"errors"` is non-empty,
			// even when HTTP 200 `"data"` contains valid aliased items alongside NOT_FOUND aliases.
			// Return `out` without retrying ONLY when `"data"` is non-null and every error is NOT_FOUND.
			if hasUsablePartialGraphQLData(out) {
				return out, nil
			}
			lastErr = fmt.Errorf("gh api graphql exit: %w (%s)", err, strings.TrimSpace(string(ee.Stderr)))
		} else {
			lastErr = err
		}
		if attempt+1 >= attempts {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * backoff):
		}
	}
	return nil, lastErr
}

func hasUsablePartialGraphQLData(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	var env struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false
	}
	d := bytes.TrimSpace(env.Data)
	if len(d) == 0 || bytes.Equal(d, []byte("null")) {
		return false
	}
	for _, ge := range env.Errors {
		isNotFound := strings.EqualFold(ge.Type, "NOT_FOUND") ||
			(ge.Type == "" && strings.Contains(ge.Message, "Could not resolve to a"))
		if !isNotFound {
			return false
		}
	}
	return true
}

type RateLimitNode struct {
	Limit     int    `json:"limit"`
	Cost      int    `json:"cost"`
	Remaining int    `json:"remaining"`
	Used      int    `json:"used"`
	ResetAt   string `json:"resetAt"`
}

type RateLimitInfo struct {
	ViewerLogin string `json:"viewer_login"`
	Limit       int    `json:"limit"`
	Remaining   int    `json:"remaining"`
	Used        int    `json:"used"`
	ResetAtUTC  string `json:"reset_at_utc"`
	ResetAtUTC8 string `json:"reset_at_utc8"`
}

type CallStats struct {
	GraphQLCalls       int    `json:"graphql_calls"`
	GraphQLCost        int    `json:"graphql_cost"`
	RateLimitLimit     int    `json:"rate_limit_limit,omitempty"`
	RateLimitRemaining int    `json:"rate_limit_remaining,omitempty"`
	RateLimitResetUTC8 string `json:"rate_limit_reset_utc8,omitempty"`
}

// CostTracker aggregates call count, point cost, and minimum remaining quota across concurrent requests.
type CostTracker struct {
	mu          sync.Mutex
	calls       int
	cost        int
	limit       int
	remaining   int
	hasObserved bool
	resetUTC8   string
}

func NewCostTracker() *CostTracker {
	return &CostTracker{remaining: -1}
}

func (ct *CostTracker) Observe(rl *RateLimitNode) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	ct.calls++
	if rl == nil {
		return
	}
	ct.cost += rl.Cost
	ct.limit = rl.Limit
	if !ct.hasObserved || rl.Remaining < ct.remaining {
		ct.remaining = rl.Remaining
		ct.hasObserved = true
		if rl.ResetAt != "" {
			ct.resetUTC8 = ToUTC8Display(rl.ResetAt) + " (UTC+8)"
		}
	}
}

func (ct *CostTracker) Snapshot() CallStats {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	rem := ct.remaining
	if rem < 0 {
		rem = 0
	}
	return CallStats{
		GraphQLCalls:       ct.calls,
		GraphQLCost:        ct.cost,
		RateLimitLimit:     ct.limit,
		RateLimitRemaining: rem,
		RateLimitResetUTC8: ct.resetUTC8,
	}
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

func CheckQuota(ctx context.Context, runner Runner, minRemaining int) (RateLimitInfo, error) {
	if runner == nil {
		if _, err := exec.LookPath("gh"); err != nil {
			return RateLimitInfo{}, fmt.Errorf("gh CLI not found in PATH: %w", err)
		}
		runner = DefaultRunner()
	} else if _, isCLI := runner.(*CLIRunner); isCLI {
		if _, err := exec.LookPath("gh"); err != nil {
			return RateLimitInfo{}, fmt.Errorf("gh CLI not found in PATH: %w", err)
		}
	}

	// Query `rateLimit` alone so GitHub charges 0 GraphQL points (succeeding even when remaining == 0).
	raw, err := runner.Query(ctx, `query { rateLimit { limit remaining used resetAt } }`, nil)
	if err != nil {
		return RateLimitInfo{}, fmt.Errorf("gh GraphQL authentication/network check failed: %w", err)
	}
	var resp struct {
		Data struct {
			Viewer struct {
				Login string `json:"login"`
			} `json:"viewer"`
			RateLimit RateLimitNode `json:"rateLimit"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return RateLimitInfo{}, fmt.Errorf("decode rateLimit response: %w", err)
	}
	if resp.Data.RateLimit.Limit == 0 && len(resp.Errors) > 0 {
		return RateLimitInfo{}, fmt.Errorf("gh GraphQL error: %s", resp.Errors[0].Message)
	}
	info := RateLimitInfo{
		ViewerLogin: resp.Data.Viewer.Login,
		Limit:       resp.Data.RateLimit.Limit,
		Remaining:   resp.Data.RateLimit.Remaining,
		Used:        resp.Data.RateLimit.Used,
		ResetAtUTC:  resp.Data.RateLimit.ResetAt,
	}
	if info.ResetAtUTC != "" {
		info.ResetAtUTC8 = ToUTC8Display(info.ResetAtUTC) + " (UTC+8)"
	}
	if minRemaining > 0 && info.Remaining < minRemaining {
		who := ""
		if info.ViewerLogin != "" {
			who = " for @" + info.ViewerLogin
		}
		return info, fmt.Errorf(
			"insufficient GitHub GraphQL quota%s: remaining %d/%d points (need >= %d, resets at %s)",
			who, info.Remaining, info.Limit, minRemaining, info.ResetAtUTC8,
		)
	}
	return info, nil
}
