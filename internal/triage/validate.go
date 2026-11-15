package triage

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/winoros/tidb-triage-hub/internal/model"
	"github.com/winoros/tidb-triage-hub/internal/prstatus"
)

type ValidateOptions struct {
	SkipTodo       bool
	AllowEmptyNote bool
	StrictNotes    bool
}

type ValidationWarning struct {
	Issue   int    `json:"issue"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

func IsTodoNote(n *model.LocalizedNote) bool {
	if n == nil {
		return false
	}
	zh := strings.ToUpper(strings.TrimSpace(n.ZhCN))
	en := strings.ToUpper(strings.TrimSpace(n.En))
	return strings.HasPrefix(zh, "TODO:") ||
		strings.HasPrefix(zh, "TODO ") ||
		zh == "TODO" ||
		strings.HasPrefix(en, "TODO:") ||
		strings.HasPrefix(en, "TODO ") ||
		en == "TODO"
}

func isTodoNote(n *model.LocalizedNote) bool {
	return IsTodoNote(n)
}

// ValidateSingleNoteSyntax rejects placeholder TODO text and unmatched backticks on any note write path.
func ValidateSingleNoteSyntax(num int, note *model.LocalizedNote) error {
	if note == nil || note.IsEmpty() {
		return nil
	}
	if IsTodoNote(note) {
		return fmt.Errorf("issue #%d: `note` still contains `TODO:` placeholder", num)
	}
	zh := strings.TrimSpace(note.ZhCN)
	en := strings.TrimSpace(note.En)
	if strings.Count(zh, "`")%2 != 0 {
		return fmt.Errorf("issue #%d: `note.zh-CN` has unmatched backtick (`)", num)
	}
	if strings.Count(en, "`")%2 != 0 {
		return fmt.Errorf("issue #%d: `note.en` has unmatched backtick (`)", num)
	}
	return nil
}

func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func normalizeIssueKey(k string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(k), "#"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid issue number key %q", k)
	}
	return n, nil
}

// ValidateAndPrepareDecisions validates and normalizes batch triage decisions, enforcing
// pairwise mutual exclusion across `new_issues`, `exclude_issues`, `overrides`, `notes`, and `ack_retriage`
// (auto-resolving only untouched `TODO:` template entries in `new_issues` when moved to `exclude_issues`).
func ValidateAndPrepareDecisions(
	dec model.BatchTriageDecisions,
	opts ValidateOptions,
) (model.BatchTriageDecisions, []ValidationWarning, error) {
	out := model.BatchTriageDecisions{
		NewIssues:     make(map[string]model.TriagePatchRequest),
		ExcludeIssues: make(map[string]string),
		Overrides:     make(map[string]model.TriagePatchRequest),
		Notes:         make(map[string]model.NotePatchRequest),
		ManualPRLinks: make(map[string][]string),
	}
	if rawBase := strings.TrimSpace(dec.BaseExportedAt); rawBase != "" {
		norm, ok := model.NormalizeRFC3339NanoFixed(rawBase)
		if !ok {
			return out, nil, fmt.Errorf("invalid `base_exported_at` timestamp %q (expected RFC3339 UTC)", dec.BaseExportedAt)
		}
		out.BaseExportedAt = norm
	}

	excludedByNum := make(map[int]string)
	for k, reason := range dec.ExcludeIssues {
		num, err := normalizeIssueKey(k)
		if err != nil {
			return out, nil, err
		}
		if prevKey, dup := excludedByNum[num]; dup {
			return out, nil, fmt.Errorf("duplicate issue #%d in `exclude_issues` (%q and %q)", num, prevKey, k)
		}
		key := strconv.Itoa(num)
		out.ExcludeIssues[key] = strings.TrimSpace(reason)
		excludedByNum[num] = k
	}

	var warnings []ValidationWarning

	validateNote := func(num int, cat string, isBorderline bool, note *model.LocalizedNote, requireNote bool, forceComplete bool) error {
		if cat != "" && isBorderline && !model.IsRealBugCategory(cat) {
			warnings = append(warnings, ValidationWarning{
				Issue:   num,
				Field:   "is_borderline_kept",
				Message: fmt.Sprintf("`is_borderline_kept` should only be true on EASY/MEDIUM/HARD bugs (got %s); auto-cleared", cat),
			})
		}
		allowEmpty := opts.AllowEmptyNote && !forceComplete
		if note == nil || note.IsEmpty() {
			if requireNote && !allowEmpty {
				return fmt.Errorf("issue #%d: bilingual `note` (`zh-CN` and `en`) is required (pass --allow-empty-note to bypass)", num)
			}
			return nil
		}
		if isTodoNote(note) {
			return fmt.Errorf("issue #%d: `note` still contains `TODO:` placeholder (complete the note or pass --skip-todo)", num)
		}

		zh := strings.TrimSpace(note.ZhCN)
		en := strings.TrimSpace(note.En)
		if !allowEmpty && (zh == "" || en == "") {
			return fmt.Errorf("issue #%d: both `note.zh-CN` and `note.en` must be non-empty", num)
		}
		if strings.Count(zh, "`")%2 != 0 {
			return fmt.Errorf("issue #%d: `note.zh-CN` has unmatched backtick (`)", num)
		}
		if strings.Count(en, "`")%2 != 0 {
			return fmt.Errorf("issue #%d: `note.en` has unmatched backtick (`)", num)
		}
		if zh != "" && en != "" && zh == en && !allowEmpty {
			return fmt.Errorf("issue #%d: `note.zh-CN` and `note.en` must not be identical", num)
		}
		if zh != "" && !containsHan(zh) && !allowEmpty {
			return fmt.Errorf("issue #%d: `note.zh-CN` must contain Chinese characters", num)
		}

		zhLen := utf8.RuneCountInString(zh)
		enLen := utf8.RuneCountInString(en)
		if cat == "" || model.IsRealBugCategory(cat) {
			if zh != "" && (zhLen < 12 || zhLen > 140) {
				warnings = append(warnings, ValidationWarning{
					Issue:   num,
					Field:   "note.zh-CN",
					Message: fmt.Sprintf("recommended length is 25-85 chars (got %d)", zhLen),
				})
			}
			if en != "" && (enLen < 25 || enLen > 260) {
				warnings = append(warnings, ValidationWarning{
					Issue:   num,
					Field:   "note.en",
					Message: fmt.Sprintf("recommended length is 50-170 chars (got %d)", enLen),
				})
			}
		}
		return nil
	}

	seenNew := make(map[int]string)
	skippedNewTodoNums := make(map[int]bool)
	for k, patch := range dec.NewIssues {
		num, err := normalizeIssueKey(k)
		if err != nil {
			return out, nil, err
		}
		if prevKey, dup := seenNew[num]; dup {
			return out, nil, fmt.Errorf("duplicate issue #%d in `new_issues` (%q and %q)", num, prevKey, k)
		}
		seenNew[num] = k
		if _, isExcluded := excludedByNum[num]; isExcluded {
			// Auto-resolve untouched pending template entry when moved to `exclude_issues`
			if patch.Note == nil || patch.Note.IsEmpty() || isTodoNote(patch.Note) {
				continue
			}
			return out, nil, fmt.Errorf("issue #%d appears in both `new_issues` (with completed note) and `exclude_issues`; keep only one", num)
		}
		if isTodoNote(patch.Note) {
			if opts.SkipTodo {
				skippedNewTodoNums[num] = true
				continue
			}
			return out, nil, fmt.Errorf("issue #%d: `note` still contains `TODO:` placeholder (complete the note or pass --skip-todo)", num)
		}

		if rawIf := strings.TrimSpace(patch.IfUnmodifiedSince); rawIf != "" {
			norm, ok := model.NormalizeRFC3339NanoFixed(rawIf)
			if !ok {
				return out, nil, fmt.Errorf("issue #%d: invalid `if_unmodified_since` timestamp %q (expected RFC3339 UTC)", num, patch.IfUnmodifiedSince)
			}
			patch.IfUnmodifiedSince = norm
		}
		patch.Severity = model.NormalizeSeverityForCategory(patch.Cat, patch.Severity, "MAJOR")
		if err := model.ValidateTriageFields(patch.Cat, patch.Severity, patch.Subsystem); err != nil {
			return out, nil, fmt.Errorf("issue #%d: %w", num, err)
		}
		if err := validateNote(num, patch.Cat, patch.IsBorderlineKept, patch.Note, true, false); err != nil {
			return out, nil, err
		}
		if !model.IsRealBugCategory(patch.Cat) {
			patch.IsBorderlineKept = false
		}
		out.NewIssues[strconv.Itoa(num)] = patch
	}

	seenOvr := make(map[int]string)
	for k, patch := range dec.Overrides {
		num, err := normalizeIssueKey(k)
		if err != nil {
			return out, nil, err
		}
		if prevKey, dup := seenOvr[num]; dup {
			return out, nil, fmt.Errorf("duplicate issue #%d in `overrides` (%q and %q)", num, prevKey, k)
		}
		seenOvr[num] = k
		if _, isExcluded := excludedByNum[num]; isExcluded {
			return out, nil, fmt.Errorf("issue #%d appears in both `overrides` and `exclude_issues`; remove one", num)
		}
		if _, inNew := seenNew[num]; inNew {
			return out, nil, fmt.Errorf("issue #%d appears in both `new_issues` and `overrides`; keep only one", num)
		}
		if isTodoNote(patch.Note) {
			return out, nil, fmt.Errorf("override #%d: `note` still contains `TODO:` placeholder", num)
		}
		if rawIf := strings.TrimSpace(patch.IfUnmodifiedSince); rawIf != "" {
			norm, ok := model.NormalizeRFC3339NanoFixed(rawIf)
			if !ok {
				return out, nil, fmt.Errorf("override #%d: invalid `if_unmodified_since` timestamp %q (expected RFC3339 UTC)", num, patch.IfUnmodifiedSince)
			}
			patch.IfUnmodifiedSince = norm
		}
		patch.Severity = model.NormalizeSeverityForCategory(patch.Cat, patch.Severity, "MODERATE")
		if err := model.ValidateTriageFields(patch.Cat, patch.Severity, patch.Subsystem); err != nil {
			return out, nil, fmt.Errorf("override #%d: %w", num, err)
		}
		requireNote := patch.Note != nil
		if err := validateNote(num, patch.Cat, patch.IsBorderlineKept, patch.Note, requireNote, false); err != nil {
			return out, nil, err
		}
		if !model.IsRealBugCategory(patch.Cat) {
			patch.IsBorderlineKept = false
		}
		out.Overrides[strconv.Itoa(num)] = patch
	}

	seenNotes := make(map[int]string)
	for k, np := range dec.Notes {
		num, err := normalizeIssueKey(k)
		if err != nil {
			return out, nil, err
		}
		if prevKey, dup := seenNotes[num]; dup {
			return out, nil, fmt.Errorf("duplicate issue #%d in `notes` (%q and %q)", num, prevKey, k)
		}
		seenNotes[num] = k
		if _, isExcluded := excludedByNum[num]; isExcluded {
			return out, nil, fmt.Errorf("issue #%d appears in both `notes` and `exclude_issues`; remove one", num)
		}
		if _, inNew := seenNew[num]; inNew {
			return out, nil, fmt.Errorf("issue #%d appears in both `new_issues` and `notes`; keep only one", num)
		}
		if _, inOvr := seenOvr[num]; inOvr {
			return out, nil, fmt.Errorf("issue #%d appears in both `overrides` and `notes`; keep only one", num)
		}
		noteObj := np.LocalizedNote()
		if isTodoNote(noteObj) {
			if opts.SkipTodo {
				continue
			}
			return out, nil, fmt.Errorf("issue #%d: `notes` entry still contains `TODO:` placeholder (complete the note or pass --skip-todo)", num)
		}
		if rawIf := strings.TrimSpace(np.IfUnmodifiedSince); rawIf != "" {
			norm, ok := model.NormalizeRFC3339NanoFixed(rawIf)
			if !ok {
				return out, nil, fmt.Errorf("notes #%d: invalid `if_unmodified_since` timestamp %q (expected RFC3339 UTC)", num, np.IfUnmodifiedSince)
			}
			np.IfUnmodifiedSince = norm
		}
		if err := validateNote(num, "", false, noteObj, true, true); err != nil {
			return out, nil, err
		}
		np.ZhCN = strings.TrimSpace(np.ZhCN)
		np.En = strings.TrimSpace(np.En)
		out.Notes[strconv.Itoa(num)] = np
	}

	seenManual := make(map[int]string)
	for k, specs := range dec.ManualPRLinks {
		num, err := normalizeIssueKey(k)
		if err != nil {
			return out, nil, err
		}
		if prevKey, dup := seenManual[num]; dup {
			return out, nil, fmt.Errorf("duplicate issue #%d in `manual_pr_links` (%q and %q)", num, prevKey, k)
		}
		seenManual[num] = k
		if _, isExcluded := excludedByNum[num]; isExcluded {
			return out, nil, fmt.Errorf("issue #%d appears in both `manual_pr_links` and `exclude_issues`", num)
		}
		var cleaned []string
		for _, rawSpec := range specs {
			s := strings.TrimSpace(rawSpec)
			if s == "" {
				continue
			}
			if _, err := prstatus.ParsePRSpecs(s, "pingcap/tidb"); err != nil {
				return out, nil, fmt.Errorf("manual_pr_links #%d: %w", num, err)
			}
			cleaned = append(cleaned, s)
		}
		if len(cleaned) > 0 {
			if skippedNewTodoNums[num] {
				continue
			}
			out.ManualPRLinks[strconv.Itoa(num)] = cleaned
		}
	}

	seenAck := make(map[int]bool, len(dec.AckRetriage))
	for _, num := range dec.AckRetriage {
		if num <= 0 {
			return out, nil, fmt.Errorf("invalid issue number %d in `ack_retriage`", num)
		}
		if seenAck[num] {
			continue
		}
		if _, isExcluded := excludedByNum[num]; isExcluded {
			return out, nil, fmt.Errorf("issue #%d appears in both `ack_retriage` and `exclude_issues`", num)
		}
		if _, inNew := seenNew[num]; inNew {
			return out, nil, fmt.Errorf("issue #%d appears in both `new_issues` and `ack_retriage`", num)
		}
		if _, inOvr := seenOvr[num]; inOvr {
			return out, nil, fmt.Errorf("issue #%d appears in both `overrides` and `ack_retriage`; `overrides` already resets the re-triage baseline", num)
		}
		if _, inNotes := seenNotes[num]; inNotes {
			return out, nil, fmt.Errorf("issue #%d appears in both `notes` and `ack_retriage`; use `overrides` to update note while acknowledging re-triage", num)
		}
		seenAck[num] = true
		out.AckRetriage = append(out.AckRetriage, num)
	}

	if opts.StrictNotes && len(warnings) > 0 {
		w := warnings[0]
		return out, warnings, fmt.Errorf("strict note check failed on issue #%d (%s): %s", w.Issue, w.Field, w.Message)
	}

	return out, warnings, nil
}
