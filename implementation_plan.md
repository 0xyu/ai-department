# 个人 AI IT 部门 — 实施计划

## 目标
构建一个真正执行任务的「AI IT 部门」：6 名员工各自是独立的 Claude agent，命令在 Docker 沙箱中运行，所有状态写入 SQLite 并通过 WebSocket 推送到 3D 办公室。3D 画面中的每个动作都来自后端真实事件，没有纯装饰动画。

## 技术选型
| 层 | 选型 | 说明 |
|---|---|---|
| 后端 | Go 1.27 | 用户指定使用 Go |
| LLM | `github.com/anthropics/anthropic-sdk-go`，默认模型 `claude-sonnet-5-5`（可用 AIDEPT_MODEL 修改） | Claude Agent SDK 没有 Go 版本，因此在 Go 中手写 agent 循环（消息 → 工具调用 → 工具结果 → 循环） |
| 存储 | SQLite（`modernc.org/sqlite`，纯 Go，无需 CGO） | 表：requests / tasks / events / memories / approvals |
| 推送 | `gorilla/websocket` | `/ws` 推送所有事件 |
| 沙箱 | Docker 容器 `aidept-sandbox`（读写挂载 `./workspace`）+ `aidept-sandbox-ro`（只读挂载，供 Sec 使用） | 所有 shell 命令通过 `docker exec` 执行 |
| 前端 | Vite + React + React Three Fiber + drei | low-poly 办公室，几何体拼成的人物 |

## 目录结构
```
agents/<id>.md        每个员工的定义（角色、职责边界、工具白名单、交付标准、上报规则）
memory/<id>.md        每个员工的长期记忆（同时存入 SQLite memories 表）
workspace/            agent 的工作目录（挂载进沙箱 /workspace）
sandbox/Dockerfile    沙箱镜像（python3、node、git、curl、ssh 客户端）
backend/              Go 后端
  cmd/server          HTTP + WebSocket 服务，同时托管前端构建产物
  cmd/cli             命令行运行一个需求（步骤 1、2 的验证入口）
  internal/store      SQLite 访问
  internal/events     事件总线（写库 + 广播）
  internal/agents     定义加载、agent 循环、工具实现
  internal/sandbox    Docker 沙箱管理
  internal/orchestrator  需求 → 任务 → 调度 → 汇总
  internal/approval   高风险操作审批
web/                  前端
```

## 核心设计

### 1. Agent 循环
- 每次执行任务时，为该员工启动独立的对话：system prompt = 定义文件 + 记忆；工具 = 定义文件中的白名单。
- 循环调用 Messages API，执行返回的工具调用，直到员工调用 `complete_task` / `block_task` / `reject_task` 或达到轮次上限。
- 每次工具调用前写入 `tool_call` 事件（含人类可读描述，作为 3D 气泡文字），执行后写入 `tool_result` 事件。

### 2. 工具
| 工具 | 说明 | 谁能用 |
|---|---|---|
| list_files / read_file / search | 读取 workspace | 全员 |
| write_file / edit_file | 写入 workspace（Doc 仅限 .md） | Dev, Ops, QA, Doc |
| delete_file | 删除文件（**需审批**） | Dev, Ops |
| shell | 在沙箱中执行命令；高风险命令**需审批**；Sec 在只读容器中执行 | Dev, Ops, Sec, QA |
| ssh | 通过沙箱 ssh 到远程主机（**始终审批**） | Ops |
| http_request | 对外发送 HTTP 请求；非 GET **需审批** | Ops |
| create_task | 创建子任务并分派 | Max |
| complete_task / block_task | 完成 / 上报阻塞 | 执行者 |
| reject_task | 把任务打回给 Dev | QA |
| remember | 把经验追加到自己的记忆 | 全员 |

高风险判定：删除（rm、rmdir、git clean、reset --hard）、部署/远程（ssh、scp、rsync、docker、kubectl、deploy）、对外发送（git push、curl/wget 带 POST/PUT/DELETE/数据、npm publish）、花钱（任何包含 purchase/billing 等的操作）。

### 3. 工作流
1. 用户提交需求 → Max 用 `create_task` 拆解（任务立即出现在白板上，状态「待办」）。
2. 若涉及 ≥2 名员工，Max 召开启动会（`meeting_start` / `meeting_end` 事件，参与者走进会议室）。
3. 调度器按依赖关系并行派发（每人同一时间只做一个任务）：
   - 无依赖的任务：Max 把任务卡交给执行者（`handoff` Max → 执行者）。
   - 有依赖的任务：上游执行者把成果交给下游（`handoff` 上游 → 下游）。
   - handoff 后接收者才开始 working。
4. 被 QA/Sec 依赖的任务完成后进入「审核」；审核通过才变「完成」。
5. QA 调用 `reject_task` → QA 与 Dev 开缺陷评审会 → Dev 返工 → QA 复测（最多 2 轮）。
6. 全部结束后 Max 汇总交付说明，需求完成。
7. 每个员工完成任务前调用 `remember` 写入记忆；若未调用，系统会追加一轮要求其总结。

### 4. 事件与状态
- 所有事件写入 `events` 表并通过 WebSocket 广播。
- 服务端维护每个员工的展示状态：`idle` / `working`（附当前活动描述）/ `handoff`（目标员工）/ `meeting`（参与者）/ `blocked`（等待审批或阻塞）。
- 前端连接时先拉取 `/api/state` 快照，再增量应用事件。

### 5. 3D 办公室
- 开放式办公区：6 个工位、会议室、白板、经理办公室、茶水间。
- 人物：胶囊身体 + 球形头 + 角色颜色和配饰（领带、眼镜、头盔、耳机等）。
- 动作映射：idle → 工位闲坐，空闲超过一段时间去茶水间；working → 坐下打字 + 气泡；handoff → 走到接收者工位再返回；meeting → 走进会议室；blocked → 头顶红色感叹号。
- 点击人物 → 侧边栏（角色、当前任务、最近日志、记忆）；点击白板 → 完整看板；审批弹窗。

## 构建与验证顺序
1. 后端 + Dev 单 agent：CLI 运行「写一个脚本统计 workspace 里的代码行数」并执行。
2. Max 分派 + 其余角色：CLI 跑通多人协作任务。
3. 事件日志 + WebSocket。
4. 3D 场景和人物接入事件。
5. 审批弹窗、记忆、任务看板。
6. 端到端验收：「帮我做一个监控网站是否在线的脚本，写好测试和文档」。

## 运行方式
```bash
# 需要环境变量 ANTHROPIC_API_KEY，且 Docker Desktop 已启动
cd backend && go run ./cmd/server        # http://localhost:8787
cd web && npm run dev                    # 开发模式前端 http://localhost:5173
cd backend && go run ./cmd/cli "需求"    # 命令行模式
```
