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
| Phase 7 | 基于 Codex CLI 的 GPT 评审 | 已完成（订阅模式实测通过；API 模式实测、staticcheck/gosec/govulncheck、三平台 CI 与 `-race` 尚未运行） |
| Phase 8 | 第 1、2 阶段流水线 | 已完成（订阅模式实测通过；API 模式实测、staticcheck/gosec/govulncheck、三平台 CI 与 `-race` 尚未运行） |
| Phase 9 | 第 3 阶段单阶段引擎 | 已完成（全部离线场景与真实质量门集成测试通过；staticcheck/gosec/govulncheck、三平台 CI 与 `-race` 尚未运行；真实 Claude/GPT 的阶段循环实测待 Phase 10 的 `aco run`） |
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

## Phase 7 完成内容

详见 [docs/reviewer.md](docs/reviewer.md)。以 codex-cli 0.162.0 实测核对了参数、JSONL 事件、登录状态输出和错误文本；GPT Sol 6.1 在 Codex 中的模型 ID 为 `gpt-6.1-sol`（现为 `reviewer.model` 默认值，§22 第 1 个开放问题已解决）。

- `internal/agent`：新增 `Reviewer` 接口（`ReviewPlan`、`ReviewCode`），评审请求/结果类型（计划评审、阶段评审、复审、最终评审），`PlanReview`/`CodeReview` 结构及校验：未知字段、枚举、每个需求恰好一条覆盖、每个 AC 恰好一次评估、每个旧问题恰好一个状态、八个维度各评一次、问题文件必须是相对路径。结论与细节矛盾时（如 approve 但 AC 未满足）仍接受评审，并在 `Inconsistent` 中列出，供引擎发出 `review.inconsistent`。新增错误类别 `schema_invalid`、`policy_violation`；`agent.Error.Calls` 保留多次调用的用量。
- `schemas/plan-review.schema.json`、`schemas/code-review.schema.json`：严格结构化输出格式（每个对象列出全部属性为必填并禁止额外属性），测试保证与 Go 类型和枚举一致；实测被 Codex `--output-schema` 接受。
- `internal/agent/codexreview`：
  - 命令构建（黄金文件测试）：`codex exec -m <model> -c model_reasoning_effort=… -c web_search="disabled" --disable <功能>… --ignore-user-config --ignore-rules -C <工作树> --sandbox read-only --ephemeral --json --output-schema … -o … -`。只关闭 `codex features list` 中存在的功能（未知名称会让 Codex 直接失败）；Windows 上加 `-c windows.sandbox="unelevated"`（实测：不加时只读沙箱拒绝所有命令）。`CheckArgs` 拒绝一切可写或绕过沙箱的参数和 `-c` 覆盖（有测试）。
  - 上下文包经 stdin 传入，写入产物；提示词模板：评审规则、计划评审、计划复审、代码评审、代码复审、最终评审、格式纠正。超过 `reviewer.max_context_bytes`（默认 512 KiB）时：完整补丁写入产物目录并给出路径，包内保留 diff 统计和按风险排序的代码块；其他大段内容截短并指向完整文件；计划、阶段规格和 AGENTS.md 始终原文保留。
  - JSONL 解析（容忍未知事件和字段）：线程 ID、命令、文件修改、工具调用、警告、错误、token 用量；Codex 提示忽略 `model_reasoning_effort` 时调用失败。
  - 读取监控（§16.3）：检查每条命令（含 `bash -c`、`powershell -Command`、`cmd /c` 内的脚本），工作树和运行产物目录之外、位于用户主目录下或实际存在的路径记为违规；`~`、`$HOME`、`$env:USERPROFILE`、`%USERPROFILE%` 等展开为主目录，其他变量拼出的路径一律违规；跟踪 `cd`，检查指向外部的符号链接；任何文件修改或工具调用（MCP、网页搜索、浏览器等）都是违规。发现违规立即结束进程树，丢弃该评审并重试一次，再次违规返回 `policy_violation`。实测发现：Codex 会自行读取全局技能文件（`~/.agents/skills/…`），没有配置项可关闭；规则中已禁止，仍读取时由监控拦截（录制夹具 `recorded_skill_read.jsonl`）。
  - 重试：格式不合规时带校验错误重试（最多 2 次），之后换 `reviewer.fallback_model`（同一认证模式），仍失败为 `schema_invalid`；transient 和 429 指数退避（参考 CLI 给出的等待时间，最多 4 次）。
  - 错误分类：transient、subscription_limit（解析"try again in 2 hours 13 minutes"和"try again at 3:05 PM / Nov 10th, 2026 3:05 PM"）、api_rate_limit（含等待时间）、api_quota、auth、crash（含未知模型）、timeout、cancelled、budget_exceeded、schema_invalid、policy_violation。
  - API 模式费用：输入 token × `input_per_mtok` + 输出 token × `output_per_mtok`，计入 USD 预算；未配置价格时只记录 token（`Priced=false`）。
  - `Probe`：在临时目录中真实调用一次，验证登录、模型、`model_reasoning_effort`、输出 Schema，并确认沙箱拒绝写文件（必须确实执行过写命令）。
- `internal/auth`（Codex 部分）：Codex 子进程环境（去掉 `OPENAI_API_KEY`、`CODEX_API_KEY`、`OPENAI_BASE_URL`、`CODEX_ACCESS_TOKEN`、所有 `ANTHROPIC_*`/`CLAUDE_*`、疑似密钥和 `ACO_*`；订阅模式保留用户的 `CODEX_HOME`，API 模式设为专用目录）；`codex login status` 解析与模式校验；API 模式在 `~/.aco/codex-api/` 用 `codex login --with-api-key`（密钥只经 stdin）登录，按 Codex 显示的掩码判断是否需要重新登录，拒绝把 `~/.codex` 当作专用目录。每次评审前都校验登录方式。质量门环境同样去掉上述 Codex 变量。
- `internal/apidiff`（新包）：解析统一 diff，并以启发式提取 Go、TypeScript/JavaScript、Python 中新增、删除、修改的公开 API（作为回归数据放入上下文包）。
- `internal/agent/fake`：脚本化评审器 `fake.Reviewer`（按真实适配器的方式校验脚本中的最终消息），以及 `PlanReviewPass`、`CodeReviewApprove` 辅助函数。
- `aco doctor`：新增 `codex.flags`、`codex.auth`（登录方式与解析出的模式不符时失败，并用简体中文说明如何修改）、`codex.probe`（仅 `--probe`）；`config.reviewer_model` 改为信息行。
- 配置：`reviewer.model` 默认 `gpt-6.1-sol` 且为必填；新增 `reviewer.windows_sandbox`（`unelevated`|`elevated`）和 `reviewer.max_context_bytes`（≥ 64 KiB）。
- 测试：伪造 CLI（测试二进制自身）播放 JSONL 夹具：合规评审、先不合规后合规、备用模型、结论矛盾、带重置时间的用量上限（两种格式）、未登录（实录）、读取 `~/.ssh/id_rsa`、实录的技能文件读取、文件修改、transient、429、额度不足、effort 被忽略、未知模型（实录）；命令行与两种上下文包的黄金文件；大 diff 的大小上限；订阅/API 两种子进程环境；API 模式下 `~/.codex` 文件哈希不变、密钥不出现在任何产物中；登录方式不符时不会执行 `exec`；空闲超时；预算用完；监控规则表（Unix 与 Windows 规则）；错误分类与重置时间；Schema 与 Go 类型同步。`-tags=integration`：真实 CLI 的参数和功能检测、用假密钥登录临时专用目录。`-tags=live`（`ACO_LIVE_CODEX=1`）：只读沙箱无法写文件、缺失覆盖的需求被标为 `missing`、植入的 SQL 注入被报告为 `security` blocker（`store/users.go`）。
- 本机已运行：`gofmt`、`go vet`（默认、integration、live 标签，以及 GOOS=linux/darwin 交叉检查）、全部单元测试、`internal/agent/codexreview` 的 integration 测试、订阅模式 live 测试（3 项全部通过）、`CGO_ENABLED=0 go build`、依赖检查（无厂商 SDK）、真实环境下的 `aco doctor`。staticcheck、gosec、govulncheck 本机未安装，下载被权限策略拒绝，本次未运行。

## 代码评审修复（Phase 7）

- 读取监控：普通相对路径参数（不只是绝对路径和 `..`）也按命令中跟踪的工作目录解析；路径中任一部分是符号链接或 Windows 目录联接（junction）时解析其目标（自实现的解析器，用 `os.Readlink`；Go 1.23 起 `filepath.EvalSymlinks` 不再跟随 junction），目标在允许目录之外即违规；同时按名称和文件系统标识（`os.SameFile`，可匹配 8.3 短名）比较；无法解析的链接按违规处理。新增真实文件系统的回归测试：经链接目录/junction 读取、`cd` 进入链接目录、相对文件符号链接（Windows 无权限时跳过，Linux/macOS 上执行）、工作树内正常读取、指向工作树内部的链接；适配器层验证违规评审被丢弃并只重试一次。
- `-o` 原始输出：改写到每次调用私有的临时目录（运行产物目录之外），任何退出路径都会删除；最多读取 4 MiB（超出按格式不合规处理）；只通过脱敏的产物写入器发布。转录也逐行用适配器的脱敏器处理。伪造 CLI 测试证明哨兵密钥在成功、处理失败（非 JSON、超大消息）和发布失败时都不会出现在任何产物中，且临时文件已删除。
- API 模式专用目录：在任何 API 模式的 `login status`、`login`、`exec` 之前检查（放在构建子进程环境处）：不得与 `~/.codex` 或订阅模式配置的 `CODEX_HOME` 相同、包含或被包含——按名称、解析链接/junction（含父目录别名）后、以及文件系统标识比较；专用目录本身是链接、不是目录或无法解析时一律拒绝（`ErrUnsafeAPIHome`，类别 `auth`）。测试覆盖：指向 `~/.codex` 的别名、父目录别名、位于 `~/.codex` 内、配置的 `CODEX_HOME` 及其别名、专用目录本身是链接；均证明未运行 login/exec 且用户凭据文件哈希不变。
- 评审 JSON 先按内嵌 Schema 完整校验（嵌套必填字段、必填处不得为 null、类型、整数、枚举、未知字段；遇到未实现的 Schema 关键字时直接报错，避免静默跳过），再做与请求相关的语义检查。新增 `test_assessment: {}`、必填数组为 null 等测试，并验证这类输出进入现有的格式重试流程；结论矛盾但格式合规的评审仍交给决策门。
- 转录写入错误：JSONL 超过产物大小上限（或写入失败）时立即停止调用并返回明确错误（`crash`），不保存不完整的 `transcript.jsonl`，改写 `transcript.incomplete.txt` 说明原因；错误中保留会话、用量、提示词等调用元数据。新增溢出回归测试。

## Phase 8 完成内容

详见 [docs/planning.md](docs/planning.md)。

- `internal/engine`（新包）：第 1、2 阶段流水线 `RunPlanning`。
  - 第 1 阶段：需求文档、仓库概览（语言、有上限的文件树、已有测试）、已有 AGENTS.md、计划格式指南（内嵌 `planformat.md`）和质量门目录（配置或自动检测的各类命令及可用扫描器）交给 Claude 规划器；规划器只返回结构化计划包，由编排器写文件。
  - 每一版计划包都写入运行工作树的 `.aco/plan/`（替换旧版本），仓库没有 AGENTS.md 时一并写入规划器提议的 AGENTS.md；然后从磁盘读回，用与 `aco validate-plan` 相同的 `plan.ValidateFull` 检查，因此评审的文件就是最终冻结的文件。每一版及其检查报告保存在运行产物 `planning/package-NN/`。写入前拒绝作为链接或 junction 的 `.aco`、`.aco/plan`。
  - 确定性检查不通过的计划包直接退回 Claude，不调用 GPT。
  - 第 2 阶段：Codex 评审（需求、计划包全部文件原文、需求 ID、仓库概览、AGENTS.md、检查结果；复审时附上每个未关闭问题及 Claude 的回应）。
  - 计划决策门 `DecidePlanGate` 是纯函数，带表驱动测试；只看细节，从不采用评审结论（verdict 只记录）。
  - 计划问题台账：引擎分配 `PF-001…`，记录 resolved / dispute_accepted / dispute_rejected / reopened、重开和争议次数，每次变化都写入 `findings` 表并发出对应事件。
  - 修订循环：失败时把未关闭问题和检查结果交给 Claude（第 1 阶段会话中 `Revise`；`--plan` 导入的计划尚无会话时，用一次 `Create` 同时转换并修订，并按修订 Schema 回答每个问题）。`agent.PlanRequest` 新增 `Findings`、`CheckResults`，`plan_create.tmpl` 相应扩展。
  - 轮数上限：每次循环（GPT 评审或检查失败）都计入 `limits.plan_review_rounds`；用完时运行阻塞（最后一版通过检查为 `plan_review_rounds`，否则为 `plan_invalid`），不提交，最新计划包留在工作树，所有版本和评审在产物中，`reports/plan-blocked.md` 列出最后的决策门原因和未关闭问题。
  - 冻结（§10.5）：写 `plan-review.md` 与 `plan.lock.json`；先检查改动路径只在 `.aco/plan/**` 和生成的 AGENTS.md 内（已有 AGENTS.md 绝不改动），再以 `RequireExact`、`ExpectedHead`=基础提交提交 `docs(aco): freeze implementation plan v1`（含四个 trailer），提交后再次校验锁；打 `aco/<run-id>/plan-v1` 标签、写 `git notes`（按配置）；记录计划哈希、`commits` 行、`git.commit`/`plan.frozen` 事件，并登记全部阶段（pending）。仅计划模式进入 completed，完整模式进入 executing。
  - 错误处理：可重试的规划器错误最多重试 2 次；订阅额度用尽时暂停并记录 `resume_at`（重置时间 + 2 分钟，未知时 15 分钟）；auth / API 额度 / 预算失败；评审越权读取阻塞（`policy_violations`）；`limits.agent_calls` 用尽失败（达到软比例时发出 `usage.warning`）；取消即 cancelled。每次 agent 调用都记录到 `invocations` 表并发出 `agent.invocation.*` 事件。
- 运行状态机新增 `plan_review → completed`（仅计划模式冻结后结束），已同步测试和 docs/lifecycle.md。
- `internal/agent/claudecode`：`Create` 带 `Findings` 时使用修订 Schema 并要求回答每个问题。
- `aco plan` 已实现：`--requirements` 或 `--plan`（可同时给出），计划前置元数据作为 plan 配置层；运行锁（心跳丢失即取消）、Git 预检、`~/.aco/worktrees/<run-id>` 工作树与运行分支、`~/.aco/state.db`、`~/.aco/runs/<run-id>` 产物；输出冻结结果或阻塞原因与未关闭问题。退出码：0 已冻结，1 失败/暂停/仓库被占用，2 用法，3 预检失败，4 取消，5 阻塞。新增 16 个 i18n 键。`cli.System` 新增 `Agents`、`LookPath` 注入点（测试用）。
- 测试：决策门表驱动测试与台账测试；伪造规划器/评审器 + 真实 Git 与状态库的场景测试，覆盖 AC-08.1（首轮通过并冻结，提交只含计划包和生成的 AGENTS.md，标签、trailer、锁哈希一致）、AC-08.2（未覆盖需求 + 前向依赖 → 修订 → 第 2 轮冻结）、AC-08.3（持续失败 → 在 plan_review 阻塞，无冻结提交，最新计划包和评审已保存）、AC-08.4（`--plan` Markdown 计划跳过第 1 阶段）、导入计划的转换式修订、检查失败不调用 GPT、`plan_invalid`、各类 agent 错误、重试、评审越权与格式失败、调用上限、取消、被链接的 `.aco`（Windows 上用 junction 实测）。`-tags=integration` 端到端测试（完整模式，经过检查失败、评审失败、争议和复审通过；冻结后的计划包通过 validate-plan 检查且锁已验证；用户检出逐字节不变）。`-tags=live`（`ACO_LIVE_PLAN=1`）真实 Claude + Codex（AC-08.5）。CLI 端到端测试。
- 本机已运行：`gofmt`、`go vet`（默认、integration、live 标签）、全部单元测试、全部 integration 测试、`CGO_ENABLED=0 go build`。订阅模式 live 测试通过（约 4 分钟）：真实 Claude 生成 2 个阶段的计划，GPT 第 1 轮提出 1 个 major 问题（lint 命令在 Windows 的 shell 中无法运行），Claude 修订后第 2 轮通过并冻结；另一次运行第 1 轮即通过。staticcheck、gosec、govulncheck 本机未安装，本次未运行。

## Phase 9 完成内容

详见 [docs/phase-loop.md](docs/phase-loop.md)。

- `internal/engine`：第 3 阶段单阶段引擎 `RunPhase`（运行须为 executing，阶段为 pending，或 `--retry` 后的 initializing）。结果为已提交或已阻塞；订阅额度暂停、auth/额度/预算失败、取消时由结果中的运行状态说明，只有系统问题返回错误（运行以 internal 失败）。
  - 初始化（§11.3）：校验 `plan.lock.json`（计划包与 AGENTS.md 逐文件哈希，且与运行冻结的计划哈希一致）；工作树干净、`HEAD` 等于上一阶段提交（第 1 阶段为冻结提交）、`HEAD` 在运行分支上且分支未被移动；依赖阶段已提交、其提交是 `HEAD` 的祖先、其阶段完成报告存在且通过 Schema。任一失败即阻塞 `init_check_failed`，不调用任何 agent。质量门无法解析时阻塞 `environment`。基线：读取为 `HEAD` 保存的质量门结果，没有时（第 1 阶段）先完整跑一轮并保存；这一轮只有在是干净基础树（`HEAD` 的树）的有效证据时才保存（不是 stale，且树等于基础树），否则不保存、不重置工作树，最多跑 3 次，仍不行则在调用实现者之前阻塞 `init_check_failed`，避免运行中被改动的树上看到的失败日后被当作已有失败。冻结的 `IMPLEMENTATION_PLAN.md`、`phases/PNN.md`、`AGENTS.md` 从工作树按字节原样读取并传给 Claude，另附已提交阶段的摘要、质量门命令、AC ID 和 scope 路径。每次尝试都以 `Start` 开新会话。
  - 质量门：失败即停、与基线比较，每轮写入 `gate_results` 并逐门发出事件；记录阶段中出现过的所有回归。质量门证据绑定到同一棵树：只有每个结果都记录了本轮的树、没有质量门在执行中改动树（`TreeAfter`，执行器在一个质量门的每条命令之后都检查树，命令改动树后又被后续命令恢复也能发现）、且本轮结束时工作树仍是该树，这一轮才有效（`evidenceTree`）；否则（其他写入者，或质量门改动了树，即使后面的质量门又改回）该轮为 stale，只记录（步骤与 `gate.completed` 上标 `stale`），不能授权评审或提交，并在新的候选树上重跑。stale 轮计为一次质量门修复尝试，受 `limits.gate_fix_attempts` 限制。红色轮次交给 Claude（同一会话 `gate_fix`，含摘要、回归、新问题、日志）；环境类失败（命令不存在、服务不可达、ACO 无法运行的质量门）立即阻塞。红色代码不送评审。
  - 评审（§11.6）：上下文含计划概览、阶段列表（done/current/future）、规格与 AGENTS.md 原文、阶段完整 diff 与上次评审后的增量 diff、全部质量门结果、基线比较、测试变更、回归（当前及阶段内出现过的）、安全门与 diff 策略结果、导入了改动 Go 包的文件及改动代码旁的测试、实现报告（未验证）、复审时的台账。
  - 代码问题台账：引擎分配 `F-<阶段>-<序号>`（跨尝试唯一）；处理 resolved / unresolved（"已修复"被否定或已关闭问题再出现即重开）/ partially_resolved / dispute_rejected / dispute_accepted；Claude 回答 `cannot_fix_*` 后评审接受争议即视为确认无法修复。同指纹（类别、文件、标题）的新问题更新旧问题：保留 ID 和历史（打开轮次、重开与争议次数、Claude 的回答），但采用评审当前给出的严重度、建议分类和细节，已关闭或已推迟的问题重新打开；分诊按当前状态判断，之前的推迟或降级不能掩盖当前阻塞的问题（已推迟的 minor 以范围内 major 再次出现时为 fixable）。范围蔓延降级按当前严重度重新判断，只适用于截止轮次之后才首次提出的问题。范围蔓延控制：截止轮次之后、不涉及最新改动行的新 blocker/major（安全、回归除外）降为 minor 并推迟。
  - 评审决策门 `DecideReviewGate`、分诊 `ClassifyFinding`/`Triage`、提交门 `DecideCommitGate`、收敛守卫 `CheckGuards` 均为纯函数，带穷举表驱动测试。评审结论只记录不采用：approve 但决策门失败时发出 `review.inconsistent`。分诊覆盖规则：确认的 cannot_fix_safely → unsafe、cannot_fix_within_spec → spec_conflict；降级、out_of_scope、minor/nit → deferred；文件或 required_change 中的路径命中受保护路径 → unsafe；安全问题复审 2 轮仍未解决 → unsafe；否则采用评审建议或 fixable。评审建议的 deferred 不能推迟范围内的 blocker/major（含安全问题），此时按 fixable 处理；只有 out_of_scope、minor/nit 和被降级的问题会推迟，评审决策门也会再次检查（已推迟的范围内 blocker/major 仍使决策门失败）。unsafe / spec_conflict / environment 立即阻塞，任何严重度的此类未关闭问题都使评审决策门失败。
  - 高回归风险的证据（§11.7）：评审输出新增 `regression_assessment.items`（每项含风险级别、说明、覆盖测试）。评估或任一项为 high 时，至少要有一个高风险项，且每个高风险项至少引用一个测试；每个引用必须按精确身份（`package.TestName`，或能唯一确定一个测试的测试名）对应到本轮被接受的质量门结果中的一个测试，且该测试的所有结果均为 pass。缺失、有歧义、跳过或失败的引用都使决策门保持失败（`regression_risk_high:item-N`）并作为工作项发给 Claude；说明文字不算证据，前缀或子串不算匹配。评审包按质量门列出测试身份供评审引用。没有 `items` 的旧格式评审仍可解析，视为没有证据。
  - 修复：可修复问题按 blocker → major、同级内安全 → 回归 → 其他的顺序交给 Claude（`review_fix`）；不是问题的决策门原因（AC 未满足、偏离计划、高回归风险）作为 `R-…` 工作项一并发送。
  - 收敛守卫：策略违规、反复重开、争议僵局、无进展、评审轮数、连续质量门修复次数、agent 未产出报告、阶段用量（每次尝试的调用数、轮数、超时；每次调用前检查）。守卫触发即结束尝试；在 `limits.phase_restarts` 内把工作树重置到阶段基点、以新会话和"上次尝试的教训"重新开始，否则按守卫类别阻塞。运行级调用数与轮数上限（包含前面阶段的调用）使运行失败。
  - agent 错误（§14.1）：未完成/轮数用尽 → 同一会话 `continue`（最多 2 次，只缺回答的报告可接受）；崩溃/超时 → 重置到步骤检查点后重试；瞬时错误重试；仍失败则结束尝试；评审格式无效或越权 → 结束尝试；订阅额度 → 暂停（重置时间 + 2 分钟）；auth / API 额度 / 预算 → 失败。
  - 提交门（§11.8）：重新计算当前树和改动路径；与评审树不同或质量门结果不是该树的 → 回到评审（先重跑质量门）；计划锁不符 → `plan_integrity`；最终 diff 的策略复查有阻塞项或其他检查失败 → `commit_gate_blocked`。通过后写 `.aco/reports/phase-NN.{md,json}`，按 `ACO-Phase-Attempt` 查重，用 `gitops.Commit` 只提交评审过的路径和两份报告（多余改动或未跟踪文件即拒绝）；提交绑定到生成的报告字节（`ExtraData`：分支移动前核对最终树中两份报告的 blob 与生成并校验过的内容完全一致、且为普通非可执行文件，与 notes 和 `reports` 摘要一致，否则 `ErrExtraMismatch`），父提交固定为本阶段校验过的基础提交（`ExpectedHead`，在 ref 比较交换之外），提交信息含摘要、AC 表、评审轮数和 8 个 trailer；打 `aco/<run>/phase-NN` 标签、写 `git notes`（报告 JSON + 提交 SHA）、记录 `commits`/`reports`，并把本轮质量门结果保存为下一阶段的基线。若提交门通过后、暂存前工作树发生变化，提交在移动分支前被拒绝（`ErrTreeMismatch` / `ErrUnreviewedChange`，报告文件不再是生成的字节时为 `ErrExtraMismatch`）：引擎确认 `HEAD` 未变后把提交步骤记为失败（结果 `retry: worktree_drift`），把两份报告文件恢复到本次提交尝试之前的状态——只处理仍是本次写入内容（字节与权限）的文件，被他人替换或删除的报告与其他工作树改动一样保留，发出带 `worktree_drift` 原因的 `commit_gate.failed`，然后重新跑质量门并做独立评审，再走提交门与策略复查；每次漂移计为一轮未通过的评审（`limits.review_rounds`），每次提交尝试使用独立的步骤键。被替换的报告属于受保护路径（`.aco/**`）的改动，复审后提交门的策略复查会阻塞 `commit_gate_blocked`。分支被移动（包括质量门之后、暂存之前出现的同树外部提交）或分支前进后的失败（标签、notes、状态库）不属于漂移，运行失败，不产生阶段提交。
  - 阻塞：不向运行分支提交；工作状态存入 `refs/aco/<run>/blocked/PNN`；产物 `reports/phase-NN-blocked.md` 写明类别、详情、WIP ref、需要人决定的事项、未关闭问题及 Claude 的回答、最后一轮质量门及日志路径、触发的守卫和恢复选项；阶段与运行进入 blocked。
  - 每次 agent 调用都是一个步骤：调用前后各一个检查点 `refs/aco/<run>/PNN/aK/sM`，意图/结果记录（含运行锁持有者），`invocations` 行关联步骤；质量门轮次和提交也是步骤。
- `internal/reports`（新包）：阶段完成报告模型、`schemas/phase-report.schema.json`（每个对象封闭、全部字段必填）、Markdown 渲染、Schema 校验与严格解析、读取时拒绝链接并核对阶段号、阶段摘要（≤ 6000 字符）。报告包含 §11.9 的全部内容，另列出阶段内出现过的回归和实现报告中与 diff 不符的说法。报告写入前经过脱敏。
- `agent.ValidateSchema`：用内置的 JSON Schema 子集校验任意文档（报告校验使用）。
- 文档：`docs/phase-loop.md`；实施计划 §11 末尾补充"As built in Phase 9"；`docs/lifecycle.md` 补充说明。
- 测试：纯函数穷举表（评审决策门、分诊、范围蔓延、提交门、守卫、台账、修复顺序、增量改动行）；伪造实现者/评审器/质量门 + 真实 Git 与状态库的场景测试，覆盖 AC-09.1 全部 11 个场景（另有环境类失败、范围蔓延推迟、订阅暂停、未完成报告续写）；AC-09.3 的实现提示词黄金测试（规格、AGENTS.md、计划概览原文）；`internal/reports` 的 Schema 校验与往返、Schema 与 Go 类型同步、无效报告、链接拒绝、摘要上限、Markdown 章节（AC-09.2）。`-tags=integration`：真实质量门执行器与 Go 工具链跑两个阶段，真实 `go vet` 失败在同一会话中修复，提交按顺序，两个阶段共用一次基线。
- 修订（Phase 9 复查）：引擎强制推迟规则（评审建议的 deferred 不能清除范围内 blocker/major）；质量门证据绑定到同一棵树，stale 轮重跑；高回归风险改为逐项、按精确测试身份核对的证据（评审 Schema、提示词、解析器同步更新，兼容旧格式）；提交时工作树漂移可恢复（丢弃本次报告文件，重新质量门与评审）。新增测试：推迟规则与证据的表驱动测试；approve + 建议推迟的范围内 major 的阶段测试；质量门改动树后又恢复、持续改动树的阶段测试；提交门通过后暂存前注入改动的阶段测试。
- 修订（Phase 9 第二次复查）：同指纹问题按当前严重度和分类重新分诊；初始基线只接受干净基础树上的有效证据，并在质量门命令边界检测树变化；提交绑定到生成的报告字节和文件权限，回滚只处理本次写入仍拥有的文件；阶段提交的父提交固定为校验过的基础提交。新增测试：已推迟 minor 以同指纹 major 再次出现的台账测试和两轮阶段测试；一个质量门内三条命令改动树、产生测试证据、再恢复树；stale 基线不能掩盖新失败、基线持续漂移即阻塞；仅报告被篡改、报告篡改加源文件漂移；质量门与暂存之间运行分支被空提交推进。
- 本机已运行：`gofmt`、`go vet`（默认、integration、live 标签）、全部单元测试、engine 与 reports 的 integration 测试、`CGO_ENABLED=0 go build`。staticcheck、gosec、govulncheck 本机未安装，本次未运行。

## 已知事项

- Phase 2 时本机无法下载 staticcheck；Phase 3 时已可下载并运行，整个代码库无问题。三平台 CI 仍需在 GitHub 上验证。符号链接相关测试在 Windows 上用 junction，在 Linux/macOS 上用符号链接，后者尚未在本机运行。
- 系统钥匙串只预留了 `auth.KeyStore` 接口，暂无实现。
- ~~`reviewer.model` 默认为空~~：Phase 7 已确定为 `gpt-6.1-sol`。
- Phase 7：API 模式的 Codex live 测试本机没有 `OPENAI_API_KEY`，未运行（`ACO_LIVE_AUTH_MODE=api` 可运行）；API 模式的专用目录登录只用假密钥做过 integration 测试。读取监控是基于命令文本的启发式检查，无法解析 `$(…)` 命令替换等情况，严格隔离依赖 Phase 12 的容器模式。Codex 在 `--ephemeral` 下仍会向 `CODEX_HOME` 写入状态文件（订阅模式即用户的 `~/.codex`）。
- 本机没有 gcc，`-race` 测试只能在 CI 上运行。
- Phase 5：API 模式的 live 测试本机没有 `ANTHROPIC_API_KEY`，未运行（`ACO_LIVE_AUTH_MODE=api` 可运行）；Linux/macOS 上的进程组终止只做了交叉编译检查，尚未实际运行。
- guard 是策略检查，不是沙箱：允许运行的程序（测试、构建脚本）仍可做其操作系统用户能做的一切；隔离边界是 Phase 12 的容器模式。
- `--max-turns` 在 2.1.311 的 `--help` 中不显示，按 2.x 版本默认支持；不支持时由 ACO 按轮数兜底终止。
- Phase 8：订阅额度暂停后的自动恢复、`aco resume`（含 `--revise-plan` 产生 plan-v2）由 Phase 10 实现；暂停时 `aco plan` 以退出码 1 结束。规划阶段的 agent 调用上限按单次运行计数（不跨进程恢复）。`--plan` 导入且仓库没有 AGENTS.md 时，只有在规划器参与修订并提议时才会生成 AGENTS.md。
- Phase 6：Windows 上质量门命令需要 Git for Windows 的 sh；集成测试依赖的服务地址等只能通过固定的非密钥 `ExtraEnv` 传入，配置中暂无对应字段。Stage 4 的密钥全量扫描应与运行基础提交上的全项目模式基线比较（Phase 11 负责生成）。Node 和 Python 夹具只做了检测测试，未用真实工具链执行。
- Phase 9：阶段循环尚未用真实 Claude/GPT 实测（需要 Phase 10 的 `aco run` 驱动）；场景测试使用伪造 agent，集成测试使用真实质量门。阶段中途崩溃后的对账续跑（按检查点重做或接受步骤）、暂停后的继续由 Phase 10 实现，`RunPhase` 目前只从 pending 或 `--retry` 后的 initializing 开始。§11.7 中"会话过大时以精简简报开新会话"尚未实现，修复始终在本次尝试的会话中继续。阶段用量上限按每次尝试计算。评审包中"导入了改动包的文件"只对 Go 模块做文本匹配，其他语言只列出改动代码旁的测试。高回归风险的覆盖按 `regression_assessment.items` 中逐项引用的测试判断（精确身份匹配本轮被接受的质量门结果），不再从评审说明文字中查找测试名。
