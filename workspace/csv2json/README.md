# csv2json

一个把 CSV 文件转换成 JSON 的命令行小工具。首行作为表头,输出为"对象数组"。只使用 Python 3 标准库,无需安装第三方依赖。

## 环境要求

- Python 3.8+(在 Python 3.11.2 下验证)
- 无第三方依赖

## 安装/运行

无需安装,直接在仓库根目录运行脚本:

```bash
python3 csv2json/csv2json.py --help
```

## 用法

```
python3 csv2json/csv2json.py input [-o OUTPUT] [--indent N] [--delimiter C] [--encoding ENC]
```

| 参数 | 说明 | 默认值 |
|---|---|---|
| `input` | 输入 CSV 文件路径;写 `-` 表示从标准输入读取 | 必填 |
| `-o`, `--output` | 输出 JSON 文件路径;省略则输出到 stdout | stdout |
| `--indent N` | JSON 缩进空格数;`0` 表示紧凑单行输出 | `2` |
| `--delimiter C` | 字段分隔符,必须是单个字符 | `,` |
| `--encoding ENC` | 输入文件编码;`utf-8` 会自动处理 BOM | `utf-8` |
| `-h`, `--help` | 显示帮助 | - |

行为说明:

- 所有值均输出为字符串(例如 `"age": "30"`),不做类型推断。
- 支持引号字段与含逗号/引号的字段。
- 空文件输出 `[]`;空行会被跳过。
- 某行字段数少于表头时,缺失字段补空字符串 `""`。
- 非 ASCII 字符原样输出(不转义为 `\uXXXX`)。
- 使用 `-o` 时输出文件以 UTF-8 编码写入。

## 示例

`csv2json/examples/sample.csv` 内容:

```csv
name,age,city,note
Alice,30,Beijing,"likes ""tea"", coffee"
Bob,25,上海,
"Smith, John",41,"New York",plain
```

### 输出到 stdout(默认缩进 2)

```bash
python3 csv2json/csv2json.py csv2json/examples/sample.csv
```

```json
[
  {
    "name": "Alice",
    "age": "30",
    "city": "Beijing",
    "note": "likes \"tea\", coffee"
  },
  {
    "name": "Bob",
    "age": "25",
    "city": "上海",
    "note": ""
  },
  {
    "name": "Smith, John",
    "age": "41",
    "city": "New York",
    "note": "plain"
  }
]
```

### 紧凑输出

```bash
python3 csv2json/csv2json.py csv2json/examples/sample.csv --indent 0
```

```
[{"name": "Alice", "age": "30", "city": "Beijing", "note": "likes \"tea\", coffee"}, {"name": "Bob", "age": "25", "city": "上海", "note": ""}, {"name": "Smith, John", "age": "41", "city": "New York", "note": "plain"}]
```

### 写入文件

```bash
python3 csv2json/csv2json.py csv2json/examples/sample.csv -o out.json
```

### 从标准输入读取

```bash
cat csv2json/examples/sample.csv | python3 csv2json/csv2json.py - --indent 0
```

### 其他分隔符 / 编码

```bash
python3 csv2json/csv2json.py data.csv --delimiter ';' --encoding gbk
```

## 退出码

| 退出码 | 含义 |
|---|---|
| `0` | 成功 |
| `1` | 运行错误(文件不存在/无法读写、编码未知或解码失败、CSV 格式错误),错误信息写入 stderr,形如 `error: ...` |
| `2` | 参数错误:`--delimiter` 不是单个字符,或命令行参数不合法(argparse 报错) |

错误示例:

```bash
$ python3 csv2json/csv2json.py nope.csv
error: [Errno 2] No such file or directory: 'nope.csv'     # 退出码 1
$ python3 csv2json/csv2json.py x --delimiter ab
error: --delimiter must be a single character              # 退出码 2
```

## 运行测试

在仓库根目录执行:

```bash
python -m unittest discover csv2json/tests
```

> 注意:撰写本文档时 `csv2json/tests/` 目录尚不存在(测试由其他任务产出),该命令未能验证通过,请在测试就绪后确认。

## 常见问题

**数字为什么是字符串?** 工具不做类型推断,所有值保持 CSV 中的原始文本。

**中文显示乱码?** 用 `--encoding` 指定正确编码(如 `gbk`)。UTF-8(含 BOM)无需额外参数。

**表头缺失列/行字段过多?** 行字段不足会补 `""`;多出表头的字段会被忽略。

**输出文件被覆盖吗?** 是,`-o` 指定的文件会被直接覆盖写入。
