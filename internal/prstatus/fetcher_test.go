package prstatus

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestFetchedCherryPickEvidenceCompleteness(t *testing.T) {
	for _, tc := range []struct {
		raw   string
		known bool
	}{
		{`{"number":2,"title":"fix planner","state":"OPEN","labels":{"nodes":[],"pageInfo":{"hasNextPage":true}}}`, false},
		{`{"number":2,"title":"fix planner","state":"OPEN","labels":{"nodes":[],"pageInfo":{"hasNextPage":false}}}`, true},
	} {
		var node gqlPRNode
		if err := json.Unmarshal([]byte(tc.raw), &node); err != nil {
			t.Fatal(err)
		}
		status := ClassifyPR(toRawPRInput("pingcap/tidb", nil, node), time.Now())
		if status.CherryPickKnown != tc.known || status.IsCherryPick {
			t.Fatalf("wrong evidence: %+v", status)
		}
	}
}

func TestExplicitNullIsUnavailableButOmittedPRIsFetchFailure(t *testing.T) {
	target := []PRTarget{{Repo: "pingcap/tidb", Number: 42}}
	rows, stats, err := FetchAndClassifyBatchWithRunner(context.Background(), responseRunner{payload: `{"data":{"repository":{"p42":null}}}`}, target, 20)
	if err != nil || len(rows) != 0 || len(stats.UnavailablePRs) != 1 || stats.UnavailablePRs[0].Number != 42 {
		t.Fatalf("null lookup result: rows=%v stats=%+v err=%v", rows, stats, err)
	}
	_, stats, err = FetchAndClassifyBatchWithRunner(context.Background(), responseRunner{payload: `{"data":{"repository":{}}}`}, target, 20)
	if err == nil || len(stats.UnavailablePRs) != 0 {
		t.Fatalf("partial response misclassified as unavailable: stats=%+v err=%v", stats, err)
	}
}

type responseRunner struct{ payload string }

func (r responseRunner) Query(context.Context, string, map[string]string) ([]byte, error) {
	return []byte(r.payload), nil
}
