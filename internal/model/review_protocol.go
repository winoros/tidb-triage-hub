package model

import (
	"fmt"
	"strconv"
	"strings"
)

func normalizeReviewKeys[V any](input map[string]V) (map[string]V, error) {
	if input == nil {
		return nil, nil
	}
	out := make(map[string]V, len(input))
	for key, value := range input {
		n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(key), "#"))
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("invalid issue number key %q", key)
		}
		canonical := strconv.Itoa(n)
		if _, exists := out[canonical]; exists {
			return nil, fmt.Errorf("duplicate issue #%d", n)
		}
		out[canonical] = value
	}
	return out, nil
}

// NormalizeBatchReviewProtocol is the compatibility boundary. Any explicit
// version opts the whole batch into CAS; legacy timestamps never coexist with
// version guards internally. All keys are canonical before store planning.
func NormalizeBatchReviewProtocol(dec BatchTriageDecisions) (BatchTriageDecisions, error) {
	var err error
	if dec.NewIssues, err = normalizeReviewKeys(dec.NewIssues); err != nil {
		return dec, err
	}
	if dec.Overrides, err = normalizeReviewKeys(dec.Overrides); err != nil {
		return dec, err
	}
	if dec.Notes, err = normalizeReviewKeys(dec.Notes); err != nil {
		return dec, err
	}
	if dec.ExcludeIssues, err = normalizeReviewKeys(dec.ExcludeIssues); err != nil {
		return dec, err
	}
	if dec.ManualPRLinks, err = normalizeReviewKeys(dec.ManualPRLinks); err != nil {
		return dec, err
	}
	if dec.ExpectedVersions, err = normalizeReviewKeys(dec.ExpectedVersions); err != nil {
		return dec, err
	}
	add := func(key string, v *ReviewVersion) error {
		if v == nil {
			return nil
		}
		if dec.ExpectedVersions == nil {
			dec.ExpectedVersions = make(map[string]ReviewVersion)
		}
		if old, exists := dec.ExpectedVersions[key]; exists && old != *v {
			return fmt.Errorf("issue #%s: expected_version disagrees with expected_versions", key)
		}
		dec.ExpectedVersions[key] = *v
		return nil
	}
	for key, p := range dec.NewIssues {
		if err := add(key, p.ExpectedVersion); err != nil {
			return dec, err
		}
	}
	for key, p := range dec.Overrides {
		if err := add(key, p.ExpectedVersion); err != nil {
			return dec, err
		}
	}
	for key, p := range dec.Notes {
		if err := add(key, p.ExpectedVersion); err != nil {
			return dec, err
		}
	}
	if dec.ExpectedVersions == nil {
		return dec, nil
	}
	for key, v := range dec.ExpectedVersions {
		if v.Review <= 0 || v.Notes <= 0 {
			return dec, fmt.Errorf("issue #%s: expected_versions requires positive review and notes counters", key)
		}
	}
	check := func(key string) error {
		if _, exists := dec.ExpectedVersions[key]; !exists {
			return fmt.Errorf("issue #%s: missing expected_versions entry; export and review this issue before adding it to the batch", key)
		}
		return nil
	}
	for _, patches := range []map[string]TriagePatchRequest{dec.NewIssues, dec.Overrides} {
		for key, p := range patches {
			if err := check(key); err != nil {
				return dec, err
			}
			p.ExpectedVersion, p.IfUnmodifiedSince = nil, ""
			patches[key] = p
		}
	}
	for key, p := range dec.Notes {
		if err := check(key); err != nil {
			return dec, err
		}
		p.ExpectedVersion, p.IfUnmodifiedSince = nil, ""
		dec.Notes[key] = p
	}
	for key := range dec.ExcludeIssues {
		if err := check(key); err != nil {
			return dec, err
		}
	}
	for key := range dec.ManualPRLinks {
		if err := check(key); err != nil {
			return dec, err
		}
	}
	for _, n := range dec.AckRetriage {
		if n <= 0 {
			return dec, fmt.Errorf("invalid issue number %d", n)
		}
		if err := check(strconv.Itoa(n)); err != nil {
			return dec, err
		}
	}
	dec.BaseExportedAt = ""
	return dec, nil
}
