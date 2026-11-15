package triage

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/issuesync"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
)

const prFilesFragment = `
fragment PRFileEnrichFields on PullRequest {
  number
  title
  state
  isDraft
  updatedAt
  mergedAt
  baseRefName
  additions
  deletions
  changedFiles
  body
  author { login }
  files(first: 30) {
    nodes {
      path
      additions
      deletions
    }
  }
}
`

var htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)

func isTestOrFixtureFile(path string) bool {
	p := strings.ToLower(strings.TrimSpace(path))
	if p == "" {
		return true
	}
	return strings.HasSuffix(p, "_test.go") ||
		strings.Contains(p, "/testdata/") ||
		strings.HasPrefix(p, "testdata/") ||
		strings.HasSuffix(p, ".result") ||
		strings.HasSuffix(p, ".out") ||
		strings.HasSuffix(p, ".test") ||
		strings.Contains(p, "/tests/")
}

func cleanPRBodyExcerpt(body string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = 420
	}
	cleaned := htmlCommentRe.ReplaceAllString(body, "")
	lines := strings.Split(cleaned, "\n")
	var kept []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(trimmed, "- [") ||
			strings.HasPrefix(lower, "### release note") ||
			strings.HasPrefix(lower, "### what problem") ||
			strings.HasPrefix(lower, "### what is changed") ||
			strings.HasPrefix(lower, "### check list") ||
			trimmed == "None" || trimmed == "`None`" {
			continue
		}
		kept = append(kept, trimmed)
	}
	joined := strings.Join(kept, " ")
	runes := []rune(joined)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes]) + "..."
	}
	return joined
}

type gqlPREnrichNode struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	State        string `json:"state"`
	IsDraft      bool   `json:"isDraft"`
	UpdatedAt    string `json:"updatedAt"`
	MergedAt     string `json:"mergedAt"`
	BaseRefName  string `json:"baseRefName"`
	Additions    int    `json:"additions"`
	Deletions    int    `json:"deletions"`
	ChangedFiles int    `json:"changedFiles"`
	Body         string `json:"body"`
	Author       *struct {
		Login string `json:"login"`
	} `json:"author"`
	Files struct {
		Nodes []struct {
			Path      string `json:"path"`
			Additions int    `json:"additions"`
			Deletions int    `json:"deletions"`
		} `json:"nodes"`
	} `json:"files"`
}

// FetchPendingPRDetails batch-fetches changed file paths and line counts for non-cherry-pick PRs.
func FetchPendingPRDetails(
	ctx context.Context,
	runner ghgql.Runner,
	tracker *ghgql.CostTracker,
	prRefs []model.LinkedPRRef,
) (map[string]model.PendingPRDetail, error) {
	if len(prRefs) == 0 {
		return map[string]model.PendingPRDetail{}, nil
	}
	if runner == nil {
		runner = ghgql.DefaultRunner()
	}

	byRepo := make(map[string][]int)
	seen := make(map[string]bool)
	for _, p := range prRefs {
		if p.IsCherryPick || p.Num <= 0 || !issuesync.IsKernelRepo(p.Repo) {
			continue
		}
		k := model.PRKey(p.Repo, p.Num)
		if seen[k] {
			continue
		}
		seen[k] = true
		byRepo[p.Repo] = append(byRepo[p.Repo], p.Num)
	}
	if len(byRepo) == 0 {
		return map[string]model.PendingPRDetail{}, nil
	}

	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	out := make(map[string]model.PendingPRDetail, len(seen))
	const batchSize = 15

	for _, repo := range repos {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) != 2 {
			continue
		}
		nums := byRepo[repo]
		sort.Slice(nums, func(i, j int) bool { return nums[i] > nums[j] })
		for i := 0; i < len(nums); i += batchSize {
			if err := ctx.Err(); err != nil {
				return out, err
			}
			end := i + batchSize
			if end > len(nums) {
				end = len(nums)
			}
			chunk := nums[i:end]
			var aliases strings.Builder
			for _, n := range chunk {
				fmt.Fprintf(&aliases, "    p%d: pullRequest(number: %d) { ...PRFileEnrichFields }\n", n, n)
			}
			gql := fmt.Sprintf(
				"%s\nquery {\n  rateLimit { limit cost remaining used resetAt }\n  repository(owner: %q, name: %q) {\n%s  }\n}",
				prFilesFragment, parts[0], parts[1], aliases.String(),
			)
			rawBytes, err := runner.Query(ctx, gql, nil)
			if err != nil {
				return out, err
			}
			var resp struct {
				Data *struct {
					RateLimit  *ghgql.RateLimitNode        `json:"rateLimit"`
					Repository map[string]*gqlPREnrichNode `json:"repository"`
				} `json:"data"`
				Errors []struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(rawBytes, &resp); err != nil {
				return out, fmt.Errorf("decode PR enrich response: %w", err)
			}
			if resp.Data != nil && tracker != nil {
				tracker.Observe(resp.Data.RateLimit)
			}
			for _, ge := range resp.Errors {
				isNotFound := strings.EqualFold(ge.Type, "NOT_FOUND") ||
					(ge.Type == "" && strings.Contains(ge.Message, "Could not resolve to a"))
				if !isNotFound {
					return out, fmt.Errorf("graphql PR enrich error (%s): %s", ge.Type, ge.Message)
				}
			}
			if resp.Data == nil {
				if len(resp.Errors) > 0 {
					return out, fmt.Errorf("graphql PR enrich error: %s", resp.Errors[0].Message)
				}
				return out, fmt.Errorf("graphql PR enrich returned empty data payload")
			}
			if resp.Data.Repository == nil {
				if len(resp.Errors) > 0 {
					continue
				}
				return out, fmt.Errorf("graphql PR enrich returned null repository without NOT_FOUND error")
			}

			for _, n := range chunk {
				node := resp.Data.Repository[fmt.Sprintf("p%d", n)]
				if node == nil || node.Number <= 0 {
					continue
				}
				author := "ghost"
				if node.Author != nil && node.Author.Login != "" {
					author = node.Author.Login
				}
				state := node.State
				if state == "OPEN" && node.IsDraft {
					state = "DRAFT"
				}
				isCP := prstatus.IsCherryPickPR(node.Title, nil, node.BaseRefName)

				var (
					nonTestFiles []string
					nonTestAdd   int
					nonTestDel   int
					testCnt      int
					changedStats []model.PRFileStat
				)
				for _, f := range node.Files.Nodes {
					if f.Path == "" {
						continue
					}
					changedStats = append(changedStats, model.PRFileStat{
						Path:      f.Path,
						Additions: f.Additions,
						Deletions: f.Deletions,
					})
					if isTestOrFixtureFile(f.Path) {
						testCnt++
					} else {
						nonTestFiles = append(nonTestFiles, f.Path)
						nonTestAdd += f.Additions
						nonTestDel += f.Deletions
					}
				}

				det := model.PendingPRDetail{
					Repo:              repo,
					Num:               node.Number,
					Title:             node.Title,
					State:             state,
					UpdatedAt:         node.UpdatedAt,
					MergedAt:          node.MergedAt,
					Author:            author,
					BaseRefName:       node.BaseRefName,
					IsCherryPick:      isCP,
					Additions:         node.Additions,
					Deletions:         node.Deletions,
					ChangedFilesCount: node.ChangedFiles,
					FilesTruncated:    node.ChangedFiles > len(node.Files.Nodes),
					NonTestFileCount:  len(nonTestFiles),
					NonTestAdditions:  nonTestAdd,
					NonTestDeletions:  nonTestDel,
					NonTestFiles:      nonTestFiles,
					TestFileCount:     testCnt,
					ChangedFiles:      changedStats,
					BodyExcerpt:       cleanPRBodyExcerpt(node.Body, 420),
				}
				out[model.PRKey(repo, node.Number)] = det
			}
		}
	}

	return out, nil
}
