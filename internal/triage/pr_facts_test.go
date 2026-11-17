package triage

import (
	"context"
	"testing"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

func TestEnrichmentRejectsConflictingCachedCommits(t *testing.T) {
	for _, identity := range []string{"head", "base", "partial-head", "partial-base"} {
		t.Run(identity, func(t *testing.T) {
			ref := model.LinkedPRRef{Repo: "pingcap/tidb", Num: 12345, State: "OPEN", UpdatedAt: "2026-11-17T00:00:00Z", HeadOID: "new-head", BaseOID: "new-base"}
			detail := model.PendingPRDetail{Repo: ref.Repo, Num: ref.Num, State: ref.State, UpdatedAt: ref.UpdatedAt, HeadOID: ref.HeadOID, BaseOID: ref.BaseOID, NonTestFiles: []string{"pkg/expression/old.go"}, NonTestFileCount: 1}
			switch identity {
			case "head":
				detail.HeadOID = "old-head"
			case "base":
				detail.BaseOID = "old-base"
			case "partial-head":
				detail.HeadOID = "old-head"
				detail.BaseOID = ""
				ref.BaseOID = ""
			case "partial-base":
				detail.BaseOID = "old-base"
				detail.HeadOID = ""
				ref.HeadOID = ""
			}
			candidate := model.PendingCandidate{Number: 42, Title: "planner bug", PRs: []model.LinkedPRRef{ref}, PRDetails: []model.PendingPRDetail{detail}}
			if !CandidateNeedsPREnrichment(candidate) {
				t.Fatal("conflicting cache treated as fresh")
			}
			for _, fetch := range []bool{false, true} {
				out, err := EnrichAndAnalyzePending(context.Background(), staticRunner{payload: `{"data":null}`}, nil, []model.PendingCandidate{candidate}, fetch)
				if fetch && err == nil {
					t.Fatal("expected fetch failure")
				}
				if !fetch && err != nil {
					t.Fatal(err)
				}
				got := out[0]
				if got.PRs[0].HeadOID != ref.HeadOID || got.PRs[0].BaseOID != ref.BaseOID {
					t.Fatalf("new commits overwritten: %+v", got.PRs)
				}
				if len(got.PRDetails) != 0 {
					t.Fatal("stale files used after skipped/failed fetch")
				}
			}
		})
	}
}

func TestEnrichmentFetchesMatchingEvidenceAtSameTimestamp(t *testing.T) {
	ref := model.LinkedPRRef{Repo: "pingcap/tidb", Num: 12345, State: "OPEN", UpdatedAt: "2026-11-17T00:00:00Z", HeadOID: "new-head", BaseOID: "base"}
	candidate := model.PendingCandidate{Number: 42, Title: "planner bug", PRs: []model.LinkedPRRef{ref}, PRDetails: []model.PendingPRDetail{{Repo: ref.Repo, Num: ref.Num, State: ref.State, UpdatedAt: ref.UpdatedAt, HeadOID: "old-head", BaseOID: "base"}}}
	payload := `{"data":{"repository":{"p12345":{"number":12345,"state":"OPEN","updatedAt":"2026-11-17T00:00:00Z","headRefOid":"new-head","baseRefOid":"base","files":{"nodes":[{"path":"pkg/planner/core/new.go","additions":1}]}}}}}`
	out, err := EnrichAndAnalyzePending(context.Background(), staticRunner{payload: payload}, nil, []model.PendingCandidate{candidate}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(out[0].PRDetails) != 1 || out[0].PRDetails[0].HeadOID != "new-head" || out[0].PRDetails[0].NonTestFiles[0] != "pkg/planner/core/new.go" {
		t.Fatalf("new evidence not fetched: %+v", out[0])
	}
	if CandidateNeedsPREnrichment(out[0]) {
		t.Fatal("matching evidence not reusable")
	}
}

func TestStaleProseCannotSupplyCluesWhenRefreshIsSkippedOrFails(t *testing.T) {
	ref := model.LinkedPRRef{Repo: "pingcap/tidb", Num: 12345, State: "OPEN", UpdatedAt: "2026-11-17T01:00:00Z", HeadOID: "head", BaseOID: "base"}
	detail := model.PendingPRDetail{Repo: ref.Repo, Num: ref.Num, State: ref.State, UpdatedAt: "2026-11-17T00:00:00Z", HeadOID: ref.HeadOID, BaseOID: ref.BaseOID, NonTestFiles: []string{"pkg/planner/core/plan.go"}, BodyExcerpt: "old root cause: wrong result"}
	candidate := model.PendingCandidate{Number: 42, Title: "planner issue", PRs: []model.LinkedPRRef{ref}, PRDetails: []model.PendingPRDetail{detail}}
	for _, fetch := range []bool{false, true} {
		out, err := EnrichAndAnalyzePending(context.Background(), staticRunner{payload: `{"data":null}`}, nil, []model.PendingCandidate{candidate}, fetch)
		if (err != nil) != fetch {
			t.Fatalf("unexpected fetch error: %v", err)
		}
		got := out[0]
		if len(got.PRDetails) != 1 || len(got.PRDetails[0].NonTestFiles) != 1 || got.PRDetails[0].BodyExcerpt != "" {
			t.Fatalf("invalid evidence projection: %+v", got.PRDetails)
		}
		for _, signature := range got.Clues.ErrorSignatures {
			if signature == "wrong_result" {
				t.Fatal("stale prose supplied a clue")
			}
		}
		if !CandidateNeedsPREnrichment(got) {
			t.Fatal("stale prose was marked fresh")
		}
	}
}
