package issuesync

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
)

var (
	localIssueRefRe     = regexp.MustCompile(`#(\d{4,6})\b`)
	crossTiDBIssueRefRe = regexp.MustCompile(`(?i)(?:pingcap/tidb#|github\.com/pingcap/tidb/issues/)(\d{4,6})\b`)
)

func isIdentOrPathByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_' || b == '/' || b == '-'
}

// ExtractTiDBIssueRefsFromText parses TiDB issue numbers referenced in text using RE2-safe boundary checks.
func ExtractTiDBIssueRefsFromText(repo string, text string) []int {
	seen := make(map[int]bool)

	for _, m := range crossTiDBIssueRefRe.FindAllStringSubmatch(text, -1) {
		if len(m) >= 2 {
			if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
				seen[n] = true
			}
		}
	}

	if repo == "pingcap/tidb" {
		for _, idx := range localIssueRefRe.FindAllStringSubmatchIndex(text, -1) {
			if len(idx) < 4 {
				continue
			}
			start := idx[0]
			if start > 0 && isIdentOrPathByte(text[start-1]) {
				continue
			}
			if n, err := strconv.Atoi(text[idx[2]:idx[3]]); err == nil && n > 0 {
				seen[n] = true
			}
		}
	}

	if len(seen) == 0 {
		return nil
	}
	out := make([]int, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

func BuildLinkedPRRef(repo string, num int, title string, rawState string, isDraft bool, url string, updatedAt string) model.LinkedPRRef {
	return BuildLinkedPRRefWithMeta(repo, num, title, rawState, isDraft, url, updatedAt, nil, "", "")
}

func BuildLinkedPRRefWithMeta(
	repo string,
	num int,
	title string,
	rawState string,
	isDraft bool,
	url string,
	updatedAt string,
	labels []string,
	baseRefName string,
	source string,
) model.LinkedPRRef {
	state := strings.ToUpper(strings.TrimSpace(rawState))
	if state == "OPEN" && isDraft {
		state = "DRAFT"
	}
	if url == "" {
		url = fmt.Sprintf("https://github.com/%s/pull/%d", repo, num)
	}
	return model.LinkedPRRef{
		Num:          num,
		Repo:         repo,
		Label:        prstatus.FormatPRLabel(repo, num),
		URL:          url,
		State:        state,
		Title:        title,
		IsCherryPick: prstatus.IsCherryPickPR(title, labels, baseRefName),
		UpdatedAt:    updatedAt,
		Source:       source,
	}
}

func SortLinkedPRRefs(prs []model.LinkedPRRef) {
	rank := func(p model.LinkedPRRef) int {
		switch {
		case p.State == "OPEN" && !p.IsCherryPick:
			return 0
		case p.State == "DRAFT" && !p.IsCherryPick:
			return 1
		case p.State == model.PRStateUnknown && !p.IsCherryPick:
			return 2
		case p.State == "MERGED" && !p.IsCherryPick:
			return 3
		case p.State == "OPEN" && p.IsCherryPick:
			return 4
		case p.State == "DRAFT" && p.IsCherryPick:
			return 5
		case p.State == model.PRStateUnknown && p.IsCherryPick:
			return 6
		case p.State == "MERGED" && p.IsCherryPick:
			return 7
		case p.State == "CLOSED" && !p.IsCherryPick:
			return 8
		case p.State == "CLOSED" && p.IsCherryPick:
			return 9
		default:
			return 10
		}
	}
	sort.Slice(prs, func(i, j int) bool {
		ri, rj := rank(prs[i]), rank(prs[j])
		if ri != rj {
			return ri < rj
		}
		return prs[i].Num > prs[j].Num
	})
}
