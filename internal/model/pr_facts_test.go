package model

import "testing"

func TestObservationKnowledgeFollowsAcceptedFacts(t *testing.T) {
	current := PRStatus{Repo: "pingcap/tidb", Num: 42, State: "OPEN", UpdatedAt: "2026-11-17T00:00:00Z"}
	stale := current
	stale.UpdatedAt = "2026-11-16T00:00:00Z"
	stale.CherryPickKnown = true
	stale.IsCherryPick = true
	got, applied := MergePRObservation(current, stale, true, false)
	if applied || got.CherryPickKnown || got.IsCherryPick {
		t.Fatalf("rejected source changed knowledge: %+v", got)
	}
	supplement := current
	supplement.HeadOID = "head"
	supplement.BaseOID = "base"
	supplement.CherryPickKnown = true
	got, applied = MergePRObservation(current, supplement, true, false)
	if !applied || got.HeadOID != "head" || got.BaseOID != "base" || !got.CherryPickKnown {
		t.Fatalf("missing facts not complemented: %+v", got)
	}
	conflict := supplement
	conflict.HeadOID = "other"
	next, applied := MergePRObservation(got, conflict, true, false)
	if applied || next.HeadOID != "head" {
		t.Fatalf("equal-time conflicting evidence replaced known commit: %+v", next)
	}
}

func TestTerminalMergeOrdersSameTimestampCommitConflicts(t *testing.T) {
	for _, state := range []string{"OPEN", "DRAFT", "CLOSED"} {
		for _, full := range []bool{false, true} {
			current := PRStatus{Repo: "pingcap/tidb", Num: 42, State: state, UpdatedAt: "2026-11-17T00:00:00Z", HeadOID: "before", BaseOID: "base"}
			merged := current
			merged.State, merged.HeadOID, merged.MergedAt = "MERGED", "after", current.UpdatedAt
			next, applied := MergePRObservation(current, merged, true, full)
			if !applied || next.State != "MERGED" || next.HeadOID != "after" || next.MergedAt == "" {
				t.Fatalf("terminal transition rejected: %+v", next)
			}
			back, applied := MergePRObservation(next, current, false, full)
			if applied || back.State != "MERGED" || back.HeadOID != "after" {
				t.Fatalf("terminal transition reversed: %+v", back)
			}
		}
	}
}
