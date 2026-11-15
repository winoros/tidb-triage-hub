package ghgql

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

type mockRunner struct {
	fn func(ctx context.Context, gql string, vars map[string]string) ([]byte, error)
}

func (m *mockRunner) Query(ctx context.Context, gql string, vars map[string]string) ([]byte, error) {
	return m.fn(ctx, gql, vars)
}

func TestCostTrackerAndQuotaCheck(t *testing.T) {
	ct := NewCostTracker()
	ct.Observe(&RateLimitNode{
		Limit:     5000,
		Cost:      3,
		Remaining: 4920,
		ResetAt:   "2026-11-12T12:00:00Z",
	})
	ct.Observe(&RateLimitNode{
		Limit:     5000,
		Cost:      5,
		Remaining: 4915,
		ResetAt:   "2026-11-12T12:00:00Z",
	})
	snap := ct.Snapshot()
	if snap.GraphQLCalls != 2 || snap.GraphQLCost != 8 || snap.RateLimitRemaining != 4915 {
		t.Fatalf("unexpected CostTracker snapshot: %+v", snap)
	}

	mr := &mockRunner{
		fn: func(_ context.Context, gql string, _ map[string]string) ([]byte, error) {
			if strings.Contains(gql, "viewer") {
				t.Fatalf("expected CheckQuota to use 0-point rateLimit-only query without viewer, got: %s", gql)
			}
			return []byte(`{"data":{"rateLimit":{"limit":5000,"remaining":80,"used":4920,"resetAt":"2026-11-12T12:00:00Z"}}}`), nil
		},
	}

	info, err := CheckQuota(context.Background(), mr, 50)
	if err != nil || info.Remaining != 80 {
		t.Fatalf("expected quota check pass, got info=%+v err=%v", info, err)
	}

	_, err = CheckQuota(context.Background(), mr, 200)
	if err == nil || !strings.Contains(err.Error(), "insufficient GitHub GraphQL quota") {
		t.Fatalf("expected insufficient quota error, got %v", err)
	}
}

func TestCLIRunnerPartialGraphQLDataOnNonZeroExit(t *testing.T) {
	if !hasUsablePartialGraphQLData([]byte(`{"data":{"repository":{"p1":{"number":1},"p2":null}},"errors":[{"type":"NOT_FOUND"}]}`)) {
		t.Fatal("expected partial NOT_FOUND GraphQL data to be recognized as usable")
	}
	if hasUsablePartialGraphQLData([]byte(`{"data":{"repository":null},"errors":[{"type":"INTERNAL","message":"Something went wrong"}]}`)) {
		t.Fatal("expected non-NOT_FOUND GraphQL error not to bypass retry")
	}
	if hasUsablePartialGraphQLData([]byte(`{"data":null,"errors":[{"message":"bad credentials"}]}`)) {
		t.Fatal("expected null data to be rejected")
	}

	tmp := t.TempDir()
	callsFile := filepath.Join(tmp, "calls.txt")
	fakeGH := filepath.Join(tmp, "gh")
	script := "#!/bin/sh\necho x >> " + callsFile + "\ncat <<'EOF'\n{\"data\":{\"repository\":{\"p1\":{\"number\":73064},\"p2\":null}},\"errors\":[{\"type\":\"NOT_FOUND\",\"message\":\"Could not resolve p2\"}]}\nEOF\nexit 1\n"
	if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))

	runner := &CLIRunner{MaxAttempts: 3}
	out, err := runner.Query(context.Background(), "query { viewer { login } }", nil)
	if err != nil {
		t.Fatalf("expected partial GraphQL response to succeed without error, got: %v", err)
	}
	if !strings.Contains(string(out), `"number":73064`) {
		t.Fatalf("unexpected output: %s", string(out))
	}
	callsRaw, err := os.ReadFile(callsFile)
	if err != nil {
		t.Fatal(err)
	}
	if gotCalls := strings.Count(string(callsRaw), "x"); gotCalls != 1 {
		t.Fatalf("expected exactly 1 gh invocation (no wasted retries on partial NOT_FOUND), got %d", gotCalls)
	}
}

func TestHTTPRunnerAndEnvSelectionWithoutGHBinary(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer ghp_test_secret" {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		var reqBody struct {
			Query     string            `json:"query"`
			Variables map[string]string `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if n == 1 {
			// Simulate transient 502 on first attempt
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"rateLimit":{"limit":5000,"remaining":4800,"used":200,"resetAt":"2026-11-12T12:00:00Z"},"search":{"q":"` + reqBody.Variables["q"] + `"}}}`))
	}))
	defer ts.Close()

	// Even with empty PATH (no `gh` binary installed), DefaultRunner + CheckQuota work with GITHUB_TOKEN!
	t.Setenv("PATH", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "ghp_test_secret")

	def := DefaultRunner()
	hr, ok := def.(*HTTPRunner)
	if !ok {
		t.Fatalf("expected DefaultRunner() to return *HTTPRunner when GITHUB_TOKEN is set, got %T", def)
	}
	hr.Endpoint = ts.URL
	hr.BaseBackoff = 5 * time.Millisecond

	out, err := hr.Query(context.Background(), "query($q: String!) { search(q: $q) }", map[string]string{"q": "repo:pingcap/tidb", "empty": ""})
	if err != nil {
		t.Fatalf("HTTPRunner.Query failed: %v", err)
	}
	if !strings.Contains(string(out), `"repo:pingcap/tidb"`) || hits.Load() != 2 {
		t.Fatalf("expected retry to succeed on attempt 2, hits=%d out=%s", hits.Load(), string(out))
	}

	info, err := CheckQuota(context.Background(), hr, 100)
	if err != nil || info.Remaining != 4800 {
		t.Fatalf("CheckQuota via HTTPRunner failed: info=%+v err=%v", info, err)
	}

	// Verify 401 fails immediately without retrying
	hits.Store(0)
	badHR := NewHTTPRunner("wrong_token")
	badHR.Endpoint = ts.URL
	badHR.BaseBackoff = 5 * time.Millisecond
	if _, err := badHR.Query(context.Background(), "query { rateLimit { remaining } }", nil); err == nil {
		t.Fatal("expected 401 error for invalid token")
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 401 not to retry, got %d attempts", hits.Load())
	}

	// Verify GH_TOKEN takes precedence over GITHUB_TOKEN
	t.Setenv("GH_TOKEN", "gho_priority_token")
	if got := ResolveEnvToken(); got != "gho_priority_token" {
		t.Fatalf("expected GH_TOKEN precedence, got %q", got)
	}
}

func TestHTTPRunnerTransientGraphQLErrorsWithRateLimitData(t *testing.T) {
	var mode atomic.Int32
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 0:
			// Transient INTERNAL error alongside non-null rateLimit on attempt 1, succeeds on attempt 2
			if n == 1 {
				_, _ = w.Write([]byte(`{"data":{"rateLimit":{"limit":5000,"remaining":4999},"search":null},"errors":[{"type":"INTERNAL","message":"Something went wrong while executing your query"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":{"rateLimit":{"limit":5000,"remaining":4998},"search":{"issueCount":3}}}`))
		case 1:
			// Always transient error -> must return non-nil error after MaxAttempts (never raw, nil)
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"SERVICE_UNAVAILABLE","message":"timed out"}]}`))
		case 2:
			// Partial NOT_FOUND alias with usable data -> must NOT retry
			_, _ = w.Write([]byte(`{"data":{"repository":{"p1":{"number":1},"p2":null}},"errors":[{"type":"NOT_FOUND","message":"Could not resolve p2"}]}`))
		}
	}))
	defer ts.Close()

	hr := NewHTTPRunner("ghp_tok")
	hr.Endpoint = ts.URL
	hr.MaxAttempts = 3
	hr.BaseBackoff = 2 * time.Millisecond

	// Case 0: Retries HTTP 200 transient error even when rateLimit populated data
	out, err := hr.Query(context.Background(), "query { rateLimit { remaining } search { issueCount } }", nil)
	if err != nil || calls.Load() != 2 || !strings.Contains(string(out), `"issueCount":3`) {
		t.Fatalf("expected retry on partial-data INTERNAL error, calls=%d err=%v out=%s", calls.Load(), err, string(out))
	}

	// Case 1: Exhausted transient error returns error instead of (raw, nil)
	calls.Store(0)
	mode.Store(1)
	if _, err := hr.Query(context.Background(), "query { viewer { login } }", nil); err == nil {
		t.Fatal("expected non-nil error after exhausting transient GraphQL retries")
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 attempts on persistent transient GraphQL error, got %d", calls.Load())
	}

	// Case 2: Partial NOT_FOUND does not waste retries
	calls.Store(0)
	mode.Store(2)
	out, err = hr.Query(context.Background(), "query { repository { p1 { number } } }", nil)
	if err != nil || calls.Load() != 1 || !strings.Contains(string(out), `"number":1`) {
		t.Fatalf("expected 1 call without retry on partial NOT_FOUND, calls=%d err=%v", calls.Load(), err)
	}
}

func TestSafeRedirectDeterministicGraphQLUTF8AndQuotaEmptyGuard(t *testing.T) {
	// 1. SafeRedirectPolicy blocks cross-host, scheme downgrade, and POST->GET method rewrite
	origHTTPS, _ := http.NewRequest(http.MethodPost, "https://api.github.com/graphql", nil)
	crossHost, _ := http.NewRequest(http.MethodPost, "https://evil.example/graphql", nil)
	if err := SafeRedirectPolicy(crossHost, []*http.Request{origHTTPS}); err == nil || !strings.Contains(err.Error(), "cross-host") {
		t.Fatalf("expected cross-host redirect to be blocked, got %v", err)
	}
	downgradeReq, _ := http.NewRequest(http.MethodPost, "http://api.github.com/graphql", nil)
	if err := SafeRedirectPolicy(downgradeReq, []*http.Request{origHTTPS}); err == nil || !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("expected HTTPS->HTTP redirect downgrade to be blocked, got %v", err)
	}
	methodRewrite, _ := http.NewRequest(http.MethodGet, "https://api.github.com/v2/graphql", nil)
	if err := SafeRedirectPolicy(methodRewrite, []*http.Request{origHTTPS}); err == nil || !strings.Contains(err.Error(), "POST -> GET") {
		t.Fatalf("expected POST->GET redirect rewrite to be blocked, got %v", err)
	}
	sameHost307, _ := http.NewRequest(http.MethodPost, "https://api.github.com:443/v2/graphql", nil)
	if err := SafeRedirectPolicy(sameHost307, []*http.Request{origHTTPS}); err != nil {
		t.Fatalf("expected same-host 307 POST redirect to pass, got %v", err)
	}
	crossPort, _ := http.NewRequest(http.MethodPost, "https://api.github.com:8443/v2/graphql", nil)
	if err := SafeRedirectPolicy(crossPort, []*http.Request{origHTTPS}); err == nil || !strings.Contains(err.Error(), "cross-port") {
		t.Fatalf("expected same-hostname cross-port redirect to be blocked with cross-port error, got %v", err)
	}
	origHTTP, _ := http.NewRequest(http.MethodGet, "http://hub.internal/api/status", nil)
	upgradeHTTPS, _ := http.NewRequest(http.MethodGet, "https://hub.internal:443/api/status", nil)
	if err := SafeRedirectPolicy(upgradeHTTPS, []*http.Request{origHTTP}); err != nil {
		t.Fatalf("expected same-authority HTTP->HTTPS upgrade redirect to pass, got %v", err)
	}
	downgradeHop2, _ := http.NewRequest(http.MethodGet, "http://hub.internal/api/status", nil)
	if err := SafeRedirectPolicy(downgradeHop2, []*http.Request{origHTTP, upgradeHTTPS}); err == nil || !strings.Contains(err.Error(), "insecure") {
		t.Fatalf("expected second-hop HTTPS->HTTP downgrade to be blocked, got %v", err)
	}
	origHTTPPort, _ := http.NewRequest(http.MethodGet, "http://hub.internal:8787/api/status", nil)
	upgradeDiffPort, _ := http.NewRequest(http.MethodGet, "https://hub.internal:8443/api/status", nil)
	if err := SafeRedirectPolicy(upgradeDiffPort, []*http.Request{origHTTPPort}); err == nil || !strings.Contains(err.Error(), "cross-port") {
		t.Fatalf("expected HTTP:8787 -> HTTPS:8443 cross-port redirect to be blocked, got %v", err)
	}

	// 2. Deterministic HTTP 200 GraphQL error fails fast (1 attempt), while secondary rate limit retries
	var calls atomic.Int32
	var secondaryLimit atomic.Bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if secondaryLimit.Load() {
			_, _ = w.Write([]byte(`{"data":null,"errors":[{"message":"You have exceeded a secondary rate limit. Please wait a few minutes."}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"data":null,"errors":[{"type":"FORBIDDEN","message":"Resource not accessible by personal access token"}]}`))
	}))
	defer ts.Close()

	hr := NewHTTPRunner("ghp_tok")
	hr.Endpoint = ts.URL
	hr.MaxAttempts = 3
	hr.BaseBackoff = 2 * time.Millisecond

	if _, err := hr.Query(context.Background(), "query { viewer { login } }", nil); err == nil {
		t.Fatal("expected deterministic GraphQL error")
	}
	if calls.Load() != 1 {
		t.Fatalf("expected deterministic GraphQL FORBIDDEN error not to retry, got %d calls", calls.Load())
	}

	calls.Store(0)
	secondaryLimit.Store(true)
	if _, err := hr.Query(context.Background(), "query { viewer { login } }", nil); err == nil {
		t.Fatal("expected secondary rate limit error")
	}
	if calls.Load() != 3 {
		t.Fatalf("expected secondary rate limit GraphQL error to retry 3 times, got %d calls", calls.Load())
	}

	// 3. truncateSnippet preserves UTF-8 rune boundaries and handles <=0 maxLen
	gotUTF8 := truncateSnippet([]byte("分拣中心数据库超时错误"), 4)
	if !utf8.ValidString(gotUTF8) || gotUTF8 != "分拣中心..." {
		t.Fatalf("expected valid UTF-8 rune truncation '分拣中心...', got %q", gotUTF8)
	}
	if truncateSnippet([]byte("hello"), 0) != "" || truncateSnippet([]byte("hello"), -3) != "" {
		t.Fatal("expected empty snippet when maxLen <= 0")
	}

	// 4. CheckQuota rejects empty/missing rateLimit payload (Limit == 0) even when minPoints == 0
	emptyQuotaRunner := &mockRunner{
		fn: func(_ context.Context, _ string, _ map[string]string) ([]byte, error) {
			return []byte(`{"data":{}}`), nil
		},
	}
	if _, err := CheckQuota(context.Background(), emptyQuotaRunner, 0); err == nil {
		t.Fatal("expected CheckQuota to reject empty rateLimit response with Limit==0")
	}

	// 5. CLIRunner preserves context.DeadlineExceeded when canceled by deadline
	tmp := t.TempDir()
	fakeGH := filepath.Join(tmp, "gh")
	sleepScript := "#!/bin/sh\nexec sleep 5\n"
	if err := os.WriteFile(fakeGH, []byte(sleepScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	tctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	cliRunner := &CLIRunner{MaxAttempts: 1, BaseBackoff: 5 * time.Millisecond}
	_, dErr := cliRunner.Query(tctx, "query { viewer { login } }", nil)
	if !errors.Is(dErr, context.DeadlineExceeded) {
		t.Fatalf("expected CLIRunner timeout error to wrap context.DeadlineExceeded, got %v", dErr)
	}
}
