package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/winoros/tidb-triage-hub/internal/ghgql"
	"github.com/winoros/tidb-triage-hub/internal/httputil"
	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/syncer"
)

const sqliteHeaderMagic = "SQLite format 3\x00"

type remoteConfig struct {
	BaseURL string
	Token   string
}

func bindRemoteFlags(fs *flag.FlagSet) *remoteConfig {
	cfg := &remoteConfig{}
	fs.StringVar(&cfg.BaseURL, "remote", strings.TrimSpace(os.Getenv("TRIAGE_HUB_URL")), "Remote triage-hub server URL (env: TRIAGE_HUB_URL)")
	fs.StringVar(&cfg.Token, "token", strings.TrimSpace(os.Getenv("TRIAGE_HUB_TOKEN")), "Maintainer bearer token for remote server (env: TRIAGE_HUB_TOKEN)")
	return cfg
}

func (c *remoteConfig) enabled() bool {
	return c != nil && strings.TrimSpace(c.BaseURL) != ""
}

func envOrDefault(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envBoolDefault(key string, def bool) bool {
	return model.ParseBoolDefault(os.Getenv(key), def)
}

func envDurationDefault(key string) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("[warn] ignoring invalid duration in %s=%q: %v", key, v, err)
		return 0
	}
	return d
}

func isLoopbackListenAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	host = strings.TrimSpace(host)
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type remoteClient struct {
	baseURL    string
	parsedBase *url.URL
	token      string
	reqTimeout time.Duration
	httpCli    *http.Client
}

func newRemoteClient(cfg remoteConfig, timeout time.Duration) (*remoteClient, error) {
	u, err := httputil.ParseBaseURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}

	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	return &remoteClient{
		baseURL:    u.String(),
		parsedBase: u,
		token:      strings.TrimSpace(cfg.Token),
		reqTimeout: timeout,
		httpCli: &http.Client{
			CheckRedirect: ghgql.SafeRedirectPolicy,
		},
	}, nil
}

func (rc *remoteClient) ensureContextDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, hasDeadline := ctx.Deadline(); hasDeadline {
		return ctx, func() {}
	}
	d := rc.reqTimeout
	if d <= 0 {
		d = 2 * time.Minute
	}
	const pad = 15 * time.Second
	if d <= math.MaxInt64-pad {
		d += pad
	} else {
		d = math.MaxInt64
	}
	return context.WithTimeout(ctx, d)
}

func (rc *remoteClient) doJSON(ctx context.Context, method, subpath string, query url.Values, reqBody any, dst any) error {
	reqCtx, cancel := rc.ensureContextDeadline(ctx)
	defer cancel()

	endpoint := httputil.JoinEndpoint(rc.parsedBase, subpath, query)

	var bodyReader io.Reader
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	} else if method != http.MethodGet && method != http.MethodHead {
		bodyReader = strings.NewReader("{}")
	}

	req, err := http.NewRequestWithContext(reqCtx, method, endpoint, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if bodyReader != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if rc.token != "" {
		req.Header.Set("Authorization", "Bearer "+rc.token)
	}
	if rc.reqTimeout > 0 {
		req.Header.Set("X-Triage-Timeout-Ms", strconv.FormatInt(rc.reqTimeout.Milliseconds(), 10))
	}

	resp, err := rc.httpCli.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()

	rawResp, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("read response from %s: %w", endpoint, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errPayload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(rawResp, &errPayload) == nil && errPayload.Error != "" {
			return fmt.Errorf("remote HTTP %d: %s", resp.StatusCode, errPayload.Error)
		}
		return fmt.Errorf("remote HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rawResp)))
	}

	if dst != nil {
		if err := json.Unmarshal(rawResp, dst); err != nil {
			return fmt.Errorf("decode response JSON from %s: %w", endpoint, err)
		}
	}
	return nil
}

func (rc *remoteClient) fetchStatus(ctx context.Context, checkGH bool) (map[string]any, error) {
	q := url.Values{}
	q.Set("require_auth", "1")
	if checkGH {
		q.Set("check_gh", "1")
	}
	var out map[string]any
	if err := rc.doJSON(ctx, http.MethodGet, "/api/status", q, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (rc *remoteClient) syncPRs(ctx context.Context, prSpec string, minQuota int) (any, error) {
	body := map[string]any{
		"include_state": false,
	}
	if strings.TrimSpace(prSpec) != "" {
		body["pr"] = strings.TrimSpace(prSpec)
	}
	if minQuota >= 0 {
		body["min_quota"] = minQuota
	}
	var resp struct {
		Stats any `json:"stats"`
	}
	if err := rc.doJSON(ctx, http.MethodPost, "/api/sync/prs", nil, body, &resp); err != nil {
		return nil, err
	}
	return resp.Stats, nil
}

func (rc *remoteClient) syncIssues(ctx context.Context, opts syncer.IssueSyncOptions) (syncer.IssueSyncResult, error) {
	reqBody := struct {
		syncer.IssueSyncOptions
		IncludeState bool `json:"include_state"`
	}{
		IssueSyncOptions: opts,
		IncludeState:     false,
	}
	var resp struct {
		Result syncer.IssueSyncResult `json:"result"`
	}
	if err := rc.doJSON(ctx, http.MethodPost, "/api/sync/issues", nil, reqBody, &resp); err != nil {
		return syncer.IssueSyncResult{}, err
	}
	return resp.Result, nil
}

func (rc *remoteClient) fetchPendingBundle(ctx context.Context, opts syncer.PendingBundleOptions) (map[string]any, int, error) {
	q := url.Values{}
	if opts.OnlyNum > 0 {
		q.Set("number", strconv.Itoa(opts.OnlyNum))
	}
	if len(opts.Numbers) > 0 {
		q.Set("numbers", joinIntsCSV(opts.Numbers))
	}
	if strings.TrimSpace(opts.ScopeFilter) != "" {
		q.Set("scope", strings.TrimSpace(opts.ScopeFilter))
	}
	if opts.OnlyPending {
		q.Set("only_pending", "1")
	}
	if opts.OnlyRetriage {
		q.Set("only_retriage", "1")
	}
	if len(opts.ReTriageNums) > 0 {
		q.Set("re_triage", joinIntsCSV(opts.ReTriageNums))
	}
	if opts.BackfillNotes > 0 {
		q.Set("backfill_notes", strconv.Itoa(opts.BackfillNotes))
	}
	if strings.TrimSpace(opts.CatFilter) != "" {
		q.Set("cat", strings.TrimSpace(opts.CatFilter))
	}
	if opts.Compact {
		q.Set("compact", "1")
	}
	if !opts.EnrichPRs {
		q.Set("enrich_prs", "0")
	}

	var bundle map[string]any
	if err := rc.doJSON(ctx, http.MethodGet, "/api/triage/pending", q, nil, &bundle); err != nil {
		return nil, 0, err
	}
	cnt := intFromAny(bundle["pending_count"]) + intFromAny(bundle["retriage_count"]) + intFromAny(bundle["backfill_count"])
	return bundle, cnt, nil
}

func (rc *remoteClient) postTriageDecisions(ctx context.Context, req syncer.BatchTriageExecuteRequest) (map[string]any, error) {
	var out map[string]any
	if err := rc.doJSON(ctx, http.MethodPost, "/api/triage/decisions", nil, req, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (rc *remoteClient) downloadBackup(ctx context.Context, destPath string) (int64, error) {
	reqCtx, cancel := rc.ensureContextDeadline(ctx)
	defer cancel()

	destPath = strings.TrimSpace(destPath)
	if destPath == "" {
		return 0, fmt.Errorf("backup output path is empty")
	}
	destDir := filepath.Dir(destPath)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}

	endpoint := httputil.JoinEndpoint(rc.parsedBase, "/api/admin/backup.db", nil)
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	if rc.token != "" {
		req.Header.Set("Authorization", "Bearer "+rc.token)
	}
	if rc.reqTimeout > 0 {
		req.Header.Set("X-Triage-Timeout-Ms", strconv.FormatInt(rc.reqTimeout.Milliseconds(), 10))
	}

	resp, err := rc.httpCli.Do(req)
	if err != nil {
		return 0, fmt.Errorf("GET %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		rawErr, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var errPayload struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(rawErr, &errPayload) == nil && errPayload.Error != "" {
			return 0, fmt.Errorf("remote backup HTTP %d: %s", resp.StatusCode, errPayload.Error)
		}
		return 0, fmt.Errorf("remote backup HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(rawErr)))
	}

	tmpFile, err := os.CreateTemp(destDir, ".triage-backup-*.tmp")
	if err != nil {
		return 0, err
	}
	tmpPath := tmpFile.Name()
	cleanupTmp := func() { _ = os.Remove(tmpPath) }

	n, copyErr := io.Copy(tmpFile, resp.Body)
	if copyErr != nil {
		_ = tmpFile.Close()
		cleanupTmp()
		return 0, copyErr
	}
	if n < 100 {
		_ = tmpFile.Close()
		cleanupTmp()
		return 0, fmt.Errorf("downloaded backup file too small (%d bytes)", n)
	}
	var hdr [16]byte
	if _, rErr := tmpFile.ReadAt(hdr[:], 0); rErr != nil || string(hdr[:]) != sqliteHeaderMagic {
		_ = tmpFile.Close()
		cleanupTmp()
		return 0, fmt.Errorf("downloaded backup is not a valid SQLite database (unexpected header)")
	}
	if closeErr := tmpFile.Close(); closeErr != nil {
		cleanupTmp()
		return 0, closeErr
	}
	_ = os.Chmod(tmpPath, 0o600)

	if err := os.Rename(tmpPath, destPath); err != nil {
		cleanupTmp()
		return 0, err
	}
	_ = os.Remove(destPath + "-wal")
	_ = os.Remove(destPath + "-shm")
	return n, nil
}

func joinIntsCSV(nums []int) string {
	parts := make([]string, 0, len(nums))
	for _, n := range nums {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, ",")
}

func intFromAny(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	default:
		return 0
	}
}

func hasCommittedRemoteMutations(out map[string]any) bool {
	if out == nil {
		return false
	}
	for _, k := range []string{"accepted_issues", "excluded_issues", "overridden_issues", "acknowledged_issues"} {
		if arr, ok := out[k].([]any); ok && len(arr) > 0 {
			return true
		}
	}
	return intFromAny(out["manual_prs_linked"]) > 0 || intFromAny(out["synced_new_prs"]) > 0
}
