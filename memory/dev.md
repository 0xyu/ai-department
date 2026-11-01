# Dev（全栈工程师）的长期记忆

- [2026-11-01 20:22] (任务 #1 写一个脚本统计 workspace 里的代码行数（按文件类型汇总，跳过 .git…) Line-count tools: prune SKIP_DIRS in os.walk and skip binaries (NUL byte check); the script itself counts in its own results.
- [2026-11-01 20:23] (任务 #2 开发 csv2json 命令行工具) Avoid rm/delete in shell for temp files (needs approval and may be rejected); use /dev/null or pipes instead. Decode with utf-8-sig to handle BOM.
- [2026-11-01 20:23] (任务 #3 编写自动化测试) Tests for CLIs reading sys.stdin.buffer: use subprocess with input bytes; use tempfile.TemporaryDirectory (auto-cleanup, no rm needed).
- [2026-11-01 20:33] (任务 #6 开发网站在线监控脚本) urllib monitors: catch HTTPError before URLError, plus generic Exception (ValueError for bad URL); test with local ThreadingHTTPServer on port 0.
- [2026-11-01 20:36] (任务 #9 删除 sample/ 旧示例目录) Deleting a specified dir: list_files before and after to verify; rm -rf with an absolute explicit path ran fine in sandbox.
