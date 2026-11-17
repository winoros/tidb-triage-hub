package model

import "strings"

// MergePRObservation is the common metadata policy for enrichment and persistence.
// Complete status observations own equal-time ties. Partial observations cannot
// replace conflicting known commits at the same time, except when MERGED
// establishes a strict terminal ordering.
// The bool reports whether incoming fields were accepted.
func MergePRObservation(cur, incoming PRStatus, complete, fullStatus bool) (PRStatus, bool) {
	next := cur
	stale := cur.Repo != "" && (PRObservationIsStale(cur, incoming) ||
		(!fullStatus && !(incoming.State == "MERGED" && cur.State != "MERGED") && cur.UpdatedAt == incoming.UpdatedAt && ((cur.HeadOID != "" && incoming.HeadOID != "" && cur.HeadOID != incoming.HeadOID) ||
			(cur.BaseOID != "" && incoming.BaseOID != "" && cur.BaseOID != incoming.BaseOID))))
	if !stale && complete && !fullStatus && cur.UpdatedAt == incoming.UpdatedAt && cur.State == incoming.State {
		if next.HeadOID == "" {
			next.HeadOID = incoming.HeadOID
		}
		if next.BaseOID == "" {
			next.BaseOID = incoming.BaseOID
		}
		if next.Title == "" {
			next.Title = incoming.Title
		}
		if !next.CherryPickKnown && (incoming.CherryPickKnown || incoming.IsCherryPick) {
			next.CherryPickKnown = true
			next.IsCherryPick = incoming.IsCherryPick
		}
		return next, true
	}
	if !stale {
		if fullStatus || cur.Repo == "" {
			next = incoming
		}
		if incoming.State != "" && incoming.State != PRStateUnknown {
			next.State = incoming.State
		}
		if incoming.Title != "" {
			next.Title = incoming.Title
		}
		if incoming.URL != "" {
			next.URL = incoming.URL
		}
		if incoming.Label != "" {
			next.Label = incoming.Label
		}
		if incoming.UpdatedAt != "" {
			next.UpdatedAt = incoming.UpdatedAt
		} else {
			next.UpdatedAt = cur.UpdatedAt
		}
		if incoming.CherryPickKnown || incoming.IsCherryPick {
			next.IsCherryPick = incoming.IsCherryPick
		} else {
			next.IsCherryPick = cur.IsCherryPick
		}
		if incoming.HeadOID != "" {
			next.HeadOID = incoming.HeadOID
		} else {
			next.HeadOID = cur.HeadOID
		}
		if incoming.BaseOID != "" {
			next.BaseOID = incoming.BaseOID
		} else {
			next.BaseOID = cur.BaseOID
		}
		if next.State != "MERGED" {
			next.MergedAt = ""
		} else if incoming.MergedAt != "" {
			next.MergedAt = incoming.MergedAt
		} else {
			next.MergedAt = cur.MergedAt
		}
		next.CherryPickKnown = incoming.CherryPickKnown || incoming.IsCherryPick || cur.CherryPickKnown
	}
	if next.State == "MERGED" && next.MergedAt == "" && incoming.State == "MERGED" {
		next.MergedAt = incoming.MergedAt
	}

	next.Repo, next.Num = incoming.Repo, incoming.Num
	return next, !stale
}

// PRDetailMatches checks file evidence independently of metadata freshness.
// Known commit conflicts always invalidate evidence, even with partial identities.
func PRDetailMatches(d PendingPRDetail, p LinkedPRRef) bool {
	if (d.HeadOID != "" && p.HeadOID != "" && d.HeadOID != p.HeadOID) ||
		(d.BaseOID != "" && p.BaseOID != "" && d.BaseOID != p.BaseOID) {
		return false
	}
	if d.HeadOID != "" && d.BaseOID != "" && p.HeadOID != "" && p.BaseOID != "" {
		return true
	}
	return d.State == p.State && d.UpdatedAt == p.UpdatedAt
}

// PRDetailFresh includes mutable prose, which can change without a new commit.
func PRDetailFresh(d PendingPRDetail, p LinkedPRRef) bool {
	return PRDetailMatches(d, p) && d.UpdatedAt == p.UpdatedAt
}

// ForFacts reuses matching files but never presents outdated prose as current
// evidence. Retain the fetched timestamp so enrichment knows prose needs refresh.
func (d PendingPRDetail) ForFacts(p LinkedPRRef) (PendingPRDetail, bool) {
	if !PRDetailMatches(d, p) {
		return PendingPRDetail{}, false
	}
	if !PRDetailFresh(d, p) {
		d.BodyExcerpt = ""
	}
	d.State, d.Title, d.IsCherryPick, d.MergedAt = p.State, p.Title, p.IsCherryPick, p.MergedAt
	return d, true
}

func (p LinkedPRRef) Facts() PRStatus {
	return PRStatus{Repo: p.Repo, Num: p.Num, State: p.State, Title: p.Title, Label: p.Label, URL: p.URL,
		IsCherryPick: p.IsCherryPick, CherryPickKnown: p.CherryPickKnown, UpdatedAt: p.UpdatedAt, MergedAt: p.MergedAt, HeadOID: p.HeadOID, BaseOID: p.BaseOID}
}

func (d PendingPRDetail) Facts() PRStatus {
	return PRStatus{Repo: d.Repo, Num: d.Num, State: d.State, Title: d.Title,
		IsCherryPick: d.IsCherryPick, CherryPickKnown: d.CherryPickKnown, UpdatedAt: d.UpdatedAt, MergedAt: d.MergedAt, HeadOID: d.HeadOID, BaseOID: d.BaseOID}
}

// WithFacts preserves the issue-specific relationship and acknowledgement.
func (p LinkedPRRef) WithFacts(st PRStatus) LinkedPRRef {
	p.State, p.Title, p.IsCherryPick = st.State, st.Title, st.IsCherryPick
	p.CherryPickKnown = st.CherryPickKnown
	p.UpdatedAt, p.MergedAt, p.HeadOID, p.BaseOID = st.UpdatedAt, st.MergedAt, st.HeadOID, st.BaseOID
	if p.State == "CLOSED" {
		p.AckAt = ""
	}
	return p
}

// PRObservationIsStale preserves terminal merges and orders dated observations.
func PRObservationIsStale(cur, next PRStatus) bool {
	curState, curUpdAt, nextState, nextUpdAt := cur.State, cur.UpdatedAt, next.State, next.UpdatedAt
	curState = strings.ToUpper(strings.TrimSpace(curState))
	nextState = strings.ToUpper(strings.TrimSpace(nextState))
	curUpdAt = strings.TrimSpace(curUpdAt)
	nextUpdAt = strings.TrimSpace(nextUpdAt)

	curKnown := curState != "" && curState != PRStateUnknown
	nextKnown := nextState != "" && nextState != PRStateUnknown
	if !nextKnown {
		return curKnown
	}
	if !curKnown {
		return false
	}
	// GitHub PR MERGED state is strictly terminal and cannot regress to OPEN, DRAFT, or CLOSED.
	if curState == "MERGED" && nextState != "MERGED" {
		return true
	}
	if nextState == "MERGED" && curState != "MERGED" {
		return false
	}
	if nextUpdAt == "" {
		return curUpdAt != ""
	}
	if curUpdAt == "" {
		return false
	}
	return CompareRFC3339(nextUpdAt, curUpdAt) < 0
}

// AssessmentMatches says whether CI/review/owner fields still describe these facts.
// A timestamp change can represent label or review activity even on the same commit.
func (p PRStatus) AssessmentMatches(next PRStatus) bool {
	return p.State == next.State && p.UpdatedAt == next.UpdatedAt &&
		p.HeadOID == next.HeadOID && p.BaseOID == next.BaseOID && p.IsCherryPick == next.IsCherryPick
}

// WithoutAssessment keeps metadata while removing values computed from a full
// GitHub response. No stale CI/review/owner values can leak into a new fact snapshot.
func (p PRStatus) WithoutAssessment() PRStatus {
	return PRStatus{Repo: p.Repo, Num: p.Num, Label: p.Label, URL: p.URL, Title: p.Title,
		Author: p.Author, CreatedAt: p.CreatedAt, State: p.State, IsDraft: p.State == "DRAFT",
		IsCherryPick: p.IsCherryPick, CherryPickKnown: p.CherryPickKnown, UpdatedAt: p.UpdatedAt,
		MergedAt: p.MergedAt, HeadOID: p.HeadOID, BaseOID: p.BaseOID, TrackSources: p.TrackSources}
}
