SiteI18n.extend("zh-CN", {
  "common": {
    "appBrand": "TiDB Optimizer Triage Hub",
    "langSwitchAria": "界面语言切换",
    "toggleTheme": "切换主题",
    "toggleThemeAria": "切换亮色或暗色主题",
    "themeToLight": "切换为亮色模式",
    "themeToDark": "切换为暗色模式",
    "backToTop": "回到顶部"
  },
  "report": {
    "meta": {
      "title": "TiDB 优化器 Issue 研判与 PR 推进工作台",
      "description": "针对 pingcap/tidb 优化器开放单据的去噪筛除、修复难度（Difficulty）、严重度（Severity）与在途 PR 阻塞推进独立工作台。"
    },
    "nav": {
      "more": "更多",
      "syncInfoBtn": "数据快照"
    },
    "views": {
      "issuesTab": "Issue 研判",
      "prsTab": "PR 进展"
    },
    "rw": {
      "modeBadge": "维护者",
      "readonlyBadge": "只读",
      "pendingBtn": "待处理",
      "syncPrsBtn": "刷新 PR",
      "syncIssuesBtn": "同步 Issue",
      "syncing": "同步中…",
      "unlockBtn": "维护者登录",
      "lockBtn": "退出登录",
      "authModalTitle": "维护者登录",
      "sessionExpired": "登录已过期。重新登录后可继续当前审核。",
      "authModalHint": "请输入服务端配置的访问令牌（TRIAGE_HUB_TOKEN），验证通过后将在本浏览器启用校准与 GitHub 同步按钮。",
      "authTokenPlaceholder": "输入访问令牌…",
      "authSubmitBtn": "登录",
      "authCancelBtn": "取消",
      "authInvalidToken": "访问令牌无效，请检查后重试",
      "editBtn": "校准",
      "inspectBtn": "详情",
      "editedTag": "人工校准",
      "forceKeepTag": "强制保留",
      "retriageTag": "待复核",
      "jumpToPr": "在 PR 推进看板中定位此 PR",
      "jumpToIssue": "在 Issue 研判看板中定位此单据",
      "toastSaved": "已保存 Issue #{num} 的人工校准",
      "toastReset": "已恢复 Issue #{num} 的自动基线",
      "toastPrSynced": "已刷新 {open} 个开放 PR 状态（{calls} 次 GraphQL，耗时 {ms}ms）",
      "toastIssueSynced": "已完成双流增量同步（待分拣新单 {pending} 条）",
      "toastPendingResolved": "已处理待分拣单据 #{num}",
      "toastRetriageAcked": "已确认维持 Issue #{num} 的当前分级",
      "toastRetriageBatchAcked": "已批量确认维持 {count} 条单据的当前分级",
      "toastIssueExcluded": "已将补充单 #{num} 移入排除名单",
      "toastExcludedRestored": "已从排除名单恢复 #{num}（下次同步重新评估）",
      "retriageReasons": {
        "new_fix_pr": "新增修复 PR",
        "fix_pr_merged_still_open": "修复 PR 已合入未关单",
        "customer_reported": "新增客户生产反馈",
        "gh_severity_escalated": "GitHub 严重度上调",
        "label_scope_changed": "关键标签范围变更",
        "substantive_new_comments": "新增实质性讨论"
      },
      "drawer": {
        "rootCause": "根因与研判备注",
        "english": "英文",
        "chinese": "中文",
        "noNote": "暂无备注。",
        "issueUnavailable": "Issue #{num} 已移出活跃列表，编辑窗口已关闭；请从当前队列重新查看。",
        "title": "校准 Issue #{num}",
        "inspectTitle": "Issue #{num} 研判详情",
        "close": "关闭",
        "openGh": "在 GitHub 打开",
        "metaAuthor": "提交人",
        "metaAssignees": "指派给",
        "metaUnassigned": "未指派",
        "metaCreated": "创建于",
        "metaUpdated": "最后更新",
        "metaGhSev": "GH 原标严重度",
        "metaLabels": "GitHub 标签",
        "metaLinkedPrs": "关联 PR",
        "sectionTriage": "研判分级与归属",
        "sectionFlags": "候选池保留策略",
        "sectionNotes": "双语根因与跟进备注",
        "catLabel": "分类与修复难度",
        "sevLabel": "研判严重度",
        "subLabel": "所属子模块",
        "borderlineCheck": "标记为「边界保留真 Bug」(Borderline Kept)",
        "borderlineHint": "虽由 Fuzz / 静态审计发现，但常规业务路径可达且造成错结果或崩溃",
        "forceKeepCheck": "强制保留在候选池 (Force Keep)",
        "forceKeepHint": "防止 GitHub 标签变更时在增量同步中被自动移出",
        "noteZhLabel": "中文研判备注 (zh-CN)",
        "noteEnLabel": "英文研判备注 (en)",
        "notePlaceholderZh": "填写根因定位、触发条件或跟进备注…",
        "notePlaceholderEn": "Concise root cause, trigger condition, or triage note…",
        "retriageBannerTitle": "单据状态发生实质性演进，建议复核",
        "keepCurrentBtn": "确认维持当前分级",
        "excludeSummary": "排除出优化器候选池（补充集噪声治理）",
        "excludeHint": "此单不属于 sig/planner + type/bug 标准集。若确认归属其他模块且无需保留在看板中，可将其加入排除名单。",
        "excludeReasonPlaceholder": "排除原因（如：归属执行器/BR，非优化器问题）",
        "excludeConfirmBtn": "移出看板并加入排除名单",
        "excludeCanonicalDisabled": "标准集单据 (sig/planner + type/bug) 不可直接排除，请选择「已筛除」分类保留审计记录。",
        "saveBtn": "保存校准",
        "saveHint": "Ctrl/⌘ + Enter 快速保存",
        "resetBtn": "恢复自动基线",
        "cancelBtn": "取消"
      },
      "inbox": {
        "loadingExcluded": "正在加载已排除的 Issue…",
        "loadFailed": "未能加载已排除的 Issue。",
        "title": "分拣与复核工作台",
        "tabPending": "新候选 ({count})",
        "tabRetriage": "待复核 ({count})",
        "tabExcluded": "已排除 ({count})",
        "emptyTitle": "新候选队列已清空",
        "empty": "当前没有待分拣的新增候选 Issue。可通过 CLI `triage-hub sync-issues` 增量拉取并配合 AI 分拣入库。",
        "retriageEmptyTitle": "无待复核变更单",
        "retriageEmpty": "所有已入库单据的 PR 进展、严重度标签与讨论均与上次校准基线一致。",
        "excludedEmptyTitle": "暂无已排除单据",
        "excludedEmpty": "排除名单为空，或没有匹配搜索词的记录。",
        "keepAllBtn": "全部维持原判 ({count})",
        "keepCurrentBtn": "维持当前分级",
        "recalibrateBtn": "打开校准抽屉",
        "restoreBtn": "恢复评估",
        "excludedSearchPlaceholder": "按单号、标题、原因或标签过滤已排除单据…",
        "excludedMeta": "排除于 {date} · 来源 {source}",
        "suggestedBadge": "预判 ({confidence})",
        "suggestedSignals": "命中信号",
        "prFilesSummary": "主干 PR #{num}：非测试文件 {nonTest} 个 (+{add}/-{del}) · 测试 {test} 个",
        "savedRefreshFailed": "已保存，但刷新结果失败；再次操作前请检查最新状态。",
        "missingReviewVersion": "审核数据不完整，请刷新后再确认。",
        "staleDraft": "此草稿基于较早的数据，请先审核最新内容再提交。",
        "reviewLatest": "审核最新内容",
        "canonicalExcludeTip": "标准集单据 (sig/planner + type/bug) 不可排除，请使用筛除分类入库",
        "scopesLabel": "命中规则",
        "bodySummary": "查看正文与最近评论 ({comments})",
        "excludeReasonPlaceholder": "排除原因（选填，如：归属执行器/BR）",
        "acceptBtn": "确认定级入库",
        "excludeBtn": "排除此单"
      }
    },
    "header": {
      "subtitle": "pingcap/tidb · 标准集 {totalCan} 个 Issue · 补充集 {totalSup} 个",
      "boards": "工作台视图",
      "issuesTitle": "Issue 研判",
      "prsTitle": "PR 进展",
      "prSubtitle": "主干 PR {mainline} 个 · Cherry-pick {backports} 个"
    },
    "sync": {
      "snapshotLabel": "Issue 快照",
      "prSnapshotLabel": "PR 刷新",
      "watermarkLabel": "更新水位线",
      "queryLabel": "增量检索词",
      "copyTitle": "点击复制 GitHub 增量搜索限定词",
      "copied": "已复制检索词",
      "deltaTitle": "距上一快照变更明细",
      "deltaAdded": "新增开放候选 ({count})",
      "deltaClosed": "已关单或转出 ({count})",
      "copyFailed": "复制失败，可选中查询条件手动复制。"
    },
    "bugTrend": {
      "all": "查看全部日期",
      "title": "已确认且仍开放的 Bug",
      "hint": "每日最后一次记录 · UTC+8 · 横向拖选放大"
    },
    "kpi": {
      "scopedTotal": {
        "label": "全部候选"
      },
      "realBugs": {
        "label": "有效 Bug"
      },
      "highSev": {
        "label": "高严重度"
      },
      "prCoverage": {
        "label": "修复 PR 覆盖"
      },
      "groups": {
        "canonical": "标准集 {count}",
        "supplemental": "补充集 {count}",
        "easy": "简单 {count}",
        "medium": "中等 {count}",
        "hard": "困难 {count}",
        "critical": "S0 {count}",
        "major": "S1 {count}",
        "open": "有在途 PR {count}",
        "uncovered": "无在途 PR {count}"
      }
    },
    "matrix": {
      "hint": "点击数量筛选",
      "title": "优先级矩阵",
      "corner": "修复难度",
      "difficulty": {
        "EASY": "简单",
        "MEDIUM": "中等",
        "HARD": "困难"
      },
      "total": "合计",
      "quickWinTag": "速赢",
      "cellSub": "{wait} 无在途 PR · {inPr} 有在途 PR",
      "resetSub": "清除矩阵筛选"
    },
    "rubric": {
      "summary": "分级与去噪口径说明",
      "link": "分级说明",
      "scope": {
        "title": "筛除与保留",
        "fuzzHtml": "<b>筛除 · Random / 差分 Fuzz</b> 由 SQLancer、TLP / NoREC / CERT、多层非常规类型集合运算、随机内建函数组合或手动黑名单关停优化规则构造的差分测试极端用例。",
        "aiHtml": "<b>筛除 · AI 静态猎奇边界</b> 基于源码静态审计刻意拼凑的表达式哈希碰撞、纪元极值哨兵或未默认开启开关的理论边界，常规业务 SQL 几乎不可达。",
        "nonOptHtml": "<b>筛除 · 非优化器 / 已修 / 无效</b> 归属执行器、DDL、存储客户端、UI 等非优化器模块的问题，主干已合入修复仅待关单/回移的单据，以及单测 Flaky 用例。",
        "keepHtml": "<b>边界保留与 Enhancement 区分</b> 虽由 Fuzz 约简或 AI 发现，但触发路径常规且造成<b>错结果、死锁、漏锁或崩溃</b>的问题一律保留为真实 Bug；结果正确、诉求为新增改写规则 / 访问路径 / 属性推导或语法支持的单据归入 <b>Enhancement</b>。"
      },
      "difficulty": {
        "title": "修复难度",
        "easyHtml": "<b>Easy · 简单（预计 1~3 天）</b> 根因清晰、改动收敛在单文件局部守卫或元数据补齐（如权限检查补漏、Plan Cache 键补齐会话变量、局部算术边界修正、告警与展示修复），不改变多阶段规则契约。",
        "medHtml": "<b>Medium · 中等（预计 3~7 天）</b> 涉及单子系统相邻阶段的不变量协同（如逻辑改写与列裁剪 Schema 映射、Range 提取与分区悲观锁下推、参数泛化与类型精度、分区 Global Stats 合并）。",
        "hardHtml": "<b>Hard · 困难（预计 1~3+ 周）</b> 牵涉跨模块重构、CBO 代价与超界外推模型权衡（易引发基准或生产计划回归）、<code>ANALYZE</code> 内存与系统表并发锁治理、MPP 隔离交互或单遍名称解析历史架构债，需配套设计方案与全量回归验证。"
      },
      "severity": {
        "title": "严重度",
        "critHtml": "<b>S0 · Critical 致命</b> 进程 Panic / Crash、<code>ANALYZE</code> 或编译引发实例 OOM、后台核心协程永久退出、长时间阻塞全集群 GC、DML 错删改或串表、悲观事务 <code>FOR UPDATE</code> 静默漏锁、权限校验绕过，以及常规常见 <code>SELECT</code> 容易触发的错结果。",
        "majHtml": "<b>S1 · Major 严重</b> 非边角但需特定条件触发的 <code>SELECT</code> 错结果、合法常见 SQL 报内部错误（如 <code>ERROR 1105</code>、隐式列解析失败）或统计信息元表死锁阻断执行、核心生产场景出现数量级计划退化。",
        "modHtml": "<b>S2 · Moderate 中等</b> 边界保留但较难触发的边角错结果、一般性基数/代价估算偏差（计划次优但结果正确且无雪崩）、Plan Cache / SPM 未命中、Hint 导出回放局部失配或低频 MySQL 兼容性边缘差异。",
        "minHtml": "<b>S3 · Minor 轻微</b> Hint 告警文案、<code>EXPLAIN</code> / <code>SHOW STATS</code> / 慢日志 / 监控指标显示细节或周边诊断工具微瑕。<i>（若 GitHub 原始 <code>severity/*</code> 标签未标或与研判不一致，行内会附注 <code>GH</code> 原标供对照。）</i>"
      }
    },
    "filters": {
      "retriageOnly": "仅看待复核",
      "searchPlaceholder": "搜索标题、编号、作者或研判备注",
      "searchAria": "搜索单据",
      "clearSearch": "清空搜索",
      "resetAll": "重置全部筛选",
      "scopeAria": "统计口径",
      "scope": {
        "all": "全量候选池 ({count})",
        "canonical": "标准集 sig/planner + bug ({count})",
        "customer": "客户生产反馈 ({count})",
        "borderline": "边界保留真 Bug ({count})",
        "retriage": "待复核变更单 ({count})"
      },
      "subAria": "子系统过滤",
      "subAll": "全部子模块",
      "sevAria": "严重度过滤",
      "sev": {
        "none": "严重度条件无交集",
        "all": "全部严重度",
        "high": "高危 S0 + S1 (Critical/Major)",
        "critical": "S0 · Critical 致命",
        "major": "S1 · Major 严重",
        "moderate": "S2 · Moderate 中等",
        "minor": "S3 · Minor 轻微",
        "ghUnlabeled": "GH 未标 severity/*"
      },
      "prAria": "PR 状态过滤",
      "pr": {
        "all": "全部 PR 进展",
        "openFix": "有在途修复 PR (OPEN/Draft)",
        "ballReviewer": "在途 PR 卡在 Reviewer",
        "ballAuthor": "在途 PR 卡在 Author / CI 挂",
        "needsLgtm2": "在途 PR 已获 LGT1 (差1票)",
        "unclaimed": "无在途修复 PR (待认领)",
        "hasAny": "含任意关联 PR",
        "hasMerged": "含已合入 PR (MERGED)",
        "noPr": "无任何关联 PR"
      },
      "assAria": "指派状态过滤",
      "ass": {
        "all": "全部指派状态",
        "unassigned": "未指派 (Unassigned)",
        "assigned": "已指派负责人"
      },
      "updWinAria": "最近更新窗口",
      "updWin": {
        "all": "全部更新时间",
        "d1": "近 24 小时更新",
        "d7": "近 7 天更新",
        "d30": "近 30 天更新",
        "d90": "近 90 天更新",
        "custom": "自定义起始日期…"
      },
      "sincePrefix": "自",
      "sinceSuffix": "起",
      "sinceAria": "增量起始日期",
      "sortAria": "排序",
      "sort": {
        "updatedDesc": "最近更新",
        "prioDesc": "优先级最高",
        "numDesc": "编号从新到旧",
        "numAsc": "编号从旧到新"
      },
      "more": "筛选",
      "category": "分类",
      "removeFilter": "移除筛选：{label}",
      "stage": "阻塞阶段",
      "ownerLabel": "下一步责任方",
      "scopeLabel": "范围",
      "severityLabel": "严重度",
      "subsystemLabel": "子系统",
      "prLabel": "关联 PR",
      "assigneeLabel": "指派状态",
      "updatedLabel": "更新时间"
    },
    "pills": {
      "REAL_ALL": "有效 Bug",
      "EASY": "Easy",
      "MEDIUM": "Medium",
      "HARD": "Hard",
      "ENHANCEMENT": "Enhancement",
      "FILTER_RANDOM_FUZZ": "筛除: Fuzz",
      "FILTER_AI_OBSCURE": "筛除: AI 边界",
      "FILTER_NON_OPT_OR_FIXED": "筛除: 已修/非优化器",
      "ALL": "全部分类"
    },
    "status": {
      "count": "显示 {matched} / {scoped} 条单据"
    },
    "table": {
      "colIssue": "Issue",
      "colCatSev": "研判分类",
      "colSub": "所属子模块",
      "colTime": "最近动态",
      "ghSevNote": "GH: {val}",
      "ghUnlabeled": "未标",
      "tagCustomer": "客户反馈",
      "tagBorderline": "边界保留",
      "tagSupplemental": "补充池",
      "updatedPrefix": "{time}",
      "createdPrefix": "创建 {date}",
      "assignedTo": "指派 @{user}",
      "emptyTitle": "未找到匹配的 Issue",
      "empty": "请尝试放宽筛选条件、清空搜索关键词或点击下方按钮重置筛选。"
    },
    "prBoard": {
      "courts": {
        "REVIEWER": "待审阅人处理",
        "REVIEWER_sub": "代码审阅或测试放行",
        "AUTHOR": "待作者处理",
        "AUTHOR_sub": "修改代码、解决冲突或修复 CI",
        "MERGE_QUEUE": "合并队列",
        "MERGE_QUEUE_sub": "已批准，等待合并",
        "HOLD": "暂缓处理",
        "HOLD_sub": "草稿或带有 hold 标签"
      },
      "cards": {
        "ALL": "全部阶段",
        "NEEDS_LGTM2": "LGT1 差一票",
        "WAITING_REVIEWER": "待首审 / 复审",
        "NEEDS_OK_TO_TEST": "待 ok-to-test",
        "CI_FAILED": "CI 检查失败",
        "CONFLICT": "分支合并冲突",
        "WAITING_AUTHOR": "等作者回应",
        "GATE_BLOCKED": "卡合并门禁",
        "READY_MERGING": "批准待合入",
        "DRAFT_OR_HOLD": "草稿 / Hold"
      },
      "filters": {
        "searchPlaceholder": "搜索 PR 标题、编号、审阅人或 CI 检查",
        "scopeMainline": "主干在途 PR ({count})",
        "scopeRealBugMainline": "仅真 Bug 主干 PR ({count})",
        "scopeAllOpen": "全部开放 PR · 含 Cherry-Pick ({count})",
        "ballAll": "全部责任方",
        "ballReviewer": "审阅人",
        "ballAuthor": "作者",
        "ballHold": "暂挂 Hold / 草稿",
        "ballQueue": "已批准排队列",
        "sortPrio": "关联 Issue 优先级",
        "sortWaitDesc": "等待最久",
        "sortUpdDesc": "最近更新",
        "sortNumDesc": "编号从新到旧",
        "searchAria": "搜索 PR"
      },
      "statusCount": "显示 {matched} / {scoped} 个在途 PR",
      "cols": {
        "pr": "PR",
        "blocker": "阻塞原因",
        "issues": "关联 Issue",
        "time": "最近动态"
      },
      "ballTag": {
        "REVIEWER": "Reviewer",
        "AUTHOR": "Author",
        "MERGE_QUEUE": "Queue",
        "HOLD": "Hold"
      },
      "waitDays": "已等 {days}d",
      "waitToday": "<1d",
      "ciRunning": "{count} 项 CI 运行中",
      "ciPassed": "{count} 项通过",
      "ciMore": "展开其余 {count} 项失败",
      "revLgtm": "LGTM: {users}",
      "revChanges": "需修改: {users}",
      "revThreadsWithUsers": "未决讨论 ({count}): {users}",
      "revThreadsCountOnly": "未决讨论 ({count})",
      "revRequested": "待审: {users}",
      "lastCommit": "提交 {date}",
      "noLinkedIssue": "未关联开放单",
      "emptyTitle": "没有匹配的在途 PR",
      "empty": "当前筛选条件下无匹配项，可点击下方按钮重置筛选条件。",
      "chipShort": {
        "DRAFT_OR_HOLD": "挂起",
        "CONFLICT": "冲突",
        "NEEDS_OK_TO_TEST": "待放行",
        "CI_FAILED": "CI 挂 ({count})",
        "WAITING_AUTHOR": "等作者",
        "GATE_BLOCKED": "卡门禁",
        "NEEDS_LGTM2": "LGT1",
        "WAITING_REVIEWER_zero": "0 审阅",
        "WAITING_REVIEWER_re": "待复审",
        "WAITING_REVIEWER": "待审",
        "READY_MERGING": "待合入"
      },
      "detail": {
        "draft": "处于草稿状态 (Draft)",
        "hold_label": "带有挂起标签: {gates}",
        "merge_conflict": "存在分支合并冲突，需作者 Rebase",
        "needs_ok_to_test": "外部贡献待 Maintainer 评论 /ok-to-test 放行",
        "ci_failed": "{count} 项 CI 检查失败，需排查重跑或修代码",
        "changes_requested": "{users} 提出修改意见，待作者更新",
        "changes_requested_anon": "审阅人提出修改意见，待作者更新",
        "unresolved_thread": "存在 {count} 处未解决审阅讨论 ({users})，待作者回应",
        "unresolved_thread_anon": "存在 {count} 处未解决审阅讨论，待作者回应",
        "gate_label": "已获审阅认可，但卡合并门禁: {gates}",
        "needs_second_lgtm": "已获 {users} 首票 LGTM，差第 2 票 Approve",
        "needs_second_lgtm_anon": "已有 lgtm 标签，差第 2 位审阅人 Approve",
        "zero_review": "尚无人类 Reviewer 审阅，待首次接单",
        "re_review": "作者已推新提交，待 {users} 复审",
        "re_review_anon": "作者已推新提交，待审阅人复审",
        "awaiting_review": "已有审阅互动 ({users})，尚待给出 LGTM",
        "awaiting_review_anon": "已有审阅互动，尚待给出 LGTM",
        "ci_running": "已获批 (Approved)，正在跑收尾 CI ({count} 项)",
        "merge_queue": "检查全绿，正在排 Prow Tide 合并队列"
      }
    },
    "enums": {
      "cat": {
        "EASY": "真实 Bug · Easy",
        "MEDIUM": "真实 Bug · Medium",
        "HARD": "真实 Bug · Hard",
        "ENHANCEMENT": "Enhancement 增强",
        "FILTER_RANDOM_FUZZ": "已筛除 · Random/Fuzz",
        "FILTER_AI_OBSCURE": "已筛除 · AI 极端边界",
        "FILTER_NON_OPT_OR_FIXED": "已筛除 · 非优化器/已修"
      },
      "catShort": {
        "EASY": "Easy",
        "MEDIUM": "Medium",
        "HARD": "Hard",
        "ENHANCEMENT": "Enhancement",
        "FILTER_RANDOM_FUZZ": "Fuzz 噪声",
        "FILTER_AI_OBSCURE": "AI 边界",
        "FILTER_NON_OPT_OR_FIXED": "非优化器/已修"
      },
      "diff": {
        "EASY": "预计 1~3 天",
        "MEDIUM": "预计 3~7 天",
        "HARD": "预计 1~3+ 周",
        "ENHANCEMENT": "功能或规则增强诉求",
        "FILTER_RANDOM_FUZZ": "随机或差分测试极端构造",
        "FILTER_AI_OBSCURE": "AI 静态审计极端理论边界",
        "FILTER_NON_OPT_OR_FIXED": "其他模块 / 主干已合入 / Flaky"
      },
      "severity": {
        "CRITICAL": "S0 · Critical 致命",
        "MAJOR": "S1 · Major 严重",
        "MODERATE": "S2 · Moderate 中等",
        "MINOR": "S3 · Minor 轻微",
        "N/A": "N/A"
      },
      "severityShort": {
        "CRITICAL": "S0 致命",
        "MAJOR": "S1 严重",
        "MODERATE": "S2 中等",
        "MINOR": "S3 轻微",
        "N/A": "N/A"
      },
      "subsystem": {
        "statistics": "统计信息与基数估算",
        "plan_cache_spm_hint": "Plan Cache / SPM / Hint",
        "logical_rewrite": "逻辑改写与子查询",
        "access_path_partition_lock": "访问路径 / 分区 / 锁",
        "cbo_cost_join_reorder": "CBO 代价与连接重排",
        "name_resolution_group_by": "名称解析与 GROUP BY",
        "tiflash_mpp": "TiFlash / MPP"
      }
    }
  }
});
