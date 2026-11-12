package prstatus

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/model"
)

const prFieldsFragment = `
fragment PRStatusFields on PullRequest {
  number
  title
  url
  state
  isDraft
  mergeable
  createdAt
  updatedAt
  author { login }
  labels(first: 30) { nodes { name } }
  reviewRequests(first: 15) {
    nodes {
      requestedReviewer {
        __typename
        ... on User { login }
        ... on Team { name }
      }
    }
  }
  latestOpinionatedReviews(first: 15) {
    nodes {
      author { login }
      state
      submittedAt
    }
  }
  reviews(last: 15) {
    nodes {
      author { login }
      state
      submittedAt
    }
  }
  reviewThreads(first: 30) {
    nodes {
      isResolved
      isOutdated
      comments(last: 1) {
        nodes {
          author { login }
          createdAt
        }
      }
    }
  }
  comments(last: 15) {
    nodes {
      author { login }
      body
      createdAt
    }
  }
  commits(last: 1) {
    nodes {
      commit {
        committedDate
        pushedDate
        statusCheckRollup {
          state
          contexts(first: 60) {
            nodes {
              __typename
              ... on CheckRun {
                name
                status
                conclusion
                detailsUrl
              }
              ... on StatusContext {
                context
                state
                targetUrl
              }
            }
          }
        }
      }
    }
  }
}
`

type PRTarget struct {
	Repo         string
	Number       int
	TrackSources []string
}

type gqlPRNode struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	URL       string `json:"url"`
	State     string `json:"state"`
	IsDraft   bool   `json:"isDraft"`
	Mergeable string `json:"mergeable"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Author    *struct {
		Login string `json:"login"`
	} `json:"author"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	ReviewRequests struct {
		Nodes []struct {
			RequestedReviewer struct {
				Typename string `json:"__typename"`
				Login    string `json:"login"`
				Name     string `json:"name"`
			} `json:"requestedReviewer"`
		} `json:"nodes"`
	} `json:"reviewRequests"`
	LatestOpinionatedReviews struct {
		Nodes []struct {
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			State       string `json:"state"`
			SubmittedAt string `json:"submittedAt"`
		} `json:"nodes"`
	} `json:"latestOpinionatedReviews"`
	Reviews struct {
		Nodes []struct {
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			State       string `json:"state"`
			SubmittedAt string `json:"submittedAt"`
		} `json:"nodes"`
	} `json:"reviews"`
	ReviewThreads struct {
		Nodes []struct {
			IsResolved bool `json:"isResolved"`
			IsOutdated bool `json:"isOutdated"`
			Comments   struct {
				Nodes []struct {
					Author *struct {
						Login string `json:"login"`
					} `json:"author"`
					CreatedAt string `json:"createdAt"`
				} `json:"nodes"`
			} `json:"comments"`
		} `json:"nodes"`
	} `json:"reviewThreads"`
	Comments struct {
		Nodes []struct {
			Author *struct {
				Login string `json:"login"`
			} `json:"author"`
			Body      string `json:"body"`
			CreatedAt string `json:"createdAt"`
		} `json:"nodes"`
	} `json:"comments"`
	Commits struct {
		Nodes []struct {
			Commit struct {
				CommittedDate     string `json:"committedDate"`
				PushedDate        string `json:"pushedDate"`
				StatusCheckRollup *struct {
					State    string `json:"state"`
					Contexts struct {
						Nodes []struct {
							Typename   string `json:"__typename"`
							Name       string `json:"name"`
							Status     string `json:"status"`
							Conclusion string `json:"conclusion"`
							DetailsURL string `json:"detailsUrl"`
							Context    string `json:"context"`
							State      string `json:"state"`
							TargetURL  string `json:"targetUrl"`
						} `json:"nodes"`
					} `json:"contexts"`
				} `json:"statusCheckRollup"`
			} `json:"commit"`
		} `json:"nodes"`
	} `json:"commits"`
}

func toRawPRInput(repo string, sources []string, n gqlPRNode) RawPRInput {
	author := "ghost"
	if n.Author != nil && n.Author.Login != "" {
		author = n.Author.Login
	}
	labels := make([]string, 0, len(n.Labels.Nodes))
	for _, l := range n.Labels.Nodes {
		if l.Name != "" {
			labels = append(labels, l.Name)
		}
	}
	reqRevs := make([]string, 0, len(n.ReviewRequests.Nodes))
	for _, rr := range n.ReviewRequests.Nodes {
		if rr.RequestedReviewer.Login != "" {
			reqRevs = append(reqRevs, rr.RequestedReviewer.Login)
		} else if rr.RequestedReviewer.Name != "" {
			reqRevs = append(reqRevs, "team:"+rr.RequestedReviewer.Name)
		}
	}
	opRevs := make([]RawReview, 0, len(n.LatestOpinionatedReviews.Nodes))
	for _, rv := range n.LatestOpinionatedReviews.Nodes {
		login := ""
		if rv.Author != nil {
			login = rv.Author.Login
		}
		opRevs = append(opRevs, RawReview{Author: login, State: rv.State, SubmittedAt: rv.SubmittedAt})
	}
	recRevs := make([]RawReview, 0, len(n.Reviews.Nodes))
	for _, rv := range n.Reviews.Nodes {
		login := ""
		if rv.Author != nil {
			login = rv.Author.Login
		}
		recRevs = append(recRevs, RawReview{Author: login, State: rv.State, SubmittedAt: rv.SubmittedAt})
	}
	threads := make([]RawReviewThread, 0, len(n.ReviewThreads.Nodes))
	for _, th := range n.ReviewThreads.Nodes {
		var cmts []RawThreadComment
		for _, c := range th.Comments.Nodes {
			login := ""
			if c.Author != nil {
				login = c.Author.Login
			}
			cmts = append(cmts, RawThreadComment{Author: login, CreatedAt: c.CreatedAt})
		}
		threads = append(threads, RawReviewThread{
			IsResolved: th.IsResolved,
			IsOutdated: th.IsOutdated,
			Comments:   cmts,
		})
	}
	cmts := make([]RawComment, 0, len(n.Comments.Nodes))
	for _, c := range n.Comments.Nodes {
		login := ""
		if c.Author != nil {
			login = c.Author.Login
		}
		cmts = append(cmts, RawComment{Author: login, Body: c.Body, CreatedAt: c.CreatedAt})
	}

	var lastCommitAt string
	var checks []RawCheckItem
	if len(n.Commits.Nodes) > 0 {
		cm := n.Commits.Nodes[len(n.Commits.Nodes)-1].Commit
		if cm.PushedDate != "" {
			lastCommitAt = cm.PushedDate
		} else {
			lastCommitAt = cm.CommittedDate
		}
		if cm.StatusCheckRollup != nil {
			for _, ctx := range cm.StatusCheckRollup.Contexts.Nodes {
				switch ctx.Typename {
				case "CheckRun":
					checks = append(checks, RawCheckItem{
						Typename:   "CheckRun",
						Name:       ctx.Name,
						Status:     ctx.Status,
						Conclusion: ctx.Conclusion,
						URL:        ctx.DetailsURL,
					})
				case "StatusContext":
					checks = append(checks, RawCheckItem{
						Typename: "StatusContext",
						Name:     ctx.Context,
						Status:   ctx.State,
						URL:      ctx.TargetURL,
					})
				}
			}
		}
	}

	return RawPRInput{
		Repo:               repo,
		Number:             n.Number,
		Title:              n.Title,
		URL:                n.URL,
		Author:             author,
		State:              n.State,
		IsDraft:            n.IsDraft,
		Mergeable:          n.Mergeable,
		CreatedAt:          n.CreatedAt,
		UpdatedAt:          n.UpdatedAt,
		LastCommitAt:       lastCommitAt,
		Labels:             labels,
		RequestedReviewers: reqRevs,
		OpinionatedReviews: opRevs,
		RecentReviews:      recRevs,
		ReviewThreads:      threads,
		RecentComments:     cmts,
		Checks:             checks,
		TrackSources:       sources,
	}
}

func runGHGraphQL(ctx context.Context, query string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		cmd := exec.CommandContext(ctx, "gh", "api", "graphql", "-f", "query="+query)
		out, err := cmd.Output()
		if err == nil {
			return out, nil
		}
		if ee, ok := err.(*exec.ExitError); ok {
			lastErr = fmt.Errorf("gh api graphql exit: %w (%s)", err, strings.TrimSpace(string(ee.Stderr)))
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 1200 * time.Millisecond):
		}
	}
	return nil, lastErr
}

type FetchStats struct {
	GraphQLCalls int `json:"graphql_calls"`
	TotalFetched int `json:"total_fetched"`
	OpenCount    int `json:"open_count"`
	ClosedCount  int `json:"closed_count"`
	DurationMs   int `json:"duration_ms"`
}

func FetchAndClassifyBatch(ctx context.Context, targets []PRTarget, batchSize int) ([]model.PRStatus, FetchStats, error) {
	start := time.Now()
	if batchSize <= 0 {
		batchSize = 20
	}
	if len(targets) == 0 {
		return nil, FetchStats{}, nil
	}

	byRepo := make(map[string][]PRTarget)
	seen := make(map[string]bool)
	for _, t := range targets {
		key := model.PRKey(t.Repo, t.Number)
		if seen[key] || t.Repo == "" || t.Number <= 0 {
			continue
		}
		seen[key] = true
		byRepo[t.Repo] = append(byRepo[t.Repo], t)
	}

	type batchJob struct {
		repo  string
		owner string
		name  string
		items []PRTarget
	}
	var jobs []batchJob
	repos := make([]string, 0, len(byRepo))
	for r := range byRepo {
		repos = append(repos, r)
	}
	sort.Strings(repos)

	for _, repo := range repos {
		parts := strings.SplitN(repo, "/", 2)
		if len(parts) != 2 {
			continue
		}
		list := byRepo[repo]
		sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
		for i := 0; i < len(list); i += batchSize {
			end := i + batchSize
			if end > len(list) {
				end = len(list)
			}
			jobs = append(jobs, batchJob{
				repo:  repo,
				owner: parts[0],
				name:  parts[1],
				items: list[i:end],
			})
		}
	}

	now := time.Now().UTC()
	var (
		mu       sync.Mutex
		results  []model.PRStatus
		firstErr error
		calls    int
	)

	sem := make(chan struct{}, 3)
	var wg sync.WaitGroup
	for _, jb := range jobs {
		jb := jb
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}

			var aliases strings.Builder
			srcByNum := make(map[int][]string, len(jb.items))
			for _, it := range jb.items {
				srcByNum[it.Number] = it.TrackSources
				fmt.Fprintf(&aliases, "    p%d: pullRequest(number: %d) { ...PRStatusFields }\n", it.Number, it.Number)
			}
			gql := fmt.Sprintf(
				"%s\nquery {\n  repository(owner: %q, name: %q) {\n%s  }\n}",
				prFieldsFragment, jb.owner, jb.name, aliases.String(),
			)

			rawBytes, err := runGHGraphQL(ctx, gql)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				mu.Unlock()
				return
			}

			var resp struct {
				Data struct {
					Repository map[string]*gqlPRNode `json:"repository"`
				} `json:"data"`
				Errors []struct {
					Message string `json:"message"`
				} `json:"errors"`
			}
			if err := json.Unmarshal(rawBytes, &resp); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("decode graphql response: %w", err)
				}
				mu.Unlock()
				return
			}
			if resp.Data.Repository == nil && len(resp.Errors) > 0 {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("graphql error: %s", resp.Errors[0].Message)
				}
				mu.Unlock()
				return
			}

			localOut := make([]model.PRStatus, 0, len(jb.items))
			for _, it := range jb.items {
				node := resp.Data.Repository[fmt.Sprintf("p%d", it.Number)]
				if node == nil {
					continue
				}
				rawIn := toRawPRInput(jb.repo, srcByNum[it.Number], *node)
				st := ClassifyPR(rawIn, now)
				localOut = append(localOut, st)
			}

			mu.Lock()
			calls++
			results = append(results, localOut...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if firstErr != nil {
		return nil, FetchStats{}, firstErr
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].Repo != results[j].Repo {
			return results[i].Repo < results[j].Repo
		}
		return results[i].Num > results[j].Num
	})

	stats := FetchStats{
		GraphQLCalls: calls,
		TotalFetched: len(results),
		DurationMs:   int(time.Since(start).Milliseconds()),
	}
	for _, r := range results {
		if r.State == "OPEN" || r.State == "DRAFT" {
			stats.OpenCount++
		} else {
			stats.ClosedCount++
		}
	}
	return results, stats, nil
}
