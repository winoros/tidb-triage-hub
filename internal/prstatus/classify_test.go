package prstatus

import (
	"testing"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

func TestClassifyPR_TiDBQuirksAndPriority(t *testing.T) {
	now := time.Date(2026, 11, 12, 12, 0, 0, 0, time.UTC)

	t.Run("ignores coderabbitai fake APPROVED and tide PENDING, detects needs-ok-to-test", func(t *testing.T) {
		got := ClassifyPR(RawPRInput{
			Repo:         "pingcap/tidb",
			Number:       72986,
			Title:        "statistics: bound global multi-valued index NDV",
			Author:       "idle-lab",
			State:        "OPEN",
			Mergeable:    "MERGEABLE",
			CreatedAt:    "2026-11-10T04:00:00Z",
			UpdatedAt:    "2026-11-11T09:45:31Z",
			LastCommitAt: "2026-11-11T04:09:03Z",
			Labels:       []string{"contribution", "sig/planner", "size/L", "needs-ok-to-test"},
			OpinionatedReviews: []RawReview{
				{Author: "coderabbitai", State: "APPROVED", SubmittedAt: "2026-11-11T04:08:26Z"},
			},
			Checks: []RawCheckItem{
				{Typename: "StatusContext", Name: "tide", Status: "PENDING"},
				{Typename: "CheckRun", Name: "CodeRabbit", Status: "COMPLETED", Conclusion: "SKIPPED"},
				{Typename: "StatusContext", Name: "license/cla", Status: "SUCCESS"},
			},
		}, now)

		if got.BlockerStage != model.StageNeedsOKToTest {
			t.Fatalf("expected %s, got %s", model.StageNeedsOKToTest, got.BlockerStage)
		}
		if got.BallOwner != model.BallReviewer {
			t.Fatalf("expected ball %s, got %s", model.BallReviewer, got.BallOwner)
		}
		if len(got.ApprovedBy) != 0 || len(got.ReviewedBy) != 0 {
			t.Fatalf("expected bot reviews excluded, got approved=%v reviewed=%v", got.ApprovedBy, got.ReviewedBy)
		}
		if got.RunningChecksCount != 0 {
			t.Fatalf("expected tide excluded from running checks, got %d", got.RunningChecksCount)
		}
	})

	t.Run("zero human review despite coderabbitai approval", func(t *testing.T) {
		got := ClassifyPR(RawPRInput{
			Repo:         "pingcap/tidb",
			Number:       73064,
			Title:        "planner: preserve residual filters",
			Author:       "winoros",
			State:        "OPEN",
			Mergeable:    "MERGEABLE",
			CreatedAt:    "2026-11-08T11:30:52Z",
			LastCommitAt: "2026-11-08T11:30:52Z",
			Labels:       []string{"sig/planner", "size/S"},
			OpinionatedReviews: []RawReview{
				{Author: "coderabbitai", State: "APPROVED", SubmittedAt: "2026-11-08T11:36:23Z"},
			},
			Checks: []RawCheckItem{
				{Typename: "StatusContext", Name: "tide", Status: "PENDING"},
				{Typename: "StatusContext", Name: "idc-jenkins-ci-tidb/unit-test", Status: "PENDING"},
			},
		}, now)

		if got.BlockerStage != model.StageWaitingReview || got.SubReason != "zero_review" {
			t.Fatalf("expected WAITING_REVIEWER/zero_review, got %s/%s", got.BlockerStage, got.SubReason)
		}
		if got.RunningChecksCount != 1 {
			t.Fatalf("expected 1 running check, got %d", got.RunningChecksCount)
		}
		if got.WaitingDays != 4 {
			t.Fatalf("expected 4 waiting days, got %d", got.WaitingDays)
		}
	})

	t.Run("changes requested vs author pushed newer commit flips ball to reviewer re-review", func(t *testing.T) {
		base := RawPRInput{
			Repo:         "pingcap/tidb",
			Number:       71000,
			Title:        "planner: fix decorrelation",
			Author:       "alice",
			State:        "OPEN",
			Mergeable:    "MERGEABLE",
			CreatedAt:    "2026-11-01T00:00:00Z",
			LastCommitAt: "2026-11-05T10:00:00Z",
			OpinionatedReviews: []RawReview{
				{Author: "hawkingrei", State: "CHANGES_REQUESTED", SubmittedAt: "2026-11-06T10:00:00Z"},
			},
			Checks: []RawCheckItem{
				{Typename: "StatusContext", Name: "tide", Status: "PENDING"},
			},
		}
		got := ClassifyPR(base, now)
		if got.BlockerStage != model.StageWaitingAuthor || got.BallOwner != model.BallAuthor {
			t.Fatalf("expected WAITING_AUTHOR/AUTHOR, got %s/%s", got.BlockerStage, got.BallOwner)
		}

		// Now author pushes a commit AFTER the review
		base.LastCommitAt = "2026-11-07T08:00:00Z"
		gotAfter := ClassifyPR(base, now)
		if gotAfter.BlockerStage != model.StageWaitingReview || gotAfter.SubReason != "re_review" {
			t.Fatalf("expected WAITING_REVIEWER/re_review after new commit, got %s/%s", gotAfter.BlockerStage, gotAfter.SubReason)
		}
	})

	t.Run("LGT1 label without approved maps to NEEDS_LGTM2", func(t *testing.T) {
		got := ClassifyPR(RawPRInput{
			Repo:      "pingcap/tidb",
			Number:    72978,
			Title:     "planner: optimize join order",
			Author:    "bob",
			State:     "OPEN",
			Mergeable: "MERGEABLE",
			Labels:    []string{"sig/planner", "lgtm", "size/XXL"},
			RecentComments: []RawComment{
				{Author: "time-and-fate", Body: "/lgtm", CreatedAt: "2026-11-11T08:00:00Z"},
			},
			Checks: []RawCheckItem{
				{Typename: "StatusContext", Name: "tide", Status: "PENDING"},
			},
		}, now)
		if got.BlockerStage != model.StageNeedsLGTM2 || got.BallOwner != model.BallReviewer {
			t.Fatalf("expected NEEDS_LGTM2/REVIEWER, got %s/%s", got.BlockerStage, got.BallOwner)
		}
		if len(got.LGTMBy) != 1 || got.LGTMBy[0] != "time-and-fate" {
			t.Fatalf("expected LGTMBy=[time-and-fate], got %v", got.LGTMBy)
		}
	})

	t.Run("approved + lgtm blocked by do-not-merge/needs-tests-checked maps to GATE_BLOCKED", func(t *testing.T) {
		got := ClassifyPR(RawPRInput{
			Repo:      "pingcap/tidb",
			Number:    73019,
			Title:     "planner: fix rule",
			Author:    "carol",
			State:     "OPEN",
			Mergeable: "MERGEABLE",
			Labels:    []string{"sig/planner", "approved", "lgtm", "do-not-merge/needs-tests-checked"},
			Checks: []RawCheckItem{
				{Typename: "StatusContext", Name: "tide", Status: "PENDING"},
			},
		}, now)
		if got.BlockerStage != model.StageGateBlocked {
			t.Fatalf("expected GATE_BLOCKED, got %s", got.BlockerStage)
		}
	})

	t.Run("CI failure extracts shortened failed check names and URLs", func(t *testing.T) {
		got := ClassifyPR(RawPRInput{
			Repo:      "pingcap/tidb",
			Number:    72500,
			Title:     "planner: range fix",
			Author:    "dave",
			State:     "OPEN",
			Mergeable: "MERGEABLE",
			Labels:    []string{"sig/planner", "lgtm"},
			Checks: []RawCheckItem{
				{Typename: "StatusContext", Name: "tide", Status: "PENDING"},
				{Typename: "StatusContext", Name: "idc-jenkins-ci-tidb/unit-test", Status: "FAILURE", URL: "https://do.pingcap.net/1"},
				{Typename: "CheckRun", Name: "Check Bazel Prepare", Status: "COMPLETED", Conclusion: "FAILURE", URL: "https://github.com/actions/2"},
			},
		}, now)
		if got.BlockerStage != model.StageCIFailed || len(got.FailedChecks) != 2 {
			t.Fatalf("expected CI_FAILED with 2 checks, got %s (%v)", got.BlockerStage, got.FailedChecks)
		}
		if got.FailedChecks[1].Name != "unit-test" {
			t.Fatalf("expected shortened name unit-test, got %s", got.FailedChecks[1].Name)
		}
	})

	t.Run("approved + lgtm + green checks maps to READY_MERGING", func(t *testing.T) {
		got := ClassifyPR(RawPRInput{
			Repo:      "pingcap/tidb",
			Number:    72600,
			Title:     "planner: ready pr",
			Author:    "eve",
			State:     "OPEN",
			Mergeable: "MERGEABLE",
			Labels:    []string{"sig/planner", "approved", "lgtm"},
			Checks: []RawCheckItem{
				{Typename: "StatusContext", Name: "tide", Status: "PENDING"},
				{Typename: "StatusContext", Name: "idc-jenkins-ci-tidb/unit-test", Status: "SUCCESS"},
			},
		}, now)
		if got.BlockerStage != model.StageReadyMerging || got.BallOwner != model.BallMergeQueue {
			t.Fatalf("expected READY_MERGING/MERGE_QUEUE, got %s/%s", got.BlockerStage, got.BallOwner)
		}
	})
}


func TestParsePRSpecs(t *testing.T) {
	got, err := ParsePRSpecs("73064, #72862, pingcap/tiflash#11155, #73064", "pingcap/tidb")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 deduplicated targets, got %d (%+v)", len(got), got)
	}
	if got[0].Repo != "pingcap/tidb" || got[0].Number != 73064 {
		t.Fatalf("unexpected target[0]: %+v", got[0])
	}
	if got[1].Repo != "pingcap/tidb" || got[1].Number != 72862 {
		t.Fatalf("unexpected target[1]: %+v", got[1])
	}
	if got[2].Repo != "pingcap/tiflash" || got[2].Number != 11155 {
		t.Fatalf("unexpected target[2]: %+v", got[2])
	}

	if _, err := ParsePRSpecs("not-a-pr", "pingcap/tidb"); err == nil {
		t.Fatal("expected error on invalid PR spec")
	}
}
