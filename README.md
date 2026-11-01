# Personal AI IT Department

Six Claude agents work as an IT department: they plan, write code, run it in a Docker sandbox, test it, document it, and report back. A 3D office shows what each of them is doing. Every movement in the office comes from a real backend event; nothing is animated for decoration.

| Agent | Role | Tools (whitelist in `agents/<id>.md`) |
|---|---|---|
| Max | IT manager: splits the request into tasks, assigns them, writes the delivery report. Writes no code. | read workspace, `create_task`, `finish_planning`, `deliver` |
| Dev | Full-stack engineer | files, `shell` (sandbox, git), `delete_file` |
| Ops | DevOps | files, `shell`, `ssh` (always needs approval), `http_request` |
| Sec | Security engineer | read-only files, `shell` in a **read-only** sandbox |
| QA | Test engineer: can send work back to Dev | files (tests), `shell`, `reject_task` |
| Doc | Technical writer: `.md` files only | files (`.md` only), `shell` to check example commands |

## Requirements

- Go 1.22+ (built with 1.27)
- Node 18+ (front-end build)
- Docker Desktop running
- An Anthropic API key in `.env` at the project root:
  ```
  ANTHROPIC_API_KEY=sk-ant-...
  # optional
  AIDEPT_MODEL=claude-sonnet-5-5
  AIDEPT_EFFORT=medium
  ```

## Run

```bash
cd web && npm install && npm run build      # once, or after front-end changes
cd backend && go build -o ../bin/aidept-server.exe ./cmd/server
./bin/aidept-server.exe                      # open http://127.0.0.1:8787
```

The first start builds the sandbox image (`sandbox/Dockerfile`) and starts two containers: `aidept-sandbox` (workspace mounted read-write) and `aidept-sandbox-ro` (read-only, used by Sec).

Front-end development with hot reload: `cd web && npm run dev` (http://localhost:5173 proxies `/api` and `/ws` to the Go server).

Command line (no UI):

```bash
cd backend
go run ./cmd/cli "帮我做一个监控网站是否在线的脚本，写好测试和文档"
go run ./cmd/cli -agent dev "写一个脚本统计 workspace 里的代码行数"   # one agent, no planning
```

Flags: `-fast` skips handoff and meeting pacing; `-auto-approve` approves every risky action (testing only).

## How it works

1. **Plan.** Max reads the workspace and calls `create_task` for each sub-task, with `depends_on` for ordering.
2. **Kickoff.** If two or more people are involved, everyone meets in the meeting room.
3. **Schedule.** Tasks run in parallel when their dependencies are done, with one task per person at a time. Before a task starts, the colleague who did the upstream work, or Max, walks over to hand it off. If the giver is busy, a task card flies across instead.
4. **Review.** A task that QA or Sec depends on goes to `review` and only becomes `done` when the reviewer passes it. When QA calls `reject_task`, QA and Dev hold a defect-review meeting, Dev fixes the work, and QA tests again. After 2 failed rounds, the task is blocked.
5. **Deliver.** Max checks that the files exist and writes the delivery report.
6. **Memory.** `complete_task` / `deliver` are refused until the agent has called `remember`. Each lesson goes to SQLite and to `memory/<id>.md`, and is added to that agent's system prompt next time.

**Approvals.** Some actions pause until you click 批准 or 拒绝: shell commands that delete, deploy, contact remote hosts, push or send data, or spend money; `delete_file`; `ssh`; and any `http_request` other than GET or HEAD. While waiting, the agent shows a red "!" and its task is `blocked`. The rules are in `backend/internal/agents/risk.go`.

**Events.** Every state change, handoff, meeting, tool call and result, memory and approval is written to the `events` table and pushed over `/ws`.

## API

| Method | Path | |
|---|---|---|
| GET | `/api/state` | Agents with presence, recent requests, tasks, pending approvals, recent events |
| POST | `/api/requests` | `{"text": "..."}` |
| GET | `/api/agents/{id}` | Definition, presence, current task, recent log, memories |
| POST | `/api/approvals/{id}` | `{"approved": true}` |
| GET | `/ws` | Live events (one JSON object per message) |

## Layout

```
agents/       employee definitions (role, boundaries, tool whitelist, delivery standard, escalation)
memory/       long-term memory per agent
workspace/    the agents' working directory (mounted at /workspace in the sandbox)
sandbox/      sandbox Dockerfile
backend/      Go: store, events, sandbox, approval, agents (loop + tools), orchestrator, cmd/server, cmd/cli
web/          Vite + React Three Fiber front-end
data/         SQLite database (aidept.db)
```

## Known limitations

- The risk check matches patterns in the shell command text. It does not see what a script does once it runs. For example, `python3 x.py` that deletes files is not caught, although it can only touch `/workspace`.
- The sandbox has network access, because the agents need it (for example, to check that a website is up).
- Requests are processed one at a time.
- The handoff (4 s) and meeting (8 s) pauses are real waits in the backend, so the walk can be seen before the next person starts.
