package ghgql

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
