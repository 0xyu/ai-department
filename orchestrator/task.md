# ACO 任务进度

实施计划：[AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md](AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md)

| 阶段 | 标题 | 状态 |
|---|---|---|
| Phase 1 | 项目基础、配置与 CLI 骨架 | 已完成（三平台 CI 尚未在 GitHub 上运行验证） |
| Phase 2 | 计划包模型、解析器与确定性检查 | 已完成（三平台 CI 尚未运行验证） |
| Phase 3 | 状态存储、事件、产物、状态机与运行锁 | 已完成（三平台 CI 与 `-race` 尚未运行验证） |
| Phase 4 | Git 管理 | 已完成（三平台 CI 与 `-race` 尚未运行验证） |
| Phase 5 | Claude Code 适配器 | 已完成（订阅模式实测通过；API 模式实测、三平台 CI 与 `-race` 尚未运行） |
| Phase 6 | 质量门执行器 | 已完成（staticcheck/gosec/govulncheck 本次未运行；三平台 CI 与 `-race` 尚未运行） |
| Phase 7 | 基于 Codex CLI 的 GPT 评审 | 未开始 |
| Phase 8 | 第 1、2 阶段流水线 | 未开始 |
| Phase 9 | 第 3 阶段单阶段引擎 | 未开始 |
| Phase 10 | 运行生命周期与恢复 | 未开始 |
| Phase 11 | 第 4 阶段最终验证 | 未开始 |
| Phase 12 | 安全加固与沙箱 | 未开始 |
| Phase 13 | HTTP API 与 Go 客户端 | 未开始 |
| Phase 14 | MCP、AI Department 插件、发布 | 未开始 |

## Phase 1 完成内容

- Go 模块 `github.com/0xyu/aco`（Go ≥ 1.22，唯一外部依赖 `gopkg.in/yaml.v3`，无 CGO）。Phase 3 起最低 Go 版本为 1.26，并新增 `modernc.org/sqlite`（见下）。
- `cmd/aco` 与 `internal/cli`：§17.1 的全部命令；`doctor` 和 `version` 已实现，其余为桩（退出码 2，提示"尚未实现"）。
- `internal/config`：§18 的配置结构、§14.3 的默认限额、六层加载（默认 ← 用户 ← 仓库 ← `--config` ← 计划前置元数据 ← `--set`）、带层/文件/行号/字段路径的错误；未知字段、未知质量门类型、疑似密钥字段均报错。
- `internal/auth`（第一部分）：环境文件解析、按 §16.5 顺序解析认证方式、API 密钥存在性检查、回退策略。
- `internal/i18n`：操作员文本目录（默认 zh-CN，可切换 en），测试保证所有输出都经过目录。
- `internal/logging` + `internal/redact`：带脱敏的 slog 处理器。
- `internal/doctor`：v1 环境检查。
- CI：`.github/workflows/aco-ci.yml`（Linux / Windows / macOS，gofmt、vet、staticcheck、依赖检查、`-race` 测试）。
- 文档：`docs/config.md`、`docs/cli.md`。

## 代码评审修复（Phase 1）

> 其中关于日志值转换、字节扫描和 panic 处理的条目，已被下面"统一安全工具层"的日志白名单取代（相关代码已删除），保留在此作为历史记录。

- `--set` 的错误来源只显示选项和字段路径（如 `--set auth.api_key`），不再回显值；格式错误的 `--set` 按位置显示（`--set #2`）。所有 CLI 输出都经过脱敏过滤。
- 日志脱敏覆盖实际输出的表示：`MarshalJSON`、`MarshalText`、`%+v` 和 `Error()`；JSON 日志在脱敏后仍是合法的结构化 JSON。
- 配置文件、计划前置元数据和 `--set` 值只允许一个 YAML 文档；多余的文档或其后的错误 YAML 报解析错误并给出行号。
- 价格、预算和比例字段拒绝 `.nan`、`.inf`、`-.inf`。
- CLI 支持 `--`：其后的所有参数都是位置参数；作为参数值的 `--help` 不再被当成帮助请求。
- 整数和布尔参数的值无效时，只报告参数名和期望类型，不再转发 Go flag 包的错误文本（其中含有转义后的值，字面量脱敏无法识别）。
- 日志处理器对每个值只转换一次（按 JSON 或文本日志自身的规则），输出的就是脱敏后的结果，不会在检查之后再次调用 `MarshalJSON`、`Error()` 等方法；属性键、嵌套分组键和 `WithGroup` 名称也会脱敏。
- 字节切片（包括命名的字节切片类型和嵌套在结构体中的字节切片）在 Base64 编码前先脱敏；干净的二进制数据照常输出。
- 自定义转换（`Error()`、`MarshalJSON`、`MarshalText`、`Format`）发生 panic 时就地恢复，输出固定占位符 `!PANIC: log value conversion failed`，不含 panic 内容，记录的其余字段照常写出。
- 文本日志在格式化前通过反射（不调用任何方法）检查嵌套的字节切片和字节数组；含密钥时改用脱敏后的 JSON 形式（字节数组则输出固定占位符），不再输出可还原的十进制字节列表。JSON 日志中的字节数组同样处理。
- 文本日志不再按子串 `(PANIC=` 判断 panic：顶层的 `Format`/`Error`/`String` 由处理器自己在 recover 保护下调用；嵌套值只匹配 fmt 确切的 panic 输出格式。普通错误消息中出现 `(PANIC=` 时原样保留。
- 字节扫描改为"失败即封闭"：超过深度（32）或节点预算（10000）时视为检查不完整，该属性输出占位符；切片去重同时区分地址、类型和长度（`s[:1]` 与 `s` 是不同视图），仍有循环保护；不可能含字节数据的类型（字符串、`[]string` 等）不占用预算。
- 识别会被编码成数字列表的"类字节"数据：字节数组、整数切片、元素实现了 `MarshalJSON` 的命名字节类型、由字节组成的 `[]any`。含密钥时，若所选输出方式无法脱敏则输出占位符，且不会调用这些元素的自定义转换。

## 统一安全工具层（`internal/secure`）

前几轮评审反复发现泄露，根因是"输出前在任意值里查找秘密"这种做法无法穷尽。现改为"源头标记类型 + 出口白名单"，详见 [docs/security.md](docs/security.md)。

- `secure.Secret`：API 密钥读入时即为此类型，任何打印或编码方式都只输出 `[REDACTED]`；原值存放在包内私有表中，只能通过 `Reveal()` 取出，且只有白名单中的包可以调用。
- `secure.Redactor`（原 `internal/redact`）：除原文外，还会屏蔽已登记秘密的引号转义、JSON 转义和 URL 转义形式；可并发使用。
- 日志改为白名单：只输出字符串、数字、布尔、时长、时间、以这些为底层类型的命名类型、`[]string`、`error`（只调用一次 `Error()`）、分组和 `LogValuer`；其他类型输出 `[UNLOGGABLE <类型>]`，且不调用其任何方法。原来的反射扫描代码已删除。
- 诊断信息不回显可疑输入：`Redactor.Echo` 只放行短的、字符安全的、且不含任何已登记秘密（包括短于 8 个字符、不参与文本屏蔽的秘密）的值，其他显示 `[hidden]`；适用于配置错误、认证错误、未知命令、未知参数和多余参数。
- 字段路径只由可信部分组成（`config.DisplayPath`）：配置结构中的字段名、列表下标和固定的密钥字段名（`api_key`、`token` 等）；未知键和映射键显示为 `[hidden]`（如 `planner.[hidden]`），仍给出文件和行号。`--set` 的键不完全可信时按位置显示（`--set #2`），未知质量门类型也不回显。
- 日志的分组与 `LogValuer` 展开按整条记录共享预算（嵌套最多 16 层，`LogValue` 调用最多 256 次），超出部分输出 `[TRUNCATED: log value expansion limit]`；自我包含的 `LogValuer` 不再导致栈溢出，普通同级属性照常输出。
- 守卫测试：`Reveal`、创建 slog 记录器、标准库 `log`、直接读取环境变量、在非测试代码中导入 `securetest`，都只允许出现在白名单位置；对 `os`、`syscall`、`log`、`log/slog` 的点导入（`import . "os"`）一律拒绝。
- 共享泄露语料 `securetest`：4 个假凭据 × 多种编码（原文、引号转义、JSON 转义、URL 转义、十六进制、十进制列表、三种对齐的 Base64）。CLI 语料测试覆盖参数、`--set`、位置参数、环境变量、配置文件和环境文件，文本与 JSON 日志都测。
- 后续阶段新增任何输出（事件、报告、提示、提交信息、HTTP/MCP）时，必须接入同一个 Redactor，并用 `securetest` 加一条泄露测试。

## Phase 2 完成内容

详见 [docs/plan-format.md](docs/plan-format.md)。

- `internal/plan`：计划包模型（需求、阶段规格、验收标准、质量门条目、追溯表）。三种输入形式得到同一个模型（不含原文和位置）：
  - 计划包目录 `.aco/plan/`（`IMPLEMENTATION_PLAN.md`、`requirements.md`、`traceability.json`、`phases/PNN.md`）；
  - 单个 Markdown 计划（本实施计划文档即可直接解析为 14 个阶段，每个阶段至少 3 条验收标准，无解析错误）；
  - YAML / JSON 结构化文档（`schemas/plan.schema.json`，也是第 1 阶段规划器结构化输出的格式）。
- 解析规则：识别 `Phase N: Title` 标题和各小节，跳过代码块中的标题；提取 `REQ-NNN`（列表或表格，类型可由小节标题推断）；验收标准没有显式 ID 时按位置编号为 `AC-NN.M`；识别依赖、`scope.paths`、六类质量门条目（命令、`required`、带理由的 `not_applicable`、安全扫描器）。
- 每个阶段规格的 Markdown 原文按字节保留（保留原换行符），供提示词使用；哈希前去掉 BOM 并统一换行符，同一计划在各操作系统上 ID 和哈希一致。
- 确定性检查（§10.4）：编号连续、ID 唯一、依赖只能指向更早阶段、无循环依赖、必需小节齐全、六类质量门齐全且 `not_applicable` 必须有理由、每个需求都被阶段和验收标准覆盖、追溯表引用均存在，以及阶段规模提示（警告）。检查结果是结构化的（`code`、`severity`、`location`、`message`），可直接交给 GPT 和 Claude。
- 前置元数据接入配置的 plan 层并做校验（`front_matter_invalid`）。
- AGENTS.md 发现与哈希（拒绝符号链接）；冻结清单 `plan.lock.json` 的生成、写入、解析与校验：修改、删除或新增任何冻结文件或 AGENTS.md 时，校验失败并给出文件名；清单中的路径受限于 `.aco/plan/**` 和 `AGENTS.md`，被篡改的清单无法让 ACO 读取其他文件。
- `plan.WriteDir` 可把任意形式的计划写成计划包目录，读回后模型不变（供 Phase 8 写出 Claude 的结构化输出）。
- `aco validate-plan <文件|目录> [--strict] [--json] [--repo]` 已实现：中文（或英文）摘要、阶段列表和问题列表，或 `aco.validate-plan/v1` JSON；退出码 0 通过、1 未通过、2 用法错误。`--json` 输出有黄金文件测试。
- 新增 i18n 键：`validate-plan` 输出和每个检查代码的中英文消息；测试保证英文目录与计划包中的英文消息一致。
- 泄露测试：计划文本中的已登记密钥和凭据模式在文本与 JSON 输出中都被屏蔽。
- `.gitattributes`：测试夹具和黄金文件在 Windows 检出时不做换行符转换。

## 代码评审修复（Phase 2）

- 仓库包含性：`.aco`、`.aco/plan`、包内的文件和目录以及 `plan.lock.json` 只要是符号链接、Windows 目录联接（junction）或其他重解析点，就一律拒绝，绝不跟随。加载计划包时在读取任何内容之前拒绝；冻结清单的生成、读取、写入和校验在读写外部目标之前返回 `ContainmentError` 并指出出问题的路径；`validate-plan` 报告 `path_not_contained`。已有的清单条目路径校验保持不变。回归测试在 Windows 上使用真实的 junction，覆盖被链接的 `.aco`、被链接的 `.aco/plan`、被链接的 `phases` 目录和被链接的清单文件。
- 追溯一致性：追溯项引用的验收标准若不属于该项列出的阶段，即为错误（`trace_ac_outside_phases`，默认模式即失败，不需要 `--strict`）；需求只有在至少存在一对有效的“阶段—验收标准”时才算被覆盖。显式追溯表和由 Markdown 推导的追溯都适用；阶段只能为需求列出自己的验收标准。
- 实施计划文档已能通过 `aco validate-plan`（默认与 `--strict` 均退出码 0）：新增 §4.1 分类需求列表 `REQ-001`–`REQ-030`（17 条功能需求、10 条非功能需求、3 条约束），每个阶段新增 Requirements 小节（映射到具体验收标准）和六类质量门；Phase 1、2 的 integration 为 `not_applicable` 并给出各自的理由。14 个阶段原有的验收标准、目标、范围、交付物和依赖均未改动（已逐项比对）。
- 空验收标准：只有 ID（如 `AC-01.2:`）、只有空白或 YAML/JSON 中 `text` 为空的验收标准报 `empty_acceptance_criterion` 错误（含 AC ID 和位置），且绝不计入需求覆盖。
- 质量门命令保留原有空白：只规范化质量门名称和控制关键字，命令内容（例如带重复空格的引号参数）原样保留；Markdown 与 YAML 等价性有回归测试。

## 复审修复（Phase 2）

- 关闭两个 CLI 包含性绕过：（a）`.aco` 是 junction 或符号链接时，加载被拒绝后 `CheckLock` 不再读取外部的 `plan.lock.json`，因此不会出现由外部清单内容产生的诊断；（b）显式 `--repo` 不再跳过检查：对位于 `<根>/.aco/plan` 的来源，始终按路径本身指出的根检查 `.aco` 和 `.aco/plan`。不在 `.aco/plan` 下的普通计划目录照常可用。CLI 回归测试覆盖了带畸形外部清单的 junction 和 `--repo` 指向其他目录两种情况。
- 分组的需求条目：`- REQ-004, REQ-001: AC-02.1` 中列出的验收标准适用于条目中的每个需求，并逐一按声明该条目的阶段校验（跨阶段时报 `trace_ac_outside_phases`，默认模式即失败）；未列出验收标准的分组条目仍回退为该阶段的全部验收标准。语法已写入 docs/plan-format.md。

## Phase 3 完成内容

详见 [docs/lifecycle.md](docs/lifecycle.md)。

- 新依赖 `modernc.org/sqlite` v1.61.1（纯 Go，无 CGO）；它要求 Go ≥ 1.26，因此 `go.mod` 的 Go 版本从 1.22 提升到 1.26（CI 使用 stable，不受影响）。
- `internal/state`：SQLite 状态库（WAL、`synchronous=FULL`、外键、`busy_timeout`、立即写事务，多个进程可共用一个文件）；内嵌迁移（`migrations/0001_init.sql`），遇到更新版本的数据库拒绝打开。表：runs、stages、plan_reviews、phases、attempts、steps、invocations、gate_results、reviews、findings（计划与代码问题共用，含处理分类）、commits、reports、events、locks。
- 运行状态机（§13）与阶段状态机（§11.2）是纯函数；存储层的所有状态变更都经过状态机，并在同一事务中记录阶段条目和对应事件；非法转换返回 `ErrIllegalTransition`，数据库不变。
  - 相对 §13 的简略图补充了：所有阶段都可进入 BLOCKED（包括 Planning，`plan_invalid`）；所有活动状态都可进入 FAILED 和 CANCELLED；暂停后只能回到暂停前的状态；BLOCKED/FAILED 由人工恢复（`ResumeRun`，默认回到原因中记录的阶段）。§13 图下已加说明。
  - 相对 §11.2 补充了：实现中和评审中可触发尝试用尽；环境类质量门失败直接阻塞；提交门在计划完整性或策略复查失败时阻塞。阶段只在运行处于 executing 时变更；进入 initializing 总是开始新的尝试。
- `blocked` 与 `failed` 是不同的终止状态，带结构化原因（`class`、`stage`、`phase`、`detail`）；两者的 class 列表互不重叠，在 execution 阶段阻塞必须指明阶段号，`detail` 写入前脱敏。
- 意图/结果步骤 API：`BeginStep` 先持久化意图再返回，`CompleteStep`/`FailStep` 写结果；同一 `Key` 重复调用返回已有步骤（幂等）；接管运行锁后 `MarkInterrupted` 把其他持有者遗留的 started 步骤标为 `needs_reconciliation`，再由 `ResolveStep` 记录处理方式。
- 运行锁：每个仓库（规范化路径，大小写不敏感的系统上统一小写）同时只有一个持有者；心跳（默认 TTL 30 秒，`KeepAlive` 每 TTL/3 一次）；心跳超时的锁会被接管并返回原持有者；被接管的一方心跳或释放时得到 `ErrLockLost`。
- `internal/events`：§13 的全部 56 种事件类型（封闭集合）；类型化负载（只允许字符串、整数、布尔、字符串列表，不做反射序列化）；`aco.event/v1` JSON 编码与 `schemas/event.schema.json`（测试保证枚举与代码一致）；每种事件都有中英文操作员消息（新增 56 个 i18n 键）。
- 每个运行的事件序号单调递增、无空洞（在追加事务中分配）；订阅者从数据库读取，可从任意 `seq` 重放，迟到或读得慢都不会漏掉或重复；本进程追加立即唤醒，其他进程追加通过轮询（默认 1 秒）获取。
- `internal/artifacts`：运行目录布局（`<root>/runs/<run-id>/…`）与路径辅助函数；原子写入（同目录临时文件、fsync、rename，文件 0600、目录 0700）；写入前脱敏（流式写入在提交时整体脱敏，跨块的密钥也能屏蔽）；路径限制在运行目录内，拒绝符号链接、junction 和其他重解析点；单个产物上限 64 MiB。
- 测试：两个状态机的每一对（from, to）转换；Planning ↔ PlanReview 循环；从每个阶段进入 BLOCKED；真实子进程的锁竞争、被杀进程的锁过期接管，以及“在意图和结果之间被杀”的测试（`-short` 时跳过）；迟到订阅者、并发发布、跨进程事件；数据库每一列、事件和产物的泄露测试；`-tags=integration` 的端到端测试。
- 本机已运行：`gofmt`、`go vet`（含 integration 标签）、`staticcheck`（无问题）、全部测试、integration 测试、`CGO_ENABLED=0 go build`、依赖检查、`govulncheck`（代码不受影响；报告的 14 项均在本机 Go 1.27.1 标准库中，升级到 1.27.3 即修复）。

## Phase 4 完成内容

详见 [docs/git.md](docs/git.md)。

- `internal/gitops`：Git 管理器，只有编排器调用 Git。
  - 每次调用都有超时（默认 2 分钟），设置 `GIT_TERMINAL_PROMPT=0` 等禁止交互的变量；关闭仓库钩子（`core.hooksPath` 指向不存在的目录）和 fsmonitor；差异不使用外部 diff 和 textconv。提交用 `commit-tree` + `update-ref`，因此提交钩子不会运行。
  - 子进程环境去掉所有密钥类变量（只有 push/PR 保留 `GITHUB_TOKEN`/`GH_TOKEN`）以及 `GIT_DIR` 等重定向变量；提交信息、标签信息、notes 和错误中的 stderr 都经过脱敏；错误只写 git 子命令，不写参数。`internal/gitops/exec.go` 因此加入了环境变量读取白名单。
  - 预检（git ≥ 2.30、非裸仓库、基础分支存在、无进行中的合并/变基、工作树路径可用）；运行分支名 `aco/<yyyymmdd>-<plan-slug>-<short-id>`；运行工作树与第 4 阶段的分离 HEAD 干净工作树；工作树删除。
  - 工作状态树哈希（HEAD + 所有已跟踪改动 + 未忽略的未跟踪文件），使用临时索引，不改变工作树自己的索引。
  - 检查点 `refs/aco/<run>/PNN/aK/sM`；`ResetTo` 精确恢复到检查点的树（覆盖/删除已跟踪文件，删除未忽略的未跟踪文件，保留被忽略的文件，HEAD 不动，索引回到 HEAD），并校验结果。
  - 差异：带重命名检测的变更列表（状态、大小、增删行数、是否二进制）、补丁、统计、变更路径。
  - `CommitReviewed(paths, expectedTree, extraFiles)`：暂存结果必须等于评审时的树（加上报告文件），整个工作状态也必须等于它，否则报 `ErrTreeMismatch` 或 `ErrUnreviewedChange`；分支用比较并交换方式推进（被外部移动时报 `ErrBranchMoved`）；遵守 `commit.gpgSign`。冻结提交用 `RequireExact`。
  - §10.5 冻结提交和 §11.8 阶段提交的信息模板与全部 trailer；主题和 trailer 值中的换行会被合并，无法注入额外 trailer；`FindCommitByTrailer` 用于避免重复提交同一次尝试。
  - 附注标签（同一提交重复打标签无操作，指向其他提交则报错）、`git notes --ref=aco`、阻塞阶段的 WIP 引用 `refs/aco/<run>/blocked/PNN`、影子引用一次事务清理（可保留阻塞引用）、可选的 push（从不强推）和 `gh pr create`。
- `internal/policy`（第一部分）：差异策略，结果为结构化发现（`code`、`path`、`line`、`rule`、`blocking`、`message`），发现中不包含密钥原文。
  - 受保护路径（大小写不敏感）、`scope.paths` 之外的路径（`flag`/`block`）、超过 `policy.max_file_bytes` 的文件、二进制文件（`policy.binary_files`）、密钥文件名、新增行中的密钥（AWS、私钥、Anthropic、OpenAI、GitHub、Slack、Google、Stripe、已登记的密钥值；通用赋值规则只提示不阻断）。
  - 反作弊：删除测试文件（含改名为非测试文件）、`t.Skip`、`@pytest.mark.skip`、`it.skip`/`.only`、`@Disabled`、`eslint-disable`、`#nosec`、`nolint`、`noqa`、`@ts-ignore` 等。
  - `tests_missing`：改了源代码但没有新增或修改任何测试或测试数据。
  - 路径模式规则与 `.gitignore` 一致（不含 `/` 的模式匹配任意层级的路径段，含 `/` 的模式从根开始，`**` 匹配任意层目录，匹配目录即覆盖其中所有文件）。
- 配置新增 `policy.max_file_bytes`（默认 5 MiB）和 `policy.binary_files`（`warn`|`fail`，默认 `warn`），已写入 docs/config.md。
- CI 新增 `go test -tags=integration ./...` 步骤。
- 测试：在隔离 Git 配置的临时仓库中使用真实 Git。覆盖预检、临时索引不改动工作树索引、检查点与精确恢复、评审树提交的每种拒绝情况、用 `git interpret-trailers` 解析 trailer、标签与 notes、重复提交检测、阻塞引用与清理、分离工作树、环境变量清理、钩子不运行、超时、脱敏，以及每条策略规则。`-tags=integration` 的 `TestSimulatedRun` 跑完整流程，并验证用户检出（文件、`.git/index`、`HEAD`、基础分支、`git status`，包括用户未提交的改动）前后逐字节一致。
- 本机已运行：`gofmt`、`go vet`（含 integration 标签）、`staticcheck`（含 integration 标签，无问题）、全部单元测试与 integration 测试、`CGO_ENABLED=0 go build`、对新包运行 `gosec`（无问题）。`govulncheck` 本次未运行。

## 代码评审修复（Phase 4）

- `.gitattributes` 无法再绕过策略检查：`policy.Collect` 不再解析 Git 的补丁输出，而是读取每个变更文件新旧版本的原始 blob（`gitops.ReadBlob`，不做任何转换、过滤或 textconv），用自己的逐行差异（Myers 算法）得到新增行。因此即使文件被标为 `-diff`，密钥和反作弊检查仍逐行进行；二进制检查（`binary_files` 的 warn/fail）保持独立。新增行仍按差异语义计算并给出新文件中的行号；超过 1000 处编辑时，公共前后缀之间的所有行都按新增处理（只会多报，不会漏报）。回归测试修改 `.gitattributes` 为 `*.go -diff`，在 `binary_files: warn` 下加入假 AWS 密钥和 `t.Skip`，二者都作为阻断发现报出。
- 外部 Git 过滤器不再执行：每个操作开始时读取已配置的过滤器驱动，并在该操作的每一次 git 调用中以 `-c filter.<驱动>.clean=`/`.smudge=`/`.process=` 置空（也会传给 Git 的子进程，例如 `worktree add` 内的检出）。文件按原始字节暂存和检出，其他属性处理（`text`、`eol` 等）保持不变。标为 `required` 的过滤器（如 Git LFS）无法安全跳过，返回 `ErrFilterUnavailable`；创建工作树失败时会删除残留的工作树和分支。回归测试配置会写标记文件的 clean、smudge、process 过滤器，覆盖工作树创建、树哈希、检查点、status、双向重置、评审提交和第 4 阶段工作树，标记文件都不出现；对照步骤证明普通 git 会运行同一个过滤器。
- 工作树归属检查：ACO 创建工作树时在其私有 git 目录写入归属标记 `aco-worktree`。`OpenWorktree(ctx, path, branch)` 必须确认路径是同一仓库的链接工作树、带有对应角色和分支的标记、HEAD 在运行分支上（或分离）。`Commit`/`CommitReviewed` 只接受运行工作树，且只推进其运行分支；`ResetTo` 接受运行工作树和验证工作树；`RemoveWorktree(ctx, wt)` 现在接收工作树对象。所有这些在写入之前都重新检查，拒绝用户检出、用户自己创建的工作树、其他仓库的工作树，以及 HEAD 已离开运行分支的运行工作树（`ErrNotOwnedWorktree`）。回归测试包括手工伪造的对象，并验证用户的文件、索引、HEAD、基础分支以及用户自己的工作树前后逐字节一致。分离的验证工作树仍可创建、重置、重新打开和删除。
- reflog 不再泄露提交主题：运行分支提交的 reflog 使用固定消息 `aco: commit`。脱敏测试在主题中加入已登记的假密钥，同时检查提交信息和分支 reflog。
- 差异收集失败即封闭：旧的补丁解析器（`ParsePatch`）已删除。新的读取方式对行长没有限制，任何读取错误都会让 `Collect`/`CheckDiff` 返回错误；新旧 blob 超过 64 MiB 的文件不扫描，并给出阻断的 `scan_incomplete` 发现。回归测试在后面的假密钥之前放了一行 3 MiB 的长行。
- 已知限制：需要 required 过滤器（Git LFS）的仓库在 v1 中无法使用；归属标记依赖 agent 不能写 `.git`（Phase 12 的守卫钩子负责强制）。

## Phase 5 完成内容

详见 [docs/claude-adapter.md](docs/claude-adapter.md)。以 Claude Code 2.1.311 实测核对了参数和 stream-json 字段。

- `internal/agent`：`Planner.Create/Revise`、`Implementer.Start/Continue` 接口与共用类型（请求、结果、用量、按范围计的 USD 预算、§14.1 错误类别）；实现报告结构 `schemas/impl-report.schema.json`，Go 类型与 Schema 由测试保持一致。
- `internal/agent/claudecode`：
  - 两种执行配置的命令构建（黄金文件测试）：规划器 `--effort high`，只有 `Read,Glob,Grep`（`--permission-mode dontAsk`）；实现器 `--model claude-opus-5-5 --effort medium`、`acceptEdits`。两者都用 `--setting-sources ""` 和 `--strict-mcp-config`（仓库里的 `.claude/settings.json` 和 MCP 服务器不生效）、`--permission-prompts none`、`--include-hook-events`。提示词只走 stdin。
  - 能力探测（`--version`/`--help`）；effort 优先用 `--effort`，否则写入设置文件的 `effortLevel`；`strict_effort` 时没有 `--effort` 直接失败。
  - 生成的设置文件对所有工具注册 PreToolUse 钩子 `aco hook pre-tool-use --run … --policy …`；钩子程序不存在时拒绝启动。从事件流确认钩子确实运行：有工具结果却没有任何钩子响应（`-p` 模式会静默忽略无效设置），或钩子退出码既不是 0 也不是 2，都立即停止调用。
  - 进程树管理（新包 `internal/proc`）：Unix 进程组；Windows 作业对象（进程挂起状态下加入作业，再恢复，保证所有子进程都在作业内）。取消、总超时、空闲超时以及正常结束后都会结束整棵进程树。
  - stream-json 解析器容忍未知消息类型和字段；校验 init 消息的 `apiKeySource`（订阅为 `none`，API 为 `ANTHROPIC_API_KEY`），不一致立即停止（`auth`）；规划器会话出现写工具时停止。
  - 结构化输出提取（`structured_output`，否则从结果文本中找 JSON）；规划包按计划 Schema 解析，修订时额外要求 `finding_responses`；实现报告严格校验并要求覆盖所有验收标准和所有问题 ID，否则为 `incomplete`（结果仍返回，便于续接会话）。
  - 错误分类：transient、subscription_limit（含重置时间：rate_limit_event、"resets 3pm (Europe/Berlin)"、`|<unix>`、"try again in …"）、api_rate_limit（含 Retry-After）、api_quota、max_turns、incomplete、crash、timeout、auth、budget_exceeded、cancelled。
  - API 模式记录 `total_cost_usd` 并计入 `budget_usd`（失败的调用也计入）；预算用完时在调用前拒绝，否则把剩余额度作为 `--max-budget-usd` 传给 CLI。
  - 提示词、系统提示、设置、guard 策略、转录、stderr 都写入运行产物目录并经过脱敏。
  - 传给 CLI 的 Schema 去掉根 `$schema`/`$id`（实测：CLI 的校验器不认识 draft 2020-12 元 Schema）；`plan.schema.json` 的 `then` 分支补上了 `type`（语义不变，消除 CLI 的 strict 警告）。
- `internal/policy` 第二部分：guard（`guard.go`）和一个只读的 shell 解析器（`shell.go`：管道、`;`/`&&`/`||`、子 shell、引号、`$(…)` 与反引号递归检查、`bash -c`、重定向、heredoc、`env`/`timeout`/`xargs` 等包装命令）。阻止：工作树之外的路径（含 `~`、`$HOME`、符号链接逃逸、通过变量赋值的路径）、`.git`、受保护路径（工具、重定向、`mv`、`cp` 目标、`sed -i`、`tee`、`dd of=`、`find -delete`/`-exec rm {}`）、`block` 模式下 scope 之外的写入、只读以外的全部 git 子命令和 `git -c`、`sudo` 等、`network` 策略禁止的网络工具、下载后管道给解释器、`eval`、无法检查的 `cmd /c`/`powershell -Command`、无法可靠解析的命令、规划器的任何写入或命令、未知工具（MCP、子代理、PowerShell）。
- `aco hook pre-tool-use`：从 stdin 读工具调用 JSON；放行时退出 0 且无输出，阻止时退出 2 并在 stderr 给出 `ACO guard: …` 原因；拒绝记录追加到 deny 日志（只记工具、规则、路径，不记命令原文）。策略缺失、属于其他运行、输入无法解析、事件类型不符时一律阻止。
- `internal/auth`（Claude 部分）：子进程环境按白名单构建，去掉所有密钥变量、`ACO_*` 和 §16.5 移除清单；API 模式只注入 `ANTHROPIC_API_KEY`（来自环境、环境文件或钥匙串）；订阅模式可传 `CLAUDE_CODE_OAUTH_TOKEN`。安全守卫测试的 `Reveal` 与环境变量读取白名单新增 `internal/auth/childenv.go`。
- `internal/prompt`：内嵌并带版本号的提示词模板（规划/实现两个系统提示，以及 plan create、plan revise、phase implement、gate fix、review fix、continue、final fix）；仓库、需求、评审和日志内容都放进 `<data>` 块，块内伪造的结束标签会被中和。
- `internal/agent/fake`：脚本化的规划器与实现器（记录请求、可模拟文件修改、生成合规报告）。
- `aco doctor`：新增 `claude.flags`、`claude.effort`、`claude.auth`（订阅模式读取 `claude auth status --json`）、`claude.probe`（仅 `--probe` 时实际调用一次模型）。
- 配置：effort 允许 `xhigh`（CLI 支持的级别）。
- 测试：录制的 stream-json 夹具（成功、最大轮数、过载、用量上限两种格式、无效密钥、额度不足、429、无结果、钩子缺失、钩子失败）；黄金命令行；伪造 CLI（测试二进制自身）导出子进程环境验证两种模式；取消后 5 秒内结束 Claude 及其子进程（Windows 实测约 4 ms）；guard 规则表；hook 命令端到端；`-tags=integration` 用真实 `aco` 二进制按生成的设置执行钩子；`-tags=live`（`ACO_LIVE_CLAUDE=1`）真实调用。
- 本机已运行：`gofmt`、`go vet`（默认、integration、live 标签，以及 GOOS=linux/darwin 交叉检查）、`staticcheck`（含 integration 标签，无问题）、全部单元与 integration 测试、`CGO_ENABLED=0 go build`、依赖检查、`govulncheck`（代码不受影响）、`gosec`（剩余均为设计如此的项，已在 docs/security.md 说明）。订阅模式 live 测试通过：规划器返回合规计划包；实现器在临时仓库写出代码；`Continue` 沿用同一会话。

## Phase 6 完成内容

详见 [docs/quality-gates.md](docs/quality-gates.md)。

- `internal/gates`：质量门执行器，不调用任何 agent。
  - 模型：六类质量门（typecheck、lint、unit、integration、build、security）按固定顺序执行；状态 `passed`/`failed`/`timeout`/`error`/`not_applicable`/`skipped`；失败类别 `quality`/`policy`/`environment`/`timeout`/`internal`，供 Phase 9 分诊使用。普通轮次遇到第一个失败即停止（后续必需质量门为 `skipped`），基线轮次总是全部执行。
  - 命令解析（`Resolve`）：阶段规格 → 配置 → 自动检测。`not_applicable` 必须带冻结规格中的理由；预检问题 `gate_unresolved`、`not_applicable_unjustified`、`command_not_found`（类别 environment，按命令中每个简单命令的首个程序在 PATH 上查找）、`unknown_scanner`。
  - 自动检测（`Detect`）：Go、Node、Python、Rust 按清单文件识别，Makefile 补齐未解析的质量门；核心工具链总是给出，可选工具（golangci-lint、staticcheck、gosec、govulncheck、ruff 等）只在已安装时给出。
  - Stage 4 全项目模式（`ModeFull`）：配置或自动检测的整项目命令加上所有阶段声明过的命令；所有阶段都标为不适用且没有其他命令时才不适用。
  - 执行：`sh -c`（Windows 使用 Git for Windows 的 sh，没有 cmd.exe 回退）；环境由新增的 `auth.GateEnv` 构建（与 agent 相同的白名单，去掉所有凭据、疑似密钥变量和 `ACO_*`，加 `CI=true` 等）；每条命令有超时（默认 30 分钟），超时后结束整棵进程树（`internal/proc`）；`{reports}` 占位符指向工作树之外的私有报告目录，其中的文件都会被解析。
  - 输出：stdout 与合并日志捕获到临时文件（各最多 64 MiB），脱敏后裁剪到 1 MiB（保留开头、第一处失败附近的内容和结尾，并标出裁掉的字节数）；`go test -json` 的日志换成从事件还原的测试输出；可选把完整的脱敏日志写入运行产物。
  - 每个结果都绑定其运行时的工作状态树哈希；质量门改变了工作状态时记录 `tree_after`。
  - 解析器：`go test -json`（含构建失败的包级失败）、JUnit XML（含 pytest）、ESLint JSON、golangci-lint JSON、SARIF 2.1.0（gosec、semgrep、trivy）、govulncheck JSON、`npm audit --json`（v2 与 npm 6 格式）、`pip-audit -f json`。govulncheck 和 pip-audit 没有严重程度，按 high 处理（失败即封闭）。扫描器以解析出的发现判定，输出无法读取时为 `error`。
  - 安全质量门：阶段模式下对上一阶段提交 → 当前工作状态运行差异策略（受保护路径、范围、文件大小、二进制、反作弊、缺少测试）和内置密钥扫描；全项目模式下扫描工作状态中的所有文件和 `Base..HEAD` 中的每个提交（在运行中加入又删除的密钥仍然阻断；运行开始前就存在于文件中的密钥按基线比较）。SAST 与依赖扫描的新发现达到 `security.fail_severity` 时失败。
  - 基线比较（`Evaluate`）：新失败的测试（基线时通过的另列为回归，按名称列出）、lint/typecheck 中高于 low 的新问题、达到阈值的新漏洞或 SAST 发现、阻断性的策略发现、没有结构化结果的命令出现基线中没有的失败行（去掉行列号、耗时、地址后比较）都会使质量门失败；已有的问题只报告不阻断。问题按工具、规则、路径、消息匹配（不含行号），密钥发现保留行号。
  - 基线存储：`NewBaseline` 与 `BaselineStore`（运行产物中的 `baselines/<sha>.json`）；`Result.Compact` 生成供评审和报告使用的有界结果；`StateRecords` 映射为 `gate_results` 行（`timeout` 存为 `error`，`skipped` 不存）。
- `internal/gitops`：新增 `RevList`（按时间正序列出提交及其第一个父提交），用于历史密钥扫描。
- 夹具：`internal/gates/testdata/fixtures/{go,node,python}`（Go 夹具含一个已知失败的测试和 integration 标签测试）；`testdata/output` 下为各工具的录制输出。
- 测试：三个夹具的六类质量门自动检测与预检；解析优先级、预检问题、全项目模式；每种解析器；已有失败不阻断、回归按名称列出（AC-06.2）；npm audit 新增 high 漏洞失败、已有漏洞只报告、阈值切换（AC-06.3）；挂起命令超时被结束、命令不存在归为 environment（AC-06.4）；2 MB 日志裁剪后保留第一处失败（AC-06.5）；子进程环境不含凭据、日志脱敏；无结构输出的基线比较；fail-fast 与不适用；扫描器报告目录；真实 Git 仓库上的差异策略、密钥扫描和历史扫描。`-tags=integration` 的 `TestGoFixtureEndToEnd` 用真实 Go 工具链在 Go 夹具上跑完整流程：基线轮次、无变化时通过、`TestAdd` 回归被列出、编译错误使 typecheck/build/unit 失败。
- 本机已运行：`gofmt`、`go vet`（默认与 integration 标签，以及 GOOS=linux/darwin 交叉检查）、全部单元测试、`internal/gates` 的 integration 测试、`CGO_ENABLED=0 go build`。staticcheck、gosec、govulncheck 本机未安装，下载被拒绝，本次未运行。

## 已知事项

- Phase 2 时本机无法下载 staticcheck；Phase 3 时已可下载并运行，整个代码库无问题。三平台 CI 仍需在 GitHub 上验证。符号链接相关测试在 Windows 上用 junction，在 Linux/macOS 上用符号链接，后者尚未在本机运行。
- 系统钥匙串只预留了 `auth.KeyStore` 接口，暂无实现。
- `reviewer.model` 默认为空（GPT Sol 6.1 的 Codex 模型 ID 待定，见 §22），`doctor` 会给出警告。
- 本机没有 gcc，`-race` 测试只能在 CI 上运行。
- Phase 5：API 模式的 live 测试本机没有 `ANTHROPIC_API_KEY`，未运行（`ACO_LIVE_AUTH_MODE=api` 可运行）；Linux/macOS 上的进程组终止只做了交叉编译检查，尚未实际运行。
- guard 是策略检查，不是沙箱：允许运行的程序（测试、构建脚本）仍可做其操作系统用户能做的一切；隔离边界是 Phase 12 的容器模式。
- `--max-turns` 在 2.1.311 的 `--help` 中不显示，按 2.x 版本默认支持；不支持时由 ACO 按轮数兜底终止。
- Phase 6：Windows 上质量门命令需要 Git for Windows 的 sh；集成测试依赖的服务地址等只能通过固定的非密钥 `ExtraEnv` 传入，配置中暂无对应字段。Stage 4 的密钥全量扫描应与运行基础提交上的全项目模式基线比较（Phase 11 负责生成）。Node 和 Python 夹具只做了检测测试，未用真实工具链执行。
