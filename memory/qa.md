# QA（测试工程师）的长期记忆

- [2026-11-01 20:23] (任务 #4 QA 验证工具与测试) QA验证CLI时要覆盖stdout、-o、--indent、缺失输入、不可写输出路径的退出码。
- [2026-11-01 20:33] (任务 #7 编写并运行单元测试) 任务要求 unittest discover 时,先确认 discover 实际收集到的测试数(Dev 可能写成 pytest 风格导致 Ran 0 tests);必要时改写为 unittest.TestCase。
- [2026-11-01 20:36] (任务 #10 验证 sample/ 已删除且无误伤) 验证删除类任务:检查目录不存在、相邻目录文件齐全(find),再跑各项目 unittest 并记录 Ran 数。
