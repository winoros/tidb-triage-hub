SiteI18n.extend("en", {
  "common": {
    "appBrand": "TiDB Optimizer Triage Hub",
    "langSwitchAria": "Switch interface language",
    "toggleTheme": "Theme",
    "toggleThemeAria": "Toggle light or dark theme",
    "themeToLight": "Switch to light mode",
    "themeToDark": "Switch to dark mode",
    "backToTop": "Back to top"
  },
  "report": {
    "meta": {
      "title": "TiDB Optimizer Issue Triage & PR Status Hub",
      "description": "Standalone triage & PR blocker workbench for open pingcap/tidb optimizer issues with noise filtering, Fix Difficulty, and Assessed Severity matrix."
    },
    "nav": {
      "dataJson": "JSON",
      "dataJsonFull": "Open JSON Snapshot (/api/dataset.json)",
      "ghIssues": "GitHub",
      "ghPrs": "Open TiDB planner issues on GitHub",
      "toggleOverviewHide": "Hide Stats",
      "toggleOverviewShow": "Show Stats",
      "syncInfoBtn": "Sync Info"
    },
    "views": {
      "issuesTab": "Issue Triage",
      "prsTab": "PR Status"
    },
    "rw": {
      "modeBadge": "RW Mode",
      "readonlyBadge": "Read-Only",
      "pendingBtn": "Inbox",
      "syncPrsBtn": "Sync PRs",
      "syncIssuesBtn": "Sync Issues",
      "syncing": "Syncing…",
      "unlockBtn": "Unlock RW",
      "lockBtn": "Lock",
      "authModalTitle": "Unlock Maintainer RW Mode",
      "authModalHint": "Enter the server access token (TRIAGE_HUB_TOKEN) to enable triage calibration and GitHub sync buttons in this browser.",
      "authTokenPlaceholder": "Enter access token…",
      "authSubmitBtn": "Unlock",
      "authCancelBtn": "Cancel",
      "authInvalidToken": "Invalid access token",
      "editBtn": "Triage",
      "inspectBtn": "Inspect",
      "editedTag": "Calibrated",
      "forceKeepTag": "Force Kept",
      "retriageTag": "Re-triage",
      "jumpToPr": "Locate this PR in PR Status board",
      "jumpToIssue": "Locate this issue in Issue Triage board",
      "toastSaved": "Saved manual triage override for Issue #{num}",
      "toastReset": "Reverted Issue #{num} to automated baseline",
      "toastPrSynced": "Refreshed {open} open PRs ({calls} GraphQL calls in {ms}ms)",
      "toastIssueSynced": "Incremental issue sync complete ({pending} pending candidates)",
      "toastPendingResolved": "Resolved pending candidate #{num}",
      "toastPendingSyncWarn": "Resolved #{num} (live PR sync skipped: {warn})",
      "toastRetriageAcked": "Confirmed current triage for Issue #{num}",
      "toastRetriageBatchAcked": "Confirmed current triage for {count} issues",
      "toastIssueExcluded": "Excluded supplemental issue #{num} from candidate pool",
      "toastExcludedRestored": "Removed #{num} from exclusion list (will re-evaluate on next sync)",
      "retriageReasons": {
        "new_fix_pr": "New Fix PR",
        "fix_pr_merged_still_open": "Fix PR Merged (Still Open)",
        "customer_reported": "Customer Escalation",
        "gh_severity_escalated": "GH Severity Escalated",
        "label_scope_changed": "Scope Label Changed",
        "substantive_new_comments": "Substantive Comments"
      },
      "drawer": {
        "title": "Calibrate Issue #{num}",
        "inspectTitle": "Issue #{num} Triage Details",
        "close": "Close",
        "openGh": "Open on GitHub",
        "metaAuthor": "Author",
        "metaAssignees": "Assignees",
        "metaUnassigned": "Unassigned",
        "metaCreated": "Created",
        "metaUpdated": "Updated",
        "metaGhSev": "GH Severity Label",
        "metaLabels": "GitHub Labels",
        "metaLinkedPrs": "Linked PRs",
        "sectionTriage": "Classification & Ownership",
        "sectionFlags": "Retention Policy",
        "sectionNotes": "Bilingual Root-Cause & Triage Notes",
        "catLabel": "Category & Fix Difficulty",
        "sevLabel": "Assessed Severity",
        "subLabel": "Optimizer Subsystem",
        "borderlineCheck": "Mark as Borderline-Kept Real Bug",
        "borderlineHint": "Discovered via fuzzing/audit, yet reachable in realistic SQL with wrong results or crash",
        "forceKeepCheck": "Force Keep in Candidate Pool",
        "forceKeepHint": "Prevent automatic removal during incremental sync if GitHub labels change",
        "noteZhLabel": "Triage Note (zh-CN)",
        "noteEnLabel": "Triage Note (en)",
        "notePlaceholderZh": "填写中文根因与影响简述…",
        "notePlaceholderEn": "Concise root cause, trigger condition, or triage note…",
        "retriageBannerTitle": "Substantive Changes Detected — Review Recommended",
        "keepCurrentBtn": "Keep Current Triage",
        "excludeSummary": "Exclude from Candidate Pool (Supplemental Noise)",
        "excludeHint": "This non-canonical issue does not carry both sig/planner and type/bug. Exclude it if it belongs to another component and should leave the board.",
        "excludeReasonPlaceholder": "Exclusion reason (e.g. Executor/BR issue, not optimizer)",
        "excludeConfirmBtn": "Remove & Exclude Issue",
        "excludeCanonicalDisabled": "Canonical issues (sig/planner + type/bug) cannot be excluded; select a Filtered category instead.",
        "saveBtn": "Save Override",
        "saveHint": "Press Ctrl/⌘ + Enter to save",
        "resetBtn": "Reset to Baseline",
        "cancelBtn": "Cancel"
      },
      "inbox": {
        "title": "Triage & Review Inbox",
        "tabPending": "Pending ({count})",
        "tabRetriage": "Re-triage ({count})",
        "tabExcluded": "Excluded ({count})",
        "emptyTitle": "No Pending Candidates",
        "empty": "No untriaged candidate issues pending. Run `triage-hub sync-issues` via CLI to discover and triage new issues.",
        "retriageEmptyTitle": "No Issues Need Re-triage",
        "retriageEmpty": "All triaged issues match their last acknowledged PR, severity, and comment baseline.",
        "excludedEmptyTitle": "No Excluded Issues",
        "excludedEmpty": "The exclusion list is empty or no items match your search.",
        "keepAllBtn": "Keep All ({count})",
        "keepCurrentBtn": "Keep Current",
        "recalibrateBtn": "Calibrate…",
        "restoreBtn": "Restore",
        "excludedSearchPlaceholder": "Filter excluded issues by #, title, reason, or label…",
        "excludedMeta": "Excluded {date} · via {source}",
        "suggestedBadge": "Suggested ({confidence})",
        "suggestedSignals": "Signals",
        "prFilesSummary": "Mainline PR #{num}: {nonTest} non-test files (+{add}/-{del}) · {test} test",
        "canonicalExcludeTip": "Canonical issue (sig/planner + type/bug) cannot be excluded; accept as Filtered instead",
        "scopesLabel": "Matched Scopes",
        "bodySummary": "Inspect Issue Body & Recent Comments ({comments})",
        "excludeReasonPlaceholder": "Exclude reason (optional, e.g. Executor/BR issue)",
        "acceptBtn": "Accept & Save Triage",
        "excludeBtn": "Exclude Issue"
      }
    },
    "header": {
      "title": "TiDB Optimizer Open Issues & In-Flight PR Workbench",
      "subtitle": "{totalCan} canonical · {totalSup} supplemental ({totalAll} open)"
    },
    "sync": {
      "snapshotLabel": "Issue Snapshot",
      "prSnapshotLabel": "PR Sync",
      "watermarkLabel": "Watermark",
      "queryLabel": "Incremental Query",
      "copyTitle": "Click to copy GitHub incremental search qualifier",
      "copied": "Query copied",
      "deltaBadge": "Delta +{added} / -{closed}",
      "deltaTitle": "Changes Since Previous Snapshot",
      "deltaAdded": "New Open Candidates ({count})",
      "deltaClosed": "Closed & Graduated ({count})",
      "popoverTitle": "Snapshot & Incremental Sync Metadata"
    },
    "kpi": {
      "scopedTotal": {
        "label": "Scoped Candidates",
        "hint": "All currently OPEN candidates (click for all)"
      },
      "realBugs": {
        "label": "Actionable Real Bugs",
        "hint": "{pct}% of scope · E {easy} / M {med} / H {hard}"
      },
      "highSev": {
        "label": "High Severity (S0+S1)",
        "hint": "Critical S0: {crit} · Major S1: {maj}"
      },
      "prCoverage": {
        "label": "Active Fix PR Coverage",
        "hint": "{open} in-flight PR · {unclaimed} unclaimed"
      },
      "enhAndNoise": {
        "label": "Enhancements / Filtered",
        "hint": "Fuzz {fuzz} · AI {ai} · Other {oth}"
      }
    },
    "legend": {
      "easy": "Easy Bug",
      "med": "Medium Bug",
      "hard": "Hard Bug",
      "enh": "Enhancement",
      "flt": "Filtered Noise"
    },
    "matrix": {
      "title": "Real Bug Priority Matrix: Difficulty × Severity",
      "subHtml": "Click any cell to filter below · Warm highlighted cells denote <b>Easy × S0/S1 Quick Wins</b>",
      "quickWinTag": "Quick Win",
      "corner": "Difficulty \\ Severity",
      "colCrit": "S0 · Critical",
      "colMaj": "S1 · Major",
      "colMod": "S2 · Moderate",
      "colMin": "S3 · Minor",
      "colTotal": "Row Total",
      "rowEasy": "Easy",
      "rowEasySub": "Est. 1–3 days",
      "rowMed": "Medium",
      "rowMedSub": "Est. 3–7 days",
      "rowHard": "Hard",
      "rowHardSub": "Est. 1–3+ weeks",
      "rowTotal": "Severity Total",
      "cellSub": "{wait} open · {inPr} in PR",
      "unclaimedSub": "{wait} unclaimed",
      "resetSub": "Reset matrix",
      "activeBadge": "Matrix: {diff} × {sev}",
      "allDiff": "All Difficulties",
      "allSev": "All Severities",
      "clearTitle": "Clear matrix filter"
    },
    "rubric": {
      "summary": "Triage Rubric",
      "toggleHint": "Expand / Collapse",
      "scope": {
        "title": "1. Triage Scope & Retention Boundary",
        "fuzzHtml": "<b>Filtered · Random / Differential Fuzz</b>: Extreme differential-testing queries generated by SQLancer, TLP / NoREC / CERT, deeply nested exotic-type set operations, random builtin chains, or manual rule blacklists.",
        "aiHtml": "<b>Filtered · Obscure AI Corner Cases</b>: Synthetic edge cases crafted from static code audits (e.g., crafted hash collisions, extreme epoch sentinels, or non-default flags) practically unreachable in real SQL workloads.",
        "nonOptHtml": "<b>Filtered · Non-Optimizer / Fixed / Invalid</b>: Issues belonging to executor, DDL, client-go, or Dashboard UI; issues already fixed on master pending closure/backport; and flaky unit tests.",
        "keepHtml": "<b>Borderline Retention & Enhancements</b>: Fuzz-reduced or AI-discovered issues with realistic trigger paths causing <b>wrong results, deadlocks, missing locks, or crashes</b> are retained as real bugs; feature requests for new rewrite rules, access paths, or syntax with correct current behavior are classified as <b>Enhancement</b>."
      },
      "difficulty": {
        "title": "2. Fix Difficulty (Engineering Effort)",
        "easyHtml": "<b>Easy (1–3 days)</b>: Clear root cause isolated to a local guard or metadata fix in a single file (e.g., missing privilege checks, session vars in Plan Cache keys, local bound arithmetic, warning/display fixes) without altering cross-phase contracts.",
        "medHtml": "<b>Medium (3–7 days)</b>: Coordinates invariants across adjacent phases within a subsystem (e.g., logical rewrite vs. column pruning schema mapping, Ranger extraction vs. partitioned pessimistic locking, parameterization type precision, partitioned Global Stats merge).",
        "hardHtml": "<b>Hard (1–3+ weeks)</b>: Cross-module refactoring, CBO cost / out-of-range extrapolation trade-offs (with plan regression risk), <code>ANALYZE</code> memory &amp; stats-table lock ordering, MPP isolation semantics, or single-pass name resolution / <code>ONLY_FULL_GROUP_BY</code> architectural debt."
      },
      "severity": {
        "title": "3. Assessed Severity (Production Impact)",
        "critHtml": "<b>S0 · Critical</b>: Server panic/crash, <code>ANALYZE</code> or compile-time OOM, permanent background worker exit, cluster-wide GC blocked &gt;12h, silent DML data corruption, missing pessimistic <code>FOR UPDATE</code> locks, privilege bypass, or common/easily-hit <code>SELECT</code> wrong results.",
        "majHtml": "<b>S1 · Major</b>: Realistic non-edge-case <code>SELECT</code> wrong results requiring specific conditions, valid production SQL failing with internal errors (e.g., <code>ERROR 1105</code>, unresolved internal column) or stats-table deadlocks, and order-of-magnitude plan regressions on core workloads.",
        "modHtml": "<b>S2 · Moderate</b>: Corner-case / hard-to-hit wrong results retained as borderline bugs, general cardinality/cost estimation skew (suboptimal plan without catastrophic regression), Plan Cache / SPM misses, hint export/replay mismatches, or minor MySQL edge compatibility gaps.",
        "minHtml": "<b>S3 · Minor</b>: Hint warning text, <code>EXPLAIN</code> / <code>SHOW STATS</code> / slow-log / metric formatting, or peripheral diagnostic tooling polish. <i>(Where the raw GitHub <code>severity/*</code> label is missing or differs, <code>GH</code> is shown inline for reference.)</i>"
      }
    },
    "filters": {
      "searchPlaceholder": "Search Issue #, PR # (e.g. #72862), title, @author, @assignee, or triage notes…",
      "searchAria": "Search issues",
      "clearSearch": "Clear search",
      "resetAll": "Reset Filters",
      "scopeAria": "Candidate scope",
      "scope": {
        "all": "All Candidates ({count})",
        "canonical": "Canonical sig/planner + bug ({count})",
        "customer": "Customer Reports ({count})",
        "borderline": "Borderline-Kept Bugs ({count})",
        "retriage": "Needs Re-triage ({count})"
      },
      "subAria": "Filter by subsystem",
      "subAll": "All Subsystems",
      "sevAria": "Filter by severity",
      "sev": {
        "all": "All Severities",
        "high": "High S0 + S1 (Critical/Major)",
        "critical": "S0 · Critical",
        "major": "S1 · Major",
        "moderate": "S2 · Moderate",
        "minor": "S3 · Minor",
        "ghUnlabeled": "GH Unlabeled (no severity/*)"
      },
      "prAria": "Filter by PR status",
      "pr": {
        "all": "All PR States",
        "openFix": "Active Fix PR (OPEN/Draft)",
        "ballReviewer": "Active PR on Reviewer",
        "ballAuthor": "Active PR on Author / CI",
        "needsLgtm2": "Active PR at LGT1 (Needs 2nd)",
        "unclaimed": "Unclaimed (No Active Fix PR)",
        "hasAny": "Any Linked PR",
        "hasMerged": "Has Merged PR (MERGED)",
        "noPr": "No Linked PR"
      },
      "assAria": "Filter by assignee",
      "ass": {
        "all": "All Assignees",
        "unassigned": "Unassigned",
        "assigned": "Assigned"
      },
      "updWinAria": "Recent update window",
      "updWin": {
        "all": "Updated: Any Time",
        "d1": "Updated in last 24h",
        "d7": "Updated in last 7d",
        "d30": "Updated in last 30d",
        "d90": "Updated in last 90d",
        "custom": "Custom start date…"
      },
      "sincePrefix": "Since",
      "sinceSuffix": "",
      "sinceAria": "Updated since date",
      "sortAria": "Sort order",
      "sort": {
        "updatedDesc": "Sort: Last Updated ↓",
        "prioDesc": "Sort: Priority (S0→S3, Easy→Hard)",
        "numDesc": "Sort: Issue # ↓ (Newest)",
        "numAsc": "Sort: Issue # ↑ (Oldest)"
      }
    },
    "pills": {
      "groupActionable": "Actionable",
      "groupOther": "Other",
      "REAL_ALL": "Real Bugs",
      "EASY": "Easy",
      "MEDIUM": "Medium",
      "HARD": "Hard",
      "ENHANCEMENT": "Enhancement",
      "FILTER_RANDOM_FUZZ": "Filtered: Fuzz",
      "FILTER_AI_OBSCURE": "Filtered: AI Edge",
      "FILTER_NON_OPT_OR_FIXED": "Filtered: Fixed/Other",
      "RETRIAGE": "Re-triage",
      "ALL": "All"
    },
    "status": {
      "count": "Showing {matched} of {scoped} issues",
      "clickHint": "Click subsystem chip to filter · Click column header to sort"
    },
    "table": {
      "colIssue": "Issue",
      "colCatSev": "Classification",
      "colSub": "Subsystem",
      "colTitle": "Title, Triage Root-Cause & Linked PRs",
      "colTime": "Activity & Owner",
      "ghSevNote": "GH: {val}",
      "ghUnlabeled": "unlabeled",
      "tagCustomer": "Customer",
      "tagBorderline": "Borderline",
      "tagSupplemental": "Supplemental",
      "updatedPrefix": "{time}",
      "createdPrefix": "Created {date}",
      "assignedTo": "Assigned @{user}",
      "emptyTitle": "No Matching Issues Found",
      "empty": "Try broadening your filter criteria, clearing the search box, or resetting filters below."
    },
    "prBoard": {
      "summaryTitle": "Ball Ownership Distribution (Click court to filter)",
      "courts": {
        "REVIEWER": "Reviewer's Court",
        "REVIEWER_sub": "Needs review / LGT1 / ok-to-test",
        "AUTHOR": "Author's Court",
        "AUTHOR_sub": "CI failed / conflict / changes req.",
        "MERGE_QUEUE": "Merge Queue",
        "MERGE_QUEUE_sub": "Approved & running final CI / Tide",
        "HOLD": "On Hold / Draft",
        "HOLD_sub": "Marked as Draft or do-not-merge/hold"
      },
      "cards": {
        "ALL": "All Active PRs",
        "NEEDS_LGTM2": "LGT1 (Needs 2nd)",
        "WAITING_REVIEWER": "Needs Review",
        "NEEDS_OK_TO_TEST": "Needs ok-to-test",
        "CI_FAILED": "CI Failed",
        "CONFLICT": "Merge Conflict",
        "WAITING_AUTHOR": "Waiting on Author",
        "GATE_BLOCKED": "Gate Blocked",
        "READY_MERGING": "Ready / Merging",
        "DRAFT_OR_HOLD": "Draft / Hold"
      },
      "filters": {
        "searchPlaceholder": "Search PR #, linked Issue #, title, @author, @reviewer, or failed CI check…",
        "scopeMainline": "Mainline Open PRs ({count})",
        "scopeRealBugMainline": "Real-Bug Mainline PRs ({count})",
        "scopeAllOpen": "All Open PRs incl. Cherry-Picks ({count})",
        "ballAll": "All Ball Owners",
        "ballReviewer": "Ball in Reviewer's Court",
        "ballAuthor": "Ball in Author's Court",
        "ballHold": "On Hold / Draft",
        "ballQueue": "Approved / Merge Queue",
        "sortPrio": "Sort: Linked Bug Severity ↓",
        "sortWaitDesc": "Sort: Days Waiting ↓",
        "sortUpdDesc": "Sort: Last Updated ↓",
        "sortNumDesc": "Sort: PR # ↓ (Newest)"
      },
      "statusCount": "Showing {matched} of {scoped} in-flight PRs",
      "clickHint": "Click red CI chips for build logs · Click target icon to jump to Issue",
      "cols": {
        "pr": "PR / Author",
        "blocker": "Blocker & Court Wait",
        "issues": "Linked Issues & Priority",
        "details": "Title, Review Progress & Failed CI Checks",
        "time": "Activity / Commit"
      },
      "ballTag": {
        "REVIEWER": "Reviewer",
        "AUTHOR": "Author",
        "MERGE_QUEUE": "Queue",
        "HOLD": "Hold"
      },
      "waitDays": "Wait {days}d",
      "waitToday": "<1d",
      "ciRunning": "{count} CI running",
      "ciPassed": "{count} passed",
      "ciMore": "+{count} more failed",
      "ciCollapse": "Show less",
      "revLgtm": "LGTM: {users}",
      "revChanges": "Changes req: {users}",
      "revThreadsWithUsers": "Unresolved ({count}): {users}",
      "revThreadsCountOnly": "Unresolved ({count})",
      "revRequested": "Requested: {users}",
      "lastCommit": "Commit {date}",
      "noLinkedIssue": "No open issue linked",
      "emptyTitle": "No Matching In-Flight PRs",
      "empty": "No active PRs match the selected filters. Reset filters below to view all.",
      "chipShort": {
        "DRAFT_OR_HOLD": "Hold",
        "CONFLICT": "Conflict",
        "NEEDS_OK_TO_TEST": "ok-to-test",
        "CI_FAILED": "CI ({count})",
        "WAITING_AUTHOR": "Author",
        "GATE_BLOCKED": "Gate",
        "NEEDS_LGTM2": "LGT1",
        "WAITING_REVIEWER_zero": "0-Review",
        "WAITING_REVIEWER_re": "Re-review",
        "WAITING_REVIEWER": "Review",
        "READY_MERGING": "Merging"
      },
      "detail": {
        "draft": "Pull request is marked as Draft",
        "hold_label": "Blocked by hold label: {gates}",
        "merge_conflict": "Merge conflict; needs rebase",
        "needs_ok_to_test": "External contribution awaiting maintainer /ok-to-test",
        "ci_failed": "{count} CI check(s) failed; needs fix or /retest",
        "changes_requested": "{users} requested changes; waiting on author update",
        "changes_requested_anon": "Reviewer requested changes; waiting on author update",
        "unresolved_thread": "{count} unresolved review thread(s) ({users}); waiting on author",
        "unresolved_thread_anon": "{count} unresolved review thread(s); waiting on author",
        "gate_label": "Reviewed, but blocked by merge gate: {gates}",
        "needs_second_lgtm": "Has 1st LGTM from {users}; needs 2nd LGTM to approve",
        "needs_second_lgtm_anon": "Has lgtm label; needs 2nd reviewer approval",
        "zero_review": "0 human reviews so far; awaiting first reviewer",
        "re_review": "Author pushed new commits; awaiting re-review from {users}",
        "re_review_anon": "Author pushed new commits; awaiting reviewer re-review",
        "awaiting_review": "Reviewed by {users}; awaiting LGTM",
        "awaiting_review_anon": "Review activity started; awaiting LGTM",
        "ci_running": "Approved; {count} CI check(s) still running",
        "merge_queue": "Approved & green; queued in Prow Tide"
      }
    },
    "enums": {
      "cat": {
        "EASY": "Real Bug · Easy",
        "MEDIUM": "Real Bug · Medium",
        "HARD": "Real Bug · Hard",
        "ENHANCEMENT": "Enhancement",
        "FILTER_RANDOM_FUZZ": "Filtered · Random/Fuzz",
        "FILTER_AI_OBSCURE": "Filtered · AI Corner Case",
        "FILTER_NON_OPT_OR_FIXED": "Filtered · Non-Opt/Fixed"
      },
      "catShort": {
        "EASY": "Easy",
        "MEDIUM": "Medium",
        "HARD": "Hard",
        "ENHANCEMENT": "Enhancement",
        "FILTER_RANDOM_FUZZ": "Fuzz Noise",
        "FILTER_AI_OBSCURE": "AI Edge",
        "FILTER_NON_OPT_OR_FIXED": "Non-Opt/Fixed"
      },
      "diff": {
        "EASY": "Est. 1–3 days",
        "MEDIUM": "Est. 3–7 days",
        "HARD": "Est. 1–3+ weeks",
        "ENHANCEMENT": "Feature / rule enhancement",
        "FILTER_RANDOM_FUZZ": "Random / differential fuzzing",
        "FILTER_AI_OBSCURE": "Obscure AI static edge case",
        "FILTER_NON_OPT_OR_FIXED": "Other module / merged / flaky"
      },
      "severity": {
        "CRITICAL": "S0 · Critical",
        "MAJOR": "S1 · Major",
        "MODERATE": "S2 · Moderate",
        "MINOR": "S3 · Minor",
        "N/A": "N/A"
      },
      "severityShort": {
        "CRITICAL": "S0 Critical",
        "MAJOR": "S1 Major",
        "MODERATE": "S2 Moderate",
        "MINOR": "S3 Minor",
        "N/A": "N/A"
      },
      "subsystem": {
        "statistics": "Statistics & Cardinality",
        "plan_cache_spm_hint": "Plan Cache / SPM / Hint",
        "logical_rewrite": "Logical Rewrite & Subquery",
        "access_path_partition_lock": "Access Path / Partition / Lock",
        "cbo_cost_join_reorder": "CBO Cost & Join Reorder",
        "name_resolution_group_by": "Name Resolution & GROUP BY",
        "tiflash_mpp": "TiFlash / MPP"
      }
    }
  }
});
