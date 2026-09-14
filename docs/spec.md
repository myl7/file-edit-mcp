# 文件编辑 MCP API 设计稿

面向一个直连文件系统的通用 MCP server，服务对象是远端的一个目录，内容形态不限。
工具形状落在已有实测支持的 search/replace 一族，不复刻任何厂商的 tool 名和描述原文。

## 0. 前提与边界

- 启动参数给一个或多个允许目录，server 只在这些目录内工作。
- 不提供 shell、进程、网络、剪贴板、截屏工具。需要执行命令时用别的 server，权限分开。

### 路径约定

`file_path` 是宿主机的真实绝对路径，和 Claude Code、官方 filesystem server 的做法一致。
Server 启动时给定一个或多个允许目录，每次调用先校验再操作。

校验按下面的顺序做，参考官方 filesystem server 的 `validatePath`。

1. 拒绝含空字节的路径。
2. 展开 `~`。
3. 不是绝对路径就拒绝，或按允许目录解析，二选一，但要固定一种并写进工具描述。
4. `path.normalize` 加 `path.resolve`，消掉 `.`、`..` 和重复分隔符。这一步必须在
   前缀比较之前完成，否则 `<root>/../etc/passwd` 会通过。
5. 与每个允许目录做前缀比较，比较的两边都用规范化后的形式。相等算通过，
   子路径算通过。注意根目录和 Windows 盘符根的双斜杠边界情况。
6. 调 `realpath` 解析符号链接，把结果再做一次前缀比较。指向允许目录外的链接拒绝。
   后续操作用 realpath 的结果，不要用原始路径。
7. 路径不存在时 `realpath` 返回 `ENOENT`，这时改为校验父目录，父目录不存在就报
   「父目录不存在」。新建文件走的是这条分支。

有一个坑官方实现专门处理了：在 POSIX 主机上收到 `C:\Users\...` 这类 Windows 路径时
直接拒绝，而不是当成相对路径。否则会在允许目录里创建一个名字叫 `C:\Users\...`
的文件并报告成功，位置完全不对。同理，任何「看起来不对但能解析成根内路径」的输入
都应该报错，不要做智能纠正。

两点补充。

先校验后打开存在 TOCTOU 窗口，校验通过之后、真正打开之前，路径上的某一段可能被换成
符号链接。要关掉这个窗口，Linux 上用 `openat2` 配 `RESOLVE_BENEATH` 和
`RESOLVE_NO_SYMLINKS`，让内核在解析时就保证不越界。做不到的话，至少在打开之后
用 `fstat` 或 `/proc/self/fd` 再确认一次拿到的是不是校验时的那个文件。

CIFS 上符号链接是否可用取决于服务端和挂载参数，当前挂载没有加 `mfsymlinks`。
不要因为「大概建不了软链」就跳过第 6 步，允许目录之外的路径段仍然可能有链接。

## 1. 工具清单

### Read

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `file_path` | string | 是 | 绝对路径 |
| `offset` | int | 否 | 起始行，1-based |
| `limit` | int | 否 | 读取行数 |

返回每行加行号前缀，格式 `<行号>\t<内容>`。行号是给模型定位用的参考，不作为寻址依据。
不带 `offset` 时从第一行开始。文件不存在返回错误，不要返回空字符串。

不带 `limit` 时的默认上限取三个，任一触发即截断。

| 限制 | 取值 |
|---|---|
| 默认行数 | 2000 |
| 单行字符数 | 2000 |
| 单次返回总字节数 | 256 KiB |

行数和单行长度这两个值抄 gemini-cli，它的 `DEFAULT_MAX_LINES_TEXT_FILE` 和
`MAX_LINE_LENGTH_TEXT_FILE` 都是 2000，另有 20 MB 的可读文件大小上限，
超长行截断后接 `... [truncated]`。总字节数上限是自己加的，因为 2000 行乘 2000 字符
最坏情况有 4 MB，光靠行数挡不住。

Codex 没有 read 工具，它用 shell 读文件，限制加在工具输出上：默认策略是
`TruncationPolicy::Bytes(10_000)`，也就是一万字节，按 model info 下发，可切成 token 模式。
它的截断方式值得抄两点。一是截掉中间保留首尾，用 `truncate_middle_chars`。
二是截断后在开头加一段说明，写明原始 token 数和总行数。

对 Read 来说保留首尾没有意义，模型是顺序读的，截掉尾部即可。但告知总行数必须做，
返回里要写清本次给了哪个行号区间、文件共多少行、以及用 `offset` 和 `limit` 继续读。
模型不知道还剩多少内容时会默认自己读完了。

Claude Code 的 Read 常被引用的数字也是 2000 行加 2000 字符每行，但这个我没有从源码核实，
按 gemini-cli 的实测值定就行。

### Write

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `file_path` | string | 是 | 绝对路径 |
| `content` | string | 是 | 完整内容 |

只用于新建文件和整体重写。目标已存在且本会话没有 Read 过时拒绝，报错要求先读。
这条约束能挡掉模型凭想象覆盖已有内容的情况，值得保留。

写入用临时文件加 rename，不要就地截断写。

### Edit

| 参数 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `file_path` | string | 是 | | 绝对路径 |
| `old_string` | string | 是 | | 要被替换的原文 |
| `new_string` | string | 是 | | 替换后的内容 |
| `replace_all` | bool | 否 | false | 是否替换所有匹配 |

语义规定如下，这几条是这套 API 的核心，不要放松。

1. 精确匹配。逐字节比较，包含空白和缩进。不做 trim 后比较，不做缩进重排，不做
   任何模糊回退。官方 filesystem server 的 `edit_file` 有按行 trim 的回退，用在
   markdown 嵌套列表这类缩进本身有语义的内容上会改掉原有缩进。
2. `replace_all` 为 false 时要求唯一匹配。命中 0 次报错，命中多次也报错并告知次数，
   不要默默改第一处。
3. `old_string` 等于 `new_string` 时报错。
4. 单个 Edit 只做一处替换。多处编辑走 MultiEdit。
5. 全部编辑在内存中应用完再落盘，中途失败不留半成品。

### MultiEdit

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `file_path` | string | 是 | 绝对路径 |
| `edits` | array | 是 | 每项含 `old_string`、`new_string`、可选 `replace_all` |

按数组顺序在逐步修改的内容上应用，每一项的唯一性要求在应用它时的当前内容上判断。
任一项失败则整体失败，不写盘，报错里指明是第几项以及原因。

### Glob

| 参数 | 类型 | 必填 | 说明 |
|---|---|---|---|
| `pattern` | string | 是 | glob 模式，支持 `**` |
| `path` | string | 否 | 搜索起点，默认允许目录 |

返回路径列表，按修改时间倒序。最近改过的文件通常就是模型要找的那批。
结果条数超过上限时截断，并告知总数。

### Grep

| 参数 | 类型 | 必填 | 默认 | 说明 |
|---|---|---|---|---|
| `pattern` | string | 是 | | 正则 |
| `path` | string | 否 | 允许目录 | 搜索范围 |
| `glob` | string | 否 | | 文件名过滤 |
| `output_mode` | enum | 否 | `files_with_matches` | `content` / `files_with_matches` / `count` |
| `case_insensitive` | bool | 否 | false | |
| `context` | int | 否 | 0 | 上下文行数，仅 `content` 模式有效 |
| `head_limit` | int | 否 | | 结果条数上限 |

底层用 ripgrep。默认只返回文件名，模型要内容时自己升级到 `content` 模式，这样省 token。

### move / rename

第一版不提供。如果后面要加，语义就是纯 `mv`，server 不做任何引用或链接的维护，
并在工具描述里写明这一点。

## 2. 错误信息

排行榜数据里单独测「使用正确编辑格式的比例」，并在出错时把反馈发回模型要求重新生成。
这说明错误信息和重试回路对实际成功率的影响不小，可能大于 schema 的字面对齐。

每条错误要满足三点：说清失败原因、给出可操作的下一步、不要重复整段原文。

- 找不到匹配：告知未找到，提示先 Read 确认原文包括空白，不要回显整个文件。
- 多处匹配：给出次数，提示扩大 `old_string` 的上下文范围或改用 `replace_all`，
  如果能给出前几处的行号更好。
- 未读就写：告知需要先 Read 该文件。
- 路径越界：告知路径不在允许目录内，并列出允许目录。
- 文件不存在：区分「文件不存在」和「文件存在但匹配失败」，两者的下一步不同。
- 后端不可用：区分「文件不存在」和「共享挂载不可达」，见第 4 节。
- 输出被截断：说明本次返回的行号区间和文件总行数，并提示用 `offset` 继续读。

## 3. 明确不做的事

- 行号寻址。Aider 试过基于行号的格式，效果差于简化 unified diff。
- 自定义控制标记。Diff-XYZ 里 udiff-l 用 ADD/DEL/CON 替换 `+`/`-`，本意是减少歧义，
  结果 GPT-4o 的 diff 生成精确匹配率掉到 0.01。偏离预训练分布的代价很大。
- V4A / apply_patch 形状。MCP 工具只有 JSON Schema，没有 freeform grammar tool，
  包进来只能退化成一个装整段 patch 的 string 参数，保真度反而掉。同一批测试里
  GPT-4.1 用 search-replace 的 diff 生成精确匹配率是 0.95，用 udiff 是 0.81，
  通用 search/replace 对 GPT 系并不吃亏。
- 整文件 put 作为唯一编辑手段。token 和延迟都不划算。
- 模糊匹配回退。宁可报错让模型重试，不要猜。

## 4. 后端是 CIFS 挂载时的约束

目标目录位于 automount 挂载的 CIFS 共享（示例路径 `/data`），
挂载参数为 `vers=3.11,uid=1000,gid=1000,noperm,file_mode=0777,dir_mode=0777,
iocharset=utf8,actimeo=30,rsize=1048576,wsize=1048576,soft,nofail,_netdev,
x-systemd.automount,x-systemd.idle-timeout=600,echo_interval=60`。

### soft 挂载的错误处理

`soft` 表示服务端无响应时系统调用返回错误而不是无限挂起。对 MCP server 是好事，
请求不会卡死，但 `stat()` 和 `read()` 会出现瞬时失败。要把 `ENOENT` 和
`EIO`/`ETIMEDOUT`/`EHOSTDOWN` 分开报，前者是文件真不存在，后者是后端抖动，
应该提示模型重试而不是让它以为文件没了。

### 首次访问的冷启动

`x-systemd.automount` 加 `idle-timeout=600` 意味着空闲 10 分钟后卸载，下一次访问
触发重新挂载。所以一段时间没用之后的第一个工具调用会明显慢，甚至在后端未就绪时失败。
Server 启动时不要假设挂载已存在，第一次访问失败后退避重试一次再报错。

### 其他

挂载用了 `noperm` 和 `file_mode/dir_mode=0777`，权限检查在客户端被绕过，
所有文件对 uid 1000 都可读写。也就是说 server 自己不做路径白名单的话，
文件系统层面不会帮你挡任何东西。inode 号由服务端提供，跨重命名的身份追踪不可靠，
不要用 inode 做文件标识。


## 5. 自测方案

分两层，先验脚本再验产品。

### 第一层：用 Diff-XYZ 跑通评测脚本

取 Diff-XYZ 的 30 条，只为确认打分逻辑没写错，不用来得结论。
数据集在 HuggingFace 上（`JetBrains-Research/diff-xyz`），单个 test split 1000 条，
parquet 下载 2.7 MB，解包 5.9 MB，kotlin、java、rust、python、javascript 各 200 条。
字段里 `old_code` 平均 1112 字符，`new_code` 平均 1313，`search-replace` 平均 918，
`udiff` 平均 745，最长三千出头。跑满 1000 条一个格式约 0.7M 输入加 0.3M 输出 token。

不要拿它当产品测试。它是单轮文本进文本出，没有 tool call、没有文件系统、没有重试，
本文档里真正能调的东西，错误信息写法、唯一匹配的报错、截断提示、失败后的收敛，
一个都不会被触发。内容也全是代码，而 search/replace 在散文里的锚点特性和代码不同，
重复列表项、frontmatter、空行都更容易撞多处匹配，恰好是本 API 最吃紧的规则。
格式对比论文已经做过，在同一份数据上重跑只会复现它的结论。

### 第二层：用自己的真实数据测产品

从实际要操作的文件里造 50 到 100 条真实编辑任务，配对设计，同一条任务喂给每一种接口。
100 条足够：成功率在 0.9 附近时二项标准误是 3 个百分点，配对还能压掉共同难度带来的方差，
而要区分的差距是十几个百分点。

三个指标：

1. 一次调用就成功的比例
2. 第一次失败后能否在两轮内收敛
3. 每次成功编辑消耗的 token

这三个是调错误信息文案和截断策略时会动的数字。

任务要覆盖几类容易出问题的情况：`old_string` 在文件里重复出现、目标行在 2000 行之后
需要先带 `offset` 读、超长单行、文件不存在、父目录不存在、路径越界。

对比组可以设成本文档这套 Edit 形状、apply_patch 形状、带 trim 回退的 edit_file 形状，
但如果时间有限，只测本套 API 在不同错误信息写法下的差别，价值更高。

## 6. 依据

- Diff-XYZ，arXiv 2510.12487，JetBrains Research。数据集规模和字段长度由
  `JetBrains-Research/diff-xyz` 的 parquet 实测得到。Table 5 给出四种表示在 apply、
  anti-apply、diff generation 三个任务上的结果。search-replace 对较强模型全面最好：
  GPT-4.1 在 diff generation 上 0.95 对 udiff 的 0.81，Claude 4 Sonnet 0.94 对 0.82,
  GPT-4o 0.73 对 0.41，Qwen2.5-Coder-32B 0.68 对 0.23。udiff-l 全线崩溃。
  论文把原因归给分布偏移和局部约束优于全局约束。
- Aider 的 unified diff 实验，把 GPT-4 Turbo 的分数提到 59%，作者说这套格式远好于
  他试过的提示词、function calling、行号格式等其他方案。
- Aider 的编辑排行榜同时报告完成率和格式合规率，并在出错时反馈重试。
- 路径校验流程取自官方 filesystem server 的 `src/filesystem/lib.ts` 里的 `validatePath`
  和 `src/filesystem/path-validation.ts` 里的 `isPathWithinAllowedDirectories`。
- 官方 filesystem server 的 `applyFileEdits` 实现：先试精确子串匹配，失败后按行
  trim 比较并重排缩进，用 `String.replace` 只替换第一处，无出现次数校验。
- 挂载参数为部署侧 CIFS 挂载所用配置。
- 读取上限取自 `google-gemini/gemini-cli` 的 `packages/core/src/utils/constants.ts`：
  `DEFAULT_MAX_LINES_TEXT_FILE = 2000`、`MAX_LINE_LENGTH_TEXT_FILE = 2000`、
  `MAX_FILE_SIZE_MB = 20`。
- Codex 的输出截断取自 `openai/codex` 的 `codex-rs`：
  `protocol/src/openai_models.rs` 和 `models-manager/src/model_info.rs` 里默认
  `TruncationPolicyConfig::bytes(10_000)`，截断实现在
  `utils/output-truncation/src/lib.rs` 的 `formatted_truncate_text`，
  中间截断加原始 token 数与总行数的提示头。
- Codex 的路径解析取自 `apply-patch/src/parser.rs` 的 `Hunk::resolve_path`
  与 `utils/path-uri/src/lib.rs` 的 `join` 和 `join_descendant`。
