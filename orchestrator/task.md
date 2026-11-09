# ACO 任务进度

实施计划：[AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md](AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md)

| 阶段 | 标题 | 状态 |
|---|---|---|
| Phase 1 | 项目基础、配置与 CLI 骨架 | 已完成（三平台 CI 尚未在 GitHub 上运行验证） |
| Phase 2 | 计划包模型、解析器与确定性检查 | 已完成（三平台 CI 尚未运行验证） |
| Phase 3 | 状态存储、事件、产物、状态机与运行锁 | 已完成（三平台 CI 与 `-race` 尚未运行验证） |
| Phase 4 | Git 管理 | 未开始 |
| Phase 5 | Claude Code 适配器 | 未开始 |
| Phase 6 | 质量门执行器 | 未开始 |
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

## 已知事项

- Phase 2 时本机无法下载 staticcheck；Phase 3 时已可下载并运行，整个代码库无问题。三平台 CI 仍需在 GitHub 上验证。符号链接相关测试在 Windows 上用 junction，在 Linux/macOS 上用符号链接，后者尚未在本机运行。
- 系统钥匙串只预留了 `auth.KeyStore` 接口，暂无实现。
- `reviewer.model` 默认为空（GPT Sol 6.1 的 Codex 模型 ID 待定，见 §22），`doctor` 会给出警告。
- 本机没有 gcc，`-race` 测试只能在 CI 上运行。
