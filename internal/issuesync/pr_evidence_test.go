package issuesync

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestTimelineCherryPickRequiresCompleteNegativeEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, labels string
		known, cp    bool
	}{
		{"omitted", `null`, false, false},
		{"truncated", `{"nodes":[],"pageInfo":{"hasNextPage":true}}`, false, false},
		{"complete", `{"nodes":[],"pageInfo":{"hasNextPage":false}}`, true, false},
		{"positive", `{"nodes":[{"name":"type/cherry-pick"}],"pageInfo":{"hasNextPage":true}}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var issue gqlFullIssueNode
			raw := fmt.Sprintf(`{"number":1,"timelineItems":{"nodes":[{"source":{"__typename":"PullRequest","number":2,"title":"fix planner","state":"OPEN","baseRefName":"master","headRefOid":"head","baseRefOid":"base","labels":%s,"repository":{"nameWithOwner":"pingcap/tidb"}}}]}}`, tc.labels)
			if err := json.Unmarshal([]byte(raw), &issue); err != nil {
				t.Fatal(err)
			}
			refs := issue.toLiveUpdate().TimelinePRs
			if len(refs) != 1 || refs[0].CherryPickKnown != tc.known || refs[0].IsCherryPick != tc.cp || refs[0].HeadOID != "head" || refs[0].BaseOID != "base" {
				t.Fatalf("wrong PR evidence: %+v", refs)
			}
		})
	}
}
