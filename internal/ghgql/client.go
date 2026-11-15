package ghgql

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/winoros/tidb-triage-hub/internal/httputil"
)

const (
	DefaultGraphQLEndpoint = "https://api.github.com/graphql"
	defaultMaxAttempts     = 3
	defaultBaseBackoff     = 1200 * time.Millisecond
)

// Runner executes a single GitHub GraphQL query with optional string variables.
type Runner interface {
	Query(ctx context.Context, gql string, vars map[string]string) ([]byte, error)
}

// ResolveEnvToken returns GH_TOKEN or GITHUB_TOKEN from the environment if set (matching `gh` CLI precedence).
func ResolveEnvToken() string {
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if tok := strings.TrimSpace(os.Getenv(k)); tok != "" {
			return tok
		}
	}
	return ""
}

// DefaultRunner prefers the zero-subprocess pure-Go HTTPRunner when GH_TOKEN or GITHUB_TOKEN
// is configured in the environment, falling back to `gh api graphql` via CLIRunner otherwise.
func DefaultRunner() Runner {
	if tok := ResolveEnvToken(); tok != "" {
		return NewHTTPRunner(tok)
	}
	return &CLIRunner{
		MaxAttempts: defaultMaxAttempts,
		BaseBackoff: defaultBaseBackoff,
	}
}

// HTTPRunner executes GitHub GraphQL queries directly over HTTPS without spawning external `gh` processes.
type HTTPRunner struct {
	Token       string
	Endpoint    string
	Client      *http.Client
	MaxAttempts int
	BaseBackoff time.Duration
}

// RedirectBlockedError indicates a deterministic redirect policy violation that must not be retried.
type RedirectBlockedError struct {
	msg string
}

func (e *RedirectBlockedError) Error() string {
	return e.msg
}

// SafeRedirectPolicy blocks HTTPS->HTTP scheme downgrades, cross-origin redirects (including
// same-hostname cross-port redirects that would leak Bearer tokens), and 301/302 method rewrites (POST -> GET),
// while permitting same-authority HTTP -> HTTPS scheme upgrades when the HTTP method is preserved.
func SafeRedirectPolicy(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return &RedirectBlockedError{msg: fmt.Sprintf("stopped after %d redirects", len(via))}
	}
	if len(via) == 0 {
		return nil
	}
	orig := via[0]
	prev := via[len(via)-1]
	if orig.URL != nil && req.URL != nil {
		origScheme := strings.ToLower(strings.TrimSpace(orig.URL.Scheme))
		prevScheme := ""
		if prev != nil && prev.URL != nil {
			prevScheme = strings.ToLower(strings.TrimSpace(prev.URL.Scheme))
		}
		reqScheme := strings.ToLower(strings.TrimSpace(req.URL.Scheme))
		if reqScheme != "http" && reqScheme != "https" {
			return &RedirectBlockedError{msg: fmt.Sprintf("refusing unsupported %s redirect to %s", req.URL.Scheme, req.URL.Redacted())}
		}
		if (origScheme == "https" || prevScheme == "https") && reqScheme != "https" {
			return &RedirectBlockedError{msg: fmt.Sprintf("refusing insecure HTTPS -> %s redirect to %s", req.URL.Scheme, req.URL.Redacted())}
		}
		sameAuthUpgrade := origScheme == "http" && reqScheme == "https" &&
			httputil.NormalizeOriginHost(orig.URL.Host, "http") != "" &&
			httputil.NormalizeOriginHost(orig.URL.Host, "http") == httputil.NormalizeOriginHost(req.URL.Host, "https")
		if !httputil.SameOrigin(orig.URL, req.URL) && !sameAuthUpgrade {
			origHost, ok1 := httputil.ExtractCanonicalHostname(orig.URL.Host)
			reqHost, ok2 := httputil.ExtractCanonicalHostname(req.URL.Host)
			if !ok1 || !ok2 || origHost != reqHost {
				return &RedirectBlockedError{msg: fmt.Sprintf("refusing cross-host redirect from %s to %s", orig.URL.Host, req.URL.Host)}
			}
			return &RedirectBlockedError{msg: fmt.Sprintf("refusing cross-port redirect from %s://%s to %s://%s", origScheme, orig.URL.Host, reqScheme, req.URL.Host)}
		}
	}
	if orig.Method != http.MethodGet && orig.Method != http.MethodHead && req.Method != orig.Method {
		targetBase := ""
		if req.URL != nil {
			u := &url.URL{Scheme: req.URL.Scheme, Host: req.URL.Host}
			targetBase = u.String()
		}
		return &RedirectBlockedError{msg: fmt.Sprintf("refusing %s -> %s redirect (configure target URL %s directly)", orig.Method, req.Method, targetBase)}
	}
	return nil
}

func NewHTTPRunner(token string) *HTTPRunner {
	return &HTTPRunner{
		Token:       strings.TrimSpace(token),
		Endpoint:    DefaultGraphQLEndpoint,
		Client:      &http.Client{Timeout: 60 * time.Second, CheckRedirect: SafeRedirectPolicy},
		MaxAttempts: defaultMaxAttempts,
		BaseBackoff: defaultBaseBackoff,
	}
}

func resolveRetryParams(maxAttempts int, baseBackoff time.Duration) (int, time.Duration) {
	if maxAttempts <= 0 {
		maxAttempts = defaultMaxAttempts
	}
	if baseBackoff <= 0 {
		baseBackoff = defaultBaseBackoff
	}
	return maxAttempts, baseBackoff
}

func waitRetryBackoff(ctx context.Context, attempt int, backoff time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(attempt+1) * backoff):
		return nil
	}
}

func (r *HTTPRunner) Query(ctx context.Context, gql string, vars map[string]string) ([]byte, error) {
	tok := strings.TrimSpace(r.Token)
	if tok == "" {
		return nil, fmt.Errorf("GitHub GraphQL token is empty (set GH_TOKEN or GITHUB_TOKEN)")
	}
	endpoint := strings.TrimSpace(r.Endpoint)
	if endpoint == "" {
		endpoint = DefaultGraphQLEndpoint
	}
	hc := r.Client
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second, CheckRedirect: SafeRedirectPolicy}
	} else if hc.CheckRedirect == nil {
		clone := *hc
		clone.CheckRedirect = SafeRedirectPolicy
		hc = &clone
	}
	attempts, backoff := resolveRetryParams(r.MaxAttempts, r.BaseBackoff)

	reqPayload := struct {
		Query     string            `json:"query"`
		Variables map[string]string `json:"variables,omitempty"`
	}{
		Query: gql,
	}
	if len(vars) > 0 {
		cleanVars := make(map[string]string, len(vars))
		for k, v := range vars {
			if v != "" {
				cleanVars[k] = v
			}
		}
		if len(cleanVars) > 0 {
			reqPayload.Variables = cleanVars
		}
	}
	bodyBytes, err := json.Marshal(reqPayload)
	if err != nil {
		return nil, fmt.Errorf("encode graphql payload: %w", err)
	}

	var lastErr error
	for attempt := 0; attempt < attempts; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
		if reqErr != nil {
			return nil, reqErr
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("User-Agent", "tidb-triage-hub")

		resp, doErr := hc.Do(req)
		if doErr != nil {
			lastErr = doErr
			var redirErr *RedirectBlockedError
			if errors.As(doErr, &redirErr) {
				return nil, lastErr
			}
		} else {
			limit := int64(32 << 20)
			if resp.StatusCode < 200 || resp.StatusCode >= 300 {
				limit = 64 << 10
			}
			raw, readErr := io.ReadAll(io.LimitReader(resp.Body, limit))
			_ = resp.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("read graphql response: %w", readErr)
			} else if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if isTransientGraphQLError(raw) {
					lastErr = fmt.Errorf("transient github graphql server error: %s", truncateSnippet(raw, 240))
				} else if !hasUsablePartialGraphQLData(raw) {
					return nil, formatDeterministicGraphQLError(raw)
				} else {
					return raw, nil
				}
			} else {
				snippet := truncateSnippet(raw, 240)
				lastErr = fmt.Errorf("github graphql HTTP %d: %s", resp.StatusCode, snippet)
				// Do not retry deterministic client/auth errors (e.g. 400, 401, 404, 422).
				if resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusForbidden {
					return nil, lastErr
				}
				if resp.StatusCode == http.StatusForbidden && !isSecondaryRateLimitText(strings.ToLower(snippet)) {
					return nil, lastErr
				}
			}
		}

		if attempt+1 >= attempts {
			break
		}
		if err := waitRetryBackoff(ctx, attempt, backoff); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func isSecondaryRateLimitText(lower string) bool {
	return strings.Contains(lower, "secondary rate limit") || strings.Contains(lower, "abuse")
}

func formatDeterministicGraphQLError(raw []byte) error {
	_, errs, ok := parseGraphQLErrorEnvelope(raw)
	if ok && len(errs) > 0 && strings.TrimSpace(errs[0].Message) != "" {
		if tp := strings.TrimSpace(errs[0].Type); tp != "" {
			return fmt.Errorf("github graphql error (%s): %s", tp, errs[0].Message)
		}
		return fmt.Errorf("github graphql error: %s", errs[0].Message)
	}
	return fmt.Errorf("github graphql returned unusable or empty data: %s", truncateSnippet(raw, 240))
}

func truncateSnippet(raw []byte, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return ""
	}
	capBytes := maxLen * 4
	if capBytes < maxLen {
		capBytes = maxLen
	}
	truncatedBytes := false
	if len(b) > capBytes {
		b = b[:capBytes]
		// Trim trailing incomplete UTF-8 rune bytes caused by byte cap
		for len(b) > 0 && !utf8.Valid(b) {
			r, size := utf8.DecodeLastRune(b)
			if r != utf8.RuneError || size != 1 {
				break
			}
			b = b[:len(b)-1]
		}
		truncatedBytes = true
	}
	s := strings.TrimSpace(strings.ToValidUTF8(string(b), "\uFFFD"))
	if utf8.RuneCountInString(s) <= maxLen {
		if truncatedBytes {
			return s + "..."
		}
		return s
	}
	count := 0
	for idx := range s {
		if count == maxLen {
			return strings.TrimSpace(s[:idx]) + "..."
		}
		count++
	}
	return s
}

type jsonNullProbe struct {
	present bool
	isNull  bool
}

func (p *jsonNullProbe) UnmarshalJSON(b []byte) error {
	p.present = true
	d := bytes.TrimSpace(b)
	p.isNull = len(d) == 0 || bytes.Equal(d, []byte("null"))
	return nil
}

type graphqlErrorItem struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func parseGraphQLErrorEnvelope(raw []byte) (hasNonNullData bool, errs []graphqlErrorItem, ok bool) {
	var env struct {
		Data   jsonNullProbe      `json:"data"`
		Errors []graphqlErrorItem `json:"errors"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return false, nil, false
	}
	return env.Data.present && !env.Data.isNull, env.Errors, true
}

func isNotFoundGraphQLError(ge graphqlErrorItem) bool {
	return strings.EqualFold(strings.TrimSpace(ge.Type), "NOT_FOUND") ||
		(strings.TrimSpace(ge.Type) == "" && strings.Contains(ge.Message, "Could not resolve to a"))
}

func isTransientGraphQLError(raw []byte) bool {
	if !bytes.Contains(raw, []byte(`"errors"`)) {
		return false
	}
	hasData, errs, ok := parseGraphQLErrorEnvelope(raw)
	if !ok || len(errs) == 0 {
		return false
	}
	if hasData {
		allNotFound := true
		for _, ge := range errs {
			if !isNotFoundGraphQLError(ge) {
				allNotFound = false
				break
			}
		}
		if allNotFound {
			return false
		}
	}
	for _, ge := range errs {
		tp := strings.ToUpper(strings.TrimSpace(ge.Type))
		msg := strings.ToLower(ge.Message)
		if tp == "INTERNAL" || tp == "SERVICE_UNAVAILABLE" || tp == "RATE_LIMITED" ||
			isSecondaryRateLimitText(msg) ||
			strings.Contains(msg, "something went wrong") ||
			strings.Contains(msg, "timed out") ||
			strings.Contains(msg, "loading data") ||
			strings.Contains(msg, "temporarily unavailable") ||
			strings.Contains(msg, "server error") {
			return true
		}
	}
	return false
}

// CLIRunner invokes `gh api graphql` with automatic retries on transient failures.
type CLIRunner struct {
	MaxAttempts int
	BaseBackoff time.Duration
}

func (r *CLIRunner) Query(ctx context.Context, gql string, vars map[string]string) ([]byte, error) {
	attempts, backoff := resolveRetryParams(r.MaxAttempts, r.BaseBackoff)

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
		attemptCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		cmd := exec.CommandContext(attemptCtx, "gh", args...)
		cmd.WaitDelay = 5 * time.Second
		out, err := cmd.Output()
		attemptErr := attemptCtx.Err()
		cancel()
		if err == nil {
			if isTransientGraphQLError(out) {
				lastErr = fmt.Errorf("transient github graphql server error: %s", truncateSnippet(out, 240))
			} else if !hasUsablePartialGraphQLData(out) {
				return nil, formatDeterministicGraphQLError(out)
			} else {
				return out, nil
			}
		} else if ctx.Err() != nil {
			return nil, fmt.Errorf("gh api graphql: %w", ctx.Err())
		} else if attemptErr != nil {
			lastErr = fmt.Errorf("gh api graphql attempt timeout: %w", attemptErr)
		} else if errors.Is(err, exec.ErrNotFound) {
			return nil, err
		} else if ee, ok := err.(*exec.ExitError); ok {
			// `gh api graphql` exits with status 1 whenever top-level `"errors"` is non-empty,
			// even when HTTP 200 `"data"` contains valid aliased items alongside NOT_FOUND aliases.
			// Return `out` without retrying ONLY when `"data"` is non-null and every error is NOT_FOUND.
			if hasUsablePartialGraphQLData(out) {
				return out, nil
			}
			stderrStr := strings.TrimSpace(string(ee.Stderr))
			lastErr = fmt.Errorf("gh api graphql exit: %w (%s)", err, stderrStr)
			lowerStderr := strings.ToLower(stderrStr)
			if strings.Contains(lowerStderr, "http 401") || strings.Contains(lowerStderr, "gh auth login") || strings.Contains(lowerStderr, "bad credentials") {
				return nil, lastErr
			}
			if _, errs, envOK := parseGraphQLErrorEnvelope(out); envOK && len(errs) > 0 && !isTransientGraphQLError(out) {
				return nil, formatDeterministicGraphQLError(out)
			}
		} else {
			lastErr = err
		}
		if attempt+1 >= attempts {
			break
		}
		if err := waitRetryBackoff(ctx, attempt, backoff); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

func hasUsablePartialGraphQLData(raw []byte) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}
	hasData, errs, ok := parseGraphQLErrorEnvelope(raw)
	if !ok || !hasData {
		return false
	}
	for _, ge := range errs {
		if !isNotFoundGraphQLError(ge) {
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
	ViewerLogin string `json:"viewer_login,omitempty"`
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
		runner = DefaultRunner()
	}
	if _, isCLI := runner.(*CLIRunner); isCLI {
		if _, err := exec.LookPath("gh"); err != nil {
			return RateLimitInfo{}, fmt.Errorf("gh CLI not found in PATH (install gh or set GITHUB_TOKEN): %w", err)
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
	if len(resp.Errors) > 0 {
		return RateLimitInfo{}, fmt.Errorf("gh GraphQL error: %s", resp.Errors[0].Message)
	}
	if resp.Data.RateLimit.Limit == 0 {
		return RateLimitInfo{}, fmt.Errorf("gh GraphQL check failed: invalid or empty rateLimit response")
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
