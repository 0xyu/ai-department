# ACO 任务进度

实施计划：[AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md](AI_CODING_ORCHESTRATOR_IMPLEMENTATION_PLAN.md)

| 阶段 | 标题 | 状态 |
|---|---|---|
| Phase 1 | 项目基础、配置与 CLI 骨架 | 已完成（三平台 CI 尚未在 GitHub 上运行验证） |
| Phase 2 | 计划包模型、解析器与确定性检查 | 未开始 |
| Phase 3 | 状态存储、事件、产物、状态机与运行锁 | 未开始 |
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

- Go 模块 `github.com/0xyu/aco`（Go ≥ 1.22，唯一外部依赖 `gopkg.in/yaml.v3`，无 CGO）。
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

## 已知事项

- 计划前置元数据层的接口已就绪（`LoadOptions.PlanFrontMatter`），由 Phase 2 的计划解析器接入。
- 系统钥匙串只预留了 `auth.KeyStore` 接口，暂无实现。
- `reviewer.model` 默认为空（GPT Sol 6.1 的 Codex 模型 ID 待定，见 §22），`doctor` 会给出警告。
- 本机没有 gcc，`-race` 测试只能在 CI 上运行。
