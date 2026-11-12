SiteI18n.extend("zh-CN", {
  "common": {
    "appBrand": "TiDB Optimizer Triage Hub",
    "langSwitchAria": "界面语言切换",
    "toggleTheme": "切换主题",
    "toggleThemeAria": "切换亮色或暗色主题"
  },
  "report": {
    "meta": {
      "title": "TiDB 优化器 Issue 研判与 PR 推进工作台",
      "description": "针对 pingcap/tidb 优化器开放单据的去噪筛除、修复难度（Difficulty）、严重度（Severity）与在途 PR 阻塞推进独立工作台。"
    },
    "nav": {
      "dataJson": "JSON 快照 (/api/dataset.json)",
      "ghIssues": "GitHub Issues"
    },
    "views": {
      "issuesTab": "📋 Issue 研判看板 ({count})",
      "prsTab": "🔀 PR 阻塞推进 ({count})"
    },
    "rw": {
      "modeBadge": "读写权限",
      "readonlyBadge": "只读视图",
      "pendingBtn": "🆕 待分拣新单 ({count})",
      "syncPrsBtn": "🔄 刷新 PR 状态",
      "syncIssuesBtn": "📥 增量同步 Issue",
      "syncing": "同步中…",
      "editBtn": "校准",
      "editedTag": "已人工校准",
      "forceKeepTag": "强制保留",
      "jumpToPr": "在 PR 看板中查看",
      "jumpToIssue": "在 Issue 看板中定位",
      "toastSaved": "已保存 Issue #{num} 的人工校准 ✓",
      "toastReset": "已恢复 Issue #{num} 的自动基线 ✓",
      "toastPrSynced": "已刷新 {open} 个开放 PR 状态（{calls} 次 GraphQL，耗时 {ms}ms）✓",
      "toastIssueSynced": "已完成双流增量同步（待分拣新单 {pending} 条）✓",
      "toastPendingResolved": "已处理待分拣单据 #{num} ✓",
      "drawer": {
        "title": "校准 Issue #{num} 研判结论",
        "close": "关闭",
        "catLabel": "分类与修复难度 (Category / Difficulty)",
        "sevLabel": "研判严重度 (Assessed Severity)",
        "subLabel": "所属子模块 (Subsystem)",
        "borderlineCheck": "标记为「边界保留真 Bug」(Borderline Kept)",
        "forceKeepCheck": "强制保留（防止标签变更时自动移出候选池）",
        "noteZhLabel": "中文研判备注 (zh-CN)",
        "noteEnLabel": "英文研判备注 (en)",
        "notePlaceholderZh": "填写根因定位、触发条件或跟进备注…",
        "notePlaceholderEn": "Concise root cause, trigger condition, or triage note…",
        "saveBtn": "💾 保存落库",
        "resetBtn": "↺ 恢复自动基线",
        "cancelBtn": "取消"
      },
      "inbox": {
        "title": "🆕 待分拣新单收件箱 ({count})",
        "empty": "当前没有待分拣的新增候选 Issue。点击顶部「📥 增量同步 Issue」可拉取最新单据。",
        "scopesLabel": "命中准入规则",
        "bodySummary": "展开查看 Issue 正文与最近评论 ({comments})",
        "excludeReasonPlaceholder": "排除原因（如：归属执行器/BR，非优化器问题）",
        "acceptBtn": "✅ 确认定级入库",
        "excludeBtn": "🚫 排除此单"
      }
    },
    "header": {
      "title": "TiDB 优化器开放 Issue 研判与在途 PR 推进看板",
      "subtitle": "覆盖 pingcap/tidb 优化器相关开放候选单据共 {totalAll} 条（含 sig/planner + type/bug 标准集 {totalCan} 条与统计信息 / SPM / 未分拣补充集 {totalSup} 条）。"
    },
    "sync": {
      "snapshotLabel": "Issue 快照",
      "prSnapshotLabel": "PR 状态刷新",
      "watermarkLabel": "最新 Issue 更新水位线",
      "queryLabel": "增量更新检索条件",
      "copyTitle": "点击复制 GitHub 增量过滤条件",
      "copied": "已复制: {query} ✓"
    },
    "kpi": {
      "scopedTotal": {
        "label": "当前口径候选总数",
        "hint": "全部为当前 OPEN 单据"
      },
      "realBugs": {
        "label": "真实有效优化器 Bug",
        "hint": "占比 {pct}% (E:{easy} · M:{med} · H:{hard})"
      },
      "highSev": {
        "label": "高危严重度 (S0+S1)",
        "hint": "致命 Critical {crit} · 严重 Major {maj}"
      },
      "prCoverage": {
        "label": "真实 Bug 修复 PR 覆盖",
        "hint": "在途 PR {open} 条 · 待认领 {unclaimed} 条"
      },
      "enhAndNoise": {
        "label": "Enhancement / 筛除噪声",
        "hint": "Fuzz {fuzz} · AI {ai} · 已修/其他 {oth}"
      }
    },
    "legend": {
      "easy": "真实 Bug · Easy",
      "med": "真实 Bug · Medium",
      "hard": "真实 Bug · Hard",
      "enh": "实为 Enhancement",
      "flt": "已筛除：Random / AI / 已修 / 非优化器"
    },
    "matrix": {
      "title": "🎯 真实 Bug 修复难度 (Difficulty) × 严重度 (Severity) 优先级矩阵",
      "subHtml": "点击任意单元格可直接联动过滤下方列表（浅黄底色为 <b>Easy × Critical/Major 高 ROI 速赢区</b>）",
      "corner": "修复难度 \\ 研判严重度",
      "colCrit": "🔥 Critical · 致命",
      "colMaj": "🟠 Major · 严重",
      "colMod": "🔵 Moderate · 中等",
      "colMin": "⚪ Minor · 轻微",
      "colTotal": "难度合计",
      "rowEasy": "🟢 Easy · 简单 (1~3d)",
      "rowMed": "🟡 Medium · 中等 (3~7d)",
      "rowHard": "🔴 Hard · 困难 (1~3w+)",
      "rowTotal": "严重度合计",
      "cellSub": "{wait} 待认领 · {inPr} 在途",
      "unclaimedSub": "{wait} 待认领",
      "resetSub": "点击重置矩阵",
      "activeBadge": "矩阵联动: {diff} × {sev}",
      "allDiff": "全部难度",
      "allSev": "全部严重度",
      "clearTitle": "清除矩阵筛选"
    },
    "rubric": {
      "summary": "筛除与分级口径说明（Triage Rubric）",
      "toggleHint": "展开 / 收起 ▾",
      "scope": {
        "title": "1. 噪声筛除与保留边界（Triage Scope）",
        "fuzzHtml": "<b>筛除 · Random / 差分 Fuzz</b>：由 SQLancer、TLP / NoREC / CERT、多层非常规类型集合运算、随机内建函数组合或手动黑名单关停优化规则构造的差分测试极端用例。",
        "aiHtml": "<b>筛除 · AI 静态猎奇边界</b>：基于源码静态审计刻意拼凑的表达式哈希碰撞、纪元极值哨兵或未默认开启开关的理论边界，常规业务 SQL 几乎不可达。",
        "nonOptHtml": "<b>筛除 · 非优化器 / 已修 / 无效</b>：归属执行器、DDL、存储客户端、UI 等非优化器模块的问题，主干已合入修复仅待关单/回移的单据，以及单测 Flaky 用例。",
        "keepHtml": "<b>边界保留与 Enhancement 区分</b>：虽由 Fuzz 约简或 AI 发现，但触发路径常规且造成<b>错结果、死锁、漏锁或崩溃</b>的问题一律保留为真实 Bug；结果正确、诉求为新增改写规则 / 访问路径 / 属性推导或语法支持的单据归入 <b>Enhancement</b>。"
      },
      "difficulty": {
        "title": "2. 修复难度分级（Difficulty · 工程成本）",
        "easyHtml": "<b>🟢 Easy · 简单（预计 1~3 天）</b>：根因清晰、改动收敛在单文件局部守卫或元数据补齐（如权限检查补漏、Plan Cache 键补齐会话变量、局部算术边界修正、告警与展示修复），不改变多阶段规则契约。",
        "medHtml": "<b>🟡 Medium · 中等（预计 3~7 天）</b>：涉及单子系统相邻阶段的不变量协同（如逻辑改写与列裁剪 Schema 映射、Range 提取与分区悲观锁下推、参数泛化与类型精度、分区 Global Stats 合并）。",
        "hardHtml": "<b>🔴 Hard · 困难（预计 1~3+ 周）</b>：牵涉跨模块重构、CBO 代价与超界外推模型权衡（易引发基准或生产计划回归）、<code>ANALYZE</code> 内存与系统表并发锁治理、MPP 隔离交互或单遍名称解析历史架构债，需配套设计方案与全量回归验证。"
      },
      "severity": {
        "title": "3. 严重度分级（Severity · 故障影响）",
        "critHtml": "<b>🔥 Critical · 致命（S0）</b>：进程 Panic / Crash、<code>ANALYZE</code> 或编译引发实例 OOM、后台核心协程永久退出、长时间阻塞全集群 GC、DML 错删改或串表、悲观事务 <code>FOR UPDATE</code> 静默漏锁、权限校验绕过。",
        "majHtml": "<b>🟠 Major · 严重（S1）</b>：只读 <code>SELECT</code> 返回错误结果、合法常见 SQL 报内部错误（如 <code>ERROR 1105</code>、隐式列解析失败）或统计信息元表死锁阻断执行、核心生产场景出现数量级计划退化。",
        "modHtml": "<b>🔵 Moderate · 中等（S2）</b>：一般性基数/代价估算偏差（计划次优但结果正确且无雪崩）、Plan Cache / SPM 未命中、Hint 导出回放局部失配或低频 MySQL 兼容性边缘差异。",
        "minHtml": "<b>⚪ Minor · 轻微（S3）</b>：Hint 告警文案、<code>EXPLAIN</code> / <code>SHOW STATS</code> / 慢日志 / 监控指标显示细节或周边诊断工具微瑕。<i>（若 GitHub 原始 <code>severity/*</code> 标签未标或与研判不一致，行内会附注 <code>GH原标</code> 供对照。）</i>"
      }
    },
    "filters": {
      "searchPlaceholder": "搜索 Issue #、PR #（如 #72862）、标题、@作者 或研判备注…",
      "searchAria": "搜索单据",
      "scopeAria": "统计口径",
      "scope": {
        "all": "全量候选池 ({count})",
        "canonical": "仅 sig/planner + type/bug ({count})",
        "customer": "仅客户生产反馈 ({count})",
        "borderline": "边界保留真 Bug ({count})"
      },
      "subAria": "子系统过滤",
      "subAll": "全部子系统",
      "sevAria": "严重度过滤",
      "sev": {
        "all": "全部严重度 (Severity)",
        "high": "🔥+🟠 Critical + Major (高优)",
        "critical": "🔥 Critical · 致命",
        "major": "🟠 Major · 严重",
        "moderate": "🔵 Moderate · 中等",
        "minor": "⚪ Minor · 轻微",
        "ghUnlabeled": "GitHub 原标签未标 severity/*"
      },
      "prAria": "PR 状态过滤",
      "pr": {
        "all": "全部 PR 状态",
        "openFix": "🟢 有在途修复 PR (OPEN/Draft)",
        "ballReviewer": "👀 在途 PR 卡在 Reviewer / 待放行",
        "ballAuthor": "🔴 在途 PR 卡在 Author / CI 挂",
        "needsLgtm2": "⚡ 在途 PR 已获 LGT1 (差1票)",
        "unclaimed": "🎯 无在途修复 PR (待认领)",
        "hasAny": "🔗 有任意关联内核 PR",
        "hasMerged": "🟣 含已合入关联 PR (MERGED)",
        "noPr": "⚪ 无任何关联 PR"
      },
      "updWinAria": "最近更新窗口",
      "updWin": {
        "all": "全部更新时间",
        "d1": "近 24 小时内更新",
        "d7": "近 7 天内更新",
        "d30": "近 30 天内更新",
        "d90": "近 90 天内更新"
      },
      "sincePrefix": "自",
      "sinceSuffix": "起更新",
      "sinceAria": "增量起始日期",
      "sortAria": "排序方式",
      "sort": {
        "updatedDesc": "排序：最后更新时间 ↓",
        "prioDesc": "排序：严重度优先 (S0→S3, 易→难)",
        "numDesc": "排序：Issue 编号 ↓ (最新创建)",
        "numAsc": "排序：Issue 编号 ↑ (最早积压)"
      }
    },
    "pills": {
      "REAL_ALL": "有效真实 Bug (全部)",
      "EASY": "Easy 简单",
      "MEDIUM": "Medium 中等",
      "HARD": "Hard 困难",
      "ENHANCEMENT": "实为 Enhancement",
      "FILTER_RANDOM_FUZZ": "筛除: Random/Fuzz",
      "FILTER_AI_OBSCURE": "筛除: AI 极端边界",
      "FILTER_NON_OPT_OR_FIXED": "筛除: 已修/非优化器",
      "ALL": "全部显示"
    },
    "status": {
      "count": "当前筛选命中 {matched} 条 Issue（口径内共 {scoped} 条）",
      "clickHint": "Issue 与 PR 标签均可直接点击打开 GitHub 页面（悬停 PR 可查看完整阻塞归因与标题）"
    },
    "table": {
      "colIssue": "Issue",
      "colCatSev": "难度 & 严重度",
      "colSub": "所属子模块",
      "colTitle": "标题、研判说明与可点击关联 PR",
      "colTime": "最后更新 / 创建信息",
      "ghSevNote": "GH原标: {val}",
      "ghUnlabeled": "未标",
      "tagCustomer": "客户反馈",
      "tagBorderline": "边界保留",
      "tagSupplemental": "补充池",
      "updatedPrefix": "更新 {time}",
      "createdPrefix": "创建 {date}",
      "empty": "无匹配 Issue，请调整筛选条件或点击重置矩阵"
    },
    "prBoard": {
      "summaryLine": "球权分布：👀 卡在 Reviewer {reviewer} · 🛠️ 卡在 Author {author} · 🚀 待合入 {queue} · ⏸️ 挂起/草稿 {hold}",
      "cards": {
        "ALL": "全部在途 PR",
        "NEEDS_LGTM2": "⚡ LGT1 差一票",
        "WAITING_REVIEWER": "👀 待首审 / 复审",
        "NEEDS_OK_TO_TEST": "🔑 待 ok-to-test",
        "CI_FAILED": "🔴 CI 失败",
        "CONFLICT": "💥 代码冲突",
        "WAITING_AUTHOR": "🛠️ 等作者回应",
        "GATE_BLOCKED": "🚧 门禁缺项",
        "READY_MERGING": "🚀 批准待合入",
        "DRAFT_OR_HOLD": "⏸️ 草稿 / Hold"
      },
      "filters": {
        "searchPlaceholder": "搜索 PR #、关联 Issue #、标题、@作者、@审阅人 或失败 CI 名称…",
        "scopeMainline": "主干在途 PR ({count})",
        "scopeRealBugMainline": "仅真实有效 Bug 主干 PR ({count})",
        "scopeAllOpen": "全部开放 PR · 含 Cherry-Pick ({count})",
        "ballAll": "全部球权归属",
        "ballReviewer": "👀 球在 Reviewer（待审 / 待放行 / 差1票）",
        "ballAuthor": "🛠️ 球在 Author（修 CI / 解冲突 / 回评论）",
        "ballHold": "⏸️ Hold / 草稿挂起",
        "ballQueue": "🚀 已批准排队列",
        "sortPrio": "排序：关联高危 Bug 优先 (S0→S3)",
        "sortWaitDesc": "排序：当前球权等待天数 ↓",
        "sortUpdDesc": "排序：PR 最后更新时间 ↓",
        "sortNumDesc": "排序：PR 编号 ↓ (最新)"
      },
      "statusCount": "当前命中 {matched} 个在途 PR（当前范围共 {scoped} 个）",
      "clickHint": "点击失败 CI 胶囊可直达 Jenkins / GitHub Actions 构建日志；点击 🔍 可定位回对应 Issue",
      "cols": {
        "pr": "PR / 作者",
        "blocker": "当前阻塞归因 & 球权",
        "issues": "关联 Issue & 严重度",
        "details": "标题、审阅进展 & 失败 CI 直链",
        "time": "最后活跃 / 提交"
      },
      "ballTag": {
        "REVIEWER": "球在 Reviewer",
        "AUTHOR": "球在 Author",
        "MERGE_QUEUE": "合并队列",
        "HOLD": "暂挂 Hold"
      },
      "waitDays": "已等 {days}d",
      "waitToday": "<1d",
      "ciRunning": "{count} 项 CI 运行中",
      "ciMore": "+{count} 项失败",
      "revLgtm": "LGTM: {users}",
      "revChanges": "需修改: {users}",
      "revThreads": "未决评论({count}): {users}",
      "revRequested": "待审: {users}",
      "lastCommit": "提交 {date}",
      "empty": "当前筛选条件下没有匹配的在途 PR",
      "chipShort": {
        "DRAFT_OR_HOLD": "⏸️ 挂起",
        "CONFLICT": "💥 冲突",
        "NEEDS_OK_TO_TEST": "🔑 待放行",
        "CI_FAILED": "🔴 CI挂({count})",
        "WAITING_AUTHOR": "🛠️ 等作者",
        "GATE_BLOCKED": "🚧 卡门禁",
        "NEEDS_LGTM2": "⚡ LGT1",
        "WAITING_REVIEWER_zero": "👀 0审阅",
        "WAITING_REVIEWER_re": "👀 待复审",
        "WAITING_REVIEWER": "👀 待审",
        "READY_MERGING": "🚀 待合入"
      },
      "detail": {
        "draft": "PR 处于草稿状态 (Draft)",
        "hold_label": "带有挂起标签: {gates}",
        "merge_conflict": "存在分支合并冲突，需作者 Rebase",
        "needs_ok_to_test": "外部贡献待 Maintainer 评论 /ok-to-test 放行 CI",
        "ci_failed": "{count} 项 CI 检查失败，需排查重跑或修代码",
        "changes_requested": "@{users} 提出修改意见 (Changes Requested)，待作者更新",
        "unresolved_thread": "存在 {count} 处未解决审阅讨论 (@{users})，待作者回应或推新提交",
        "gate_label": "代码已获审阅认可，但卡合并门禁: {gates}",
        "needs_second_lgtm": "已获 @{users} 第 1 票 LGTM，差第 2 票 Approve",
        "needs_second_lgtm_anon": "已有 lgtm 标签，差第 2 位 Reviewer Approve",
        "zero_review": "尚无人类 Reviewer 审阅，待首次接单审阅",
        "re_review": "作者已推新 Commit，待 @{users} 复审",
        "awaiting_review": "已有审阅互动 (@{users})，尚待给出 LGTM",
        "ci_running": "已获批 (Approved)，正在跑收尾 CI ({count} 项)",
        "merge_queue": "已获批且检查通过，正在排 Prow Tide 合并队列"
      }
    },
    "enums": {
      "cat": {
        "EASY": "真实 Bug · Easy",
        "MEDIUM": "真实 Bug · Medium",
        "HARD": "真实 Bug · Hard",
        "ENHANCEMENT": "实为 Enhancement",
        "FILTER_RANDOM_FUZZ": "已筛除 · Random/Fuzz",
        "FILTER_AI_OBSCURE": "已筛除 · AI Corner Case",
        "FILTER_NON_OPT_OR_FIXED": "已筛除 · 非优化器/已修/无效"
      },
      "diff": {
        "EASY": "简单 (1~3天)",
        "MEDIUM": "中等 (3~7天)",
        "HARD": "困难 (1~3+周)",
        "ENHANCEMENT": "功能/规则增强",
        "FILTER_RANDOM_FUZZ": "随机/差分测试极端用例",
        "FILTER_AI_OBSCURE": "AI 静态猎奇/极端边界",
        "FILTER_NON_OPT_OR_FIXED": "其他模块/已合入/Flaky"
      },
      "severity": {
        "CRITICAL": "Critical · 致命",
        "MAJOR": "Major · 严重",
        "MODERATE": "Moderate · 中等",
        "MINOR": "Minor · 轻微",
        "N/A": "N/A"
      },
      "subsystem": {
        "statistics": "统计信息与基数估算",
        "plan_cache_spm_hint": "Plan Cache / SPM / Hint",
        "logical_rewrite": "逻辑改写与子查询",
        "access_path_partition_lock": "访问路径 / 分区 / 锁",
        "cbo_cost_join_reorder": "CBO 代价与连接重排",
        "name_resolution_group_by": "名称解析与 GROUP BY 校验",
        "tiflash_mpp": "TiFlash / MPP"
      }
    }
  }
});
