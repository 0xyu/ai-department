# 任务清单

## 步骤 1：后端 + 单个 agent（Dev）
- [x] Go 模块初始化、依赖安装
- [x] SQLite 存储层（requests / tasks / events / memories / approvals）
- [x] 沙箱镜像与容器管理（读写 + 只读），集成测试通过
- [x] agent 定义文件加载（agents/*.md）
- [x] agent 循环 + 文件/shell 工具
- [x] CLI：Dev 完成「统计 workspace 代码行数」并真实执行（tools/count_lines.py，结果已独立复核）

## 步骤 2：Max 分派 + 多角色协作
- [x] 6 个 agent 定义文件
- [x] Max 的 create_task 拆解
- [x] 依赖调度、并行派发、handoff
- [x] 审核状态、QA 打回与返工（逻辑已实现；测试中 QA 尚未真实打回过）
- [x] Max 汇总交付
- [x] CLI 跑通多人协作任务（csv2json：Dev → QA → Doc，15 个测试通过）

## 步骤 3：事件日志 + WebSocket
- [x] events 表 + 事件总线
- [x] HTTP API（/api/state、/api/requests、/api/agents/:id、/api/approvals/:id）
- [x] WebSocket 推送（前端实时更新已验证）

## 步骤 4：3D 场景
- [x] 办公室布局（6 工位、会议室、白板、经理办公室、茶水间）
- [x] 几何体人物与配饰（领带、耳机、安全帽、墨镜、圆眼镜、贝雷帽）
- [x] 事件驱动动作（idle 坐着/去茶水间、working 打字 + 气泡、handoff 走到接收者工位、meeting 进会议室、blocked 红色感叹号）

## 步骤 5：审批、记忆、看板
- [x] 高风险操作审批卡片（批准、拒绝都已在 UI 中验证）
- [x] remember 工具 + memory 文件（完成任务前必须先记录）
- [x] 人物侧边栏（角色、当前任务、最近日志、记忆）、白板看板

## 验收
- [x] 「帮我做一个监控网站是否在线的脚本，写好测试和文档」：产出 workspace/site-monitor/（site_monitor.py、20 个测试全部通过、README.md），3D 画面中可见启动会、交接、工作气泡全过程

## 后续可改进
- [ ] 风险判定只看命令文本，看不到脚本运行后的行为
- [ ] 让 QA 真实打回一次，验证返工流程的 3D 表现
- [ ] 支持多个需求并发处理
