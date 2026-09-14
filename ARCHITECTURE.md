# file-edit-mcp 架构规格（约束合同）

本文是唯一权威规格。所有实现必须与本文一致；实现与本文冲突时以本文为准。
原始需求文档见 `docs/spec.md`（不得删改）。

## 0. 定位与运行环境

- 直连文件系统的通用 MCP server，只服务允许目录，内容形态不限。
- 不提供 shell、进程、网络、剪贴板、截屏工具。
- 语言/框架：**Go 1.27 + 官方 SDK `github.com/modelcontextprotocol/go-sdk/mcp`**，stdio transport。
- 传输：**stdio 与 streamable HTTP 双支持**（`--transport stdio|http`，默认 stdio 本地调试用）。
  HTTP 模式监听 `--addr`（默认 `:8080`），MCP 端点挂 `/{token}/mcp`——**token-path 鉴权**：URL
  路径即凭证，token 来自环境变量 `FILE_EDIT_MCP_TOKEN`（http 模式必填，缺省/空值或字符集非法
  启动即报错；校验 `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`，先于路由 pattern 构造，保证 token 无法
  注入 ServeMux 语法）。其余路径（含旧 `/mcp`、错误 token、`/`）一律 mux 404，不达 MCP handler；
  token 绝不写 stderr 日志（endpoint 以 `/<token>/mcp` 占位符记录）。**每个 HTTP 连接一个独立
  `*mcp.Server` 实例**（SDK StreamableHTTPHandler 的 GetServer 回调），即会话状态按连接隔离，
  与原来「每会话一进程」语义等价。server 必须保持 stateful（按会话 read-before-write 标记即
  核心安全特性，`Stateless=true` 会毁掉它们），而 SDK 对 `MCP-Protocol-Version: 2026-07-28`
  （SEP-2567 无会话协议，新版客户端 discover 后会带上）在 stateful server 上直接 400，故在
  token 路由之内、SDK handler 之外加一层改写中间件：请求头版本不在本 server 实际支持的版本集
  （2024-11-05、2025-03-26、2025-06-18、2025-11-25）内即改写为 initialize 协商上限
  2025-11-25（未知/未来版本同样改写，保持向前兼容）；改写后的 batch POST 仍会按 ≥2025-06-18
  的规范规则被 SDK 拒绝，属规范行为，刻意不绕过。**不再前置 nginx basic auth**：token 即唯一鉴权，轮换 = 改
  环境变量 + 重建容器。
- server instructions（MCP 的 AGENTS.md 等价物）：initialize result 的 `instructions` 字段在
  每次构造 server（stdio 与每个 HTTP 会话）时设置。默认文本仅一句话：插值实际允许目录的根
  路径清单——工具语义一律只住在工具描述里，不在这里重复（该文本每会话注入模型上下文，重复
  schema 已有事实即纯 token 成本加漂移风险；允许根路径是 schema 无法得知的唯一信息缺口）；
  环境变量 `FILE_EDIT_MCP_INSTRUCTIONS` 非空则原样覆盖（部署特定语境走此覆盖）。
- 部署形态：**单个常驻 Docker 容器**，ENTRYPOINT 即本二进制（HTTP transport）。镜像由
  开发机 `make release VERSION=...` 构建并推到 Docker Hub（`myl7/file-edit-mcp`，scratch
  多阶段 ≈ 20 MB），部署侧只 pull；部署编排在本仓库之外。
- 挂载：正式部署直接 **bind 数据子树**（如 `/data/notes:/data/notes`，同路径、默认传播），
  挂载失效的恢复交给宿主机的容器重启。经验教训：rslave 传播**只对挂载点本身**
  （`/data:/data`，`/data` 即挂载点）的 bind 有效，对子目录 bind 无效（remount 事件发生在
  子路径上方，传不进去），子树形态只能靠外部重启恢复。
- keepalive：HTTP 模式下每 4 分钟（< idle-timeout 600s）stat 一次各允许目录，防止 automount
  空闲卸载导致「文件看似消失」（卸载后 stat 返回 ENOENT 属确定类，不触发 backend 重试，
  会把「没挂上」误报成「文件不存在」）。可用 `--keepalive 0` 关闭。挂载真正断开（soft 错误）
  时走 §8 的 backend 类重试 + Root 重开。
- 内存预算：容器内常驻 RSS ≤ 20 MB；镜像 ≤ 30 MB（alpine 基底 + 静态二进制 + ripgrep）。
- 服务对象（设计目标环境）：CIFS soft 挂载（`x-systemd.automount`，idle 600s 卸载，
  `actimeo=30`，`noperm`，`file_mode/dir_mode=0777`）。inode 由服务端提供，跨重命名不可靠，
  **禁止用 inode 做文件标识**。

## 1. 模块布局

```
cmd/file-edit-mcp/main.go        入口：flag 解析、stdio server 装配
internal/pathguard/              路径校验管线（纯逻辑，可穷举测试）
internal/editengine/             Edit/MultiEdit 纯函数引擎（字节精确匹配）
internal/lineio/                 Read 的行切分、截断、行号、footer
internal/session/                会话内已读跟踪 + 每路径互斥
internal/cifsops/                errno 分类、退避重试、os.Root 池
internal/tools/                  六个 MCP 工具 handler（组装层）
internal/errmsg/                 错误消息目录（唯一允许拼错误文案的地方）
```

每个包带自己的 `*_test.go`。测试不依赖网络、不依赖真实 CIFS。

## 2. 依赖（T1 一次性固定，之后任何任务禁止改 go.mod）

- `github.com/modelcontextprotocol/go-sdk/mcp`（MCP 协议 + stdio）
- `golang.org/x/sys/unix`（errno、低级 syscall）
- `github.com/bmatcuk/doublestar/v4`（glob 的 `**`）

## 3. pathguard：路径校验管线

输入 `rawPath string`，输出 `(resolved string, err error)`。按序执行，任一步失败立即返回：

1. 含空字节 → `ENulByte`。
2. 展开 `~`（`~/x`、`~user/x` 都展开为绝对路径；展开失败按普通路径继续）。
3. 匹配 Windows 盘符形状 `^[A-Za-z]:[\\/]` → `EWindowsPath`（POSIX 主机上直接拒绝，不做智能纠正）。
4. 非绝对路径 → `ERelative`（固定选择：拒绝，不按允许目录解析；写进每个工具描述）。
5. `filepath.Clean` + `filepath.Abs` 消掉 `.`、`..`、重复分隔符；**必须在步骤 7 前完成**。
6. 对每个允许目录（同样 Clean 过）做前缀比较：相等或子路径（`dir + "/"` 前缀）通过；注意根目录 `/` 的双斜杠边界。**失败不立即拒绝**——输入可能经符号链接别名进入允许目录（如 macOS `/tmp`→`/private/tmp`，或 `/net/x` 类自动挂载），继续步骤 7 由 realpath 结果终审。
7. `filepath.EvalSymlinks`（即 realpath）解析所有已存在段；结果再做一次步骤 6 的前缀比较，这次失败才是最终 `EOutside`；**后续操作一律用 realpath 结果**。realpath 结果在界内即放行，无论步骤 6 是否通过。
8. realpath 返回 `ENOENT` 时改为校验父目录：父目录不存在 → `EParentMissing`；父目录存在且在界内 → 放行（新建文件走这条分支）。父目录按同管线递归校验（防 `a/../b` 中 a 为链接的绕过已由步骤 7 覆盖；这里只需一层）。

允许目录在启动时 Clean、EvalSymlinks 后缓存；若启动时目录不存在，报错退出（容器形态下挂载点由 `-v` 保证存在）。

## 4. os.Root 与 TOCTOU

- server 为每个允许目录持有 `*os.Root`，所有文件的 open/create/stat 走 Root 方法，
  Linux 上由内核保证不越界（openat 语义），关闭「校验后打开前被换符号链接」的窗口。
- Root 池惰性打开；遇到 backend 类错误（见 §8）时关闭并重开 Root（触发 automount 重新挂载）后重试一次。
- `os.Root` 方法集以本机 Go 1.27 实际提供为准（`go doc os.Root` 核对）：
  有 `Rename`/`CreateTemp` 就用；没有则退回 `os.Rename`/`os.CreateTemp`（路径已过 §3 管线），
  残留窗口在代码注释里写明。禁止 cgo。
- Glob/Grep 的目录遍历是只读的，走校验后的路径 + `os.ReadDir`，不要求 Root 化（可接受的残留风险，注释说明）。

## 5. 工具规格

工具名：`read`、`write`、`edit`、`multi_edit`、`glob`、`grep`。
描述用英文，简短，每条写明：只接受绝对路径；edit 系写明 exact-match、唯一性要求、无模糊回退。

### 5.1 read

参数：`file_path`(必)、`offset`(1-based 起始行，默认 1)、`limit`(行数，默认 2000)。
`offset < 1` 或 `limit <= 0` → 参数错误。

- 逐行输出 `<行号>\t<内容>`，行号是文件内真实行号（1-based 绝对值）。
- 三个上限任一触发即截断：默认行数 2000；单行 2000 字符（截断后接 `... [truncated]`）；
  单次返回总字节 256 KiB。上限是输出 payload 的约束，不是文件大小约束。
- 无效 UTF-8 字节替换为 U+FFFD 显示；文件不存在 → `ENotExist`；是目录 → `EIsDir`。
- footer 通知（凡本次未读到文件末尾都带）：写明本次行号区间、文件总行数、触发的是哪个上限、
  `continue with offset=N`。完整读到底时不带 footer。
- 行切分按 `\n`；`\r\n` 的 `\r` 保留在内容里原样输出（字节精确，不做任何规范化）。

### 5.2 write

参数：`file_path`(必)、`content`(必，完整内容)。只用于新建和整体重写，不追加。

- 目标已存在且本会话未成功 read 过 → `EUnreadWrite`。
- 目标不存在时以 `O_EXCL` 新建；若并发被抢先创建 → `EUnreadWrite`。
- 原子写：同目录临时文件（前缀 `.file-edit-mcp-`）→ 写入 → `chmod`（已存在文件沿用原 mode，
  新文件 0644）→ `rename` 覆盖目标。失败清理临时文件。CIFS 上 mode 是装饰性的，尽力而为。
- 父目录不存在 → `EParentMissing`（不自动建目录）。
- 成功后更新会话已读标记（内容即已知）。

### 5.3 edit

参数：`file_path`、`old_string`、`new_string`、`replace_all`(默认 false)。

- **逐字节精确匹配**：包含空白和缩进，不做 trim、不做缩进重排、不做任何模糊回退。
- `old_string` 为空 → `EEmptyOld`；`old_string == new_string` → `ENoop`。
- `replace_all=false` 时要求唯一：0 次命中 → `ENoMatch`；多于 1 次 → `EAmbiguous`（给次数和前 5 处行号）。
- 文件不存在 → `ENotExist`（与匹配失败是两个错误，不许合并）；未读 → `EUnreadWrite`；
  上次 read 后文件变了（size 或 mtime）→ `EStaleRead`。
- 全部编辑在内存中应用完、通过校验后才落盘（同 §5.2 的原子写路径）。
- 落盘前若 mtime/size 与 read 时记录不一致 → `EStaleRead`（防并发覆盖）。

### 5.4 multi_edit

参数：`file_path`、`edits` 数组（每项 `old_string`、`new_string`、可选 `replace_all`）。

- 按数组顺序在**逐步修改的内容**上应用；第 i 项的唯一性判断基于应用它时的当前内容。
- 任一项失败 → 整体失败、不落盘，错误指明 `edit #i`（1-based）及原因。
- 全部成功后一次性原子落盘。

### 5.5 glob

参数：`pattern`(必，支持 `**`)、`path`(搜索起点，默认所有允许目录)。

- `doublestar` 匹配，匹配的是相对 `path` 的完整相对路径。
- 返回绝对路径列表，按 mtime 倒序（最近改过的在前）。
- 上限 200 条，超出截断并报告 `showing X of Y`。
- 安全阀：遍历条目数上限 100,000，达到即停止并在结果里声明可能不完整。
  超大共享盘上这个保护是必需的，写进工具描述。

### 5.6 grep

参数：`pattern`(正则)、`path`、`glob`(文件名过滤)、
`output_mode`(`files_with_matches`(默认)/`content`/`count`)、`case_insensitive`(默认 false)、
`context`(上下文行数，仅 content 模式)、`head_limit`(结果条数上限)。

- 底层子进程 `rg`（镜像内自带）：`files_with_matches` → `-l`；`content` → `-n --no-heading`；
  `count` → `-c`；`case_insensitive` → `-i`；`context` → `-C`；`glob` → `--glob`。
  rg 退出码 1（无匹配）是正常空结果，退出码 2 才是错误。
- 服务端对最终输出应用 `head_limit`。
- rg 缺失时回退：`WalkDir` + Go `regexp`（RE2 语义与 rg 相近），无 ignore 语义，
  结果形状一致；工具描述里写明性能提示「优先用 path 收窄范围」。
- 执行超时 120s：到时杀进程，返回已得部分并声明 `search timed out; results may be incomplete`。
- content 行格式保持 rg 原样 `path:line:text`。

## 6. 错误目录（errmsg 包，唯一文案来源）

| 代号 | 文案模板（英文，`%s`/`%d` 填充） |
|---|---|
| ENulByte | `path contains a NUL byte` |
| EWindowsPath | `path %q looks like a Windows path; this server uses POSIX absolute paths (allowed: %s)` |
| ERelative | `path is not absolute; use an absolute path (allowed: %s)` |
| EOutside | `path %s is outside the allowed directories: %s` |
| EParentMissing | `parent directory does not exist: %s` |
| ENotExist | `file does not exist: %s` |
| EIsDir | `path is a directory, not a file: %s` |
| ENoMatch | `old_string not found in %s. Read the file and copy old_string exactly, including all whitespace and indentation. No fuzzy matching is performed.` |
| EAmbiguous | `old_string matches %d times in %s (first at line(s) %s). Include more surrounding context in old_string to make it unique, or set replace_all=true.` |
| ENoop | `old_string is identical to new_string; nothing to do` |
| EEmptyOld | `old_string is empty; it would match everywhere. Provide the exact text to replace.` |
| EUnreadWrite | `file exists but has not been read in this session; call read first: %s` |
| EStaleRead | `file changed since last read; read it again before editing: %s` |
| EBackend | `storage backend is unreachable or waking up (CIFS soft mount): %v. Retry shortly; if it persists, check the share server.` |
| EEditIndex | `edit #%d failed: %v`（multi_edit 包裹层） |

要求：每条错误说清原因 + 给可操作的下一步；不回显整段原文或整个文件。

## 7. 会话与并发

- 「会话」= 一个 MCP 客户端连接的生命周期。stdio 模式一进程一连接；HTTP 模式一连接一个
  server 实例（见 §0）。session 状态（已读跟踪、路径锁表）随之按连接隔离。
- `internal/session`：`map[resolvedPath]marker{size, mtime}`（read 成功时记录），
  外加 `map[resolvedPath]*sync.Mutex` 做同路径写串行化。全程持锁访问。
- MCP handler 可能并发调用；所有共享状态经 session 包的锁。
- stdout 只允许 MCP 协议帧（SDK 负责）；一切日志走 stderr（`log/slog`，`--log-level` 控制，默认 info）。

## 8. CIFS 行为（cifsops 包）

errno 两类（`errors.Is` + syscall.Errno 判断）：

- **确定类**：`ENOENT`、`ENOTDIR`、`EISDIR`、`ENAMETOOLONG` → 真实的文件系统语义，直接按 §6 报。
- **backend 类**：`EIO`、`ETIMEDOUT`、`EHOSTDOWN`、`ENOTCONN`、`ESTALE`、`ECONNRESET`、`EAGAIN`
  → 挂载抖动/冷启动。**必须与 ENOENT 分开报**：backend 类提示重试，ENOENT 不许提示重试。

重试策略（常量集中定义）：

- 进程尚未有任何一次成功 fs 操作（冷启动，automount 正在被唤醒）：
  最多 3 次，间隔 500ms / 2s / 5s，仍失败 → `EBackend`。
- 热态操作遇 backend 类错误：重开 Root（触发重新挂载）后重试 1 次（间隔 800ms），
  再失败 → `EBackend`。
- 只有 backend 类错误参与重试；确定类错误永不重试。

## 9. 测试矩阵（对齐 docs/spec.md §5）

单元层（每包自带）+ 集成层（`internal/tools/mcp_test.go`：go-sdk 客户端 over stdio 起真 server）。
必测场景（对应设计稿点名的坑）：

1. `old_string` 在文件中重复出现 → EAmbiguous 带次数和行号。
2. 目标行在 2000 行之后：不带 offset 读 → 截断 footer 正确；带 offset 续读 → 行号连续正确。
3. 超长单行（>2000 字符）→ 截断标记。
4. 文件不存在 vs 存在但匹配失败 → 两种错误可区分。
5. 父目录不存在（write 新文件）→ EParentMissing。
6. 路径越界：`..` 穿越、符号链接指向界外（用 tmp fixture 造真链接）、盘符形路径、相对路径。
7. MultiEdit 第 i 项失败 → 整体不落盘、文件内容不变。
8. 未读就写 / 读后被改 → EUnreadWrite / EStaleRead。
9. rename 原子性：写失败时目标文件保持原内容、无残留临时文件。
10. errno 分类：backend 类触发重试、ENOENT 不触发（用注入的假 op 函数测）。
11. glob 的 mtime 排序、上限截断报告。
12. grep 三种 output_mode + 无匹配 = 空结果不是错误。

Diff-XYZ 第一层评测（仅验证 apply 逻辑，不下结论）：脚本从 HF datasets-server rows API
取 30 条（固定种子、五种语言各 6 条）落成 `testdata/diffxyz-30.jsonl`（入库，测试不碰网络），
Go 测试把每条 `search-replace` 喂给 editengine，比对 `new_code`。

## 10. 构建与部署

- `Dockerfile`（canonical 多阶段）：`golang:1.27-alpine` 构建 → `alpine:3.20` 运行
  （`apk add --no-cache ripgrep`）→ 静态二进制 `CGO_ENABLED=0 -trimpath -ldflags "-s -w"`；
  ENTRYPOINT 即二进制，`CMD ["--transport","http","--addr",":8080","--allow","/data"]`
  （compose 可整体覆盖），`EXPOSE 8080`。
- `Dockerfile.prebuilt`：**发布镜像**，scratch 多阶段（从 alpine:3.20 阶段摘 rg + 三个共享库，
  加静态二进制；`ENV PATH=/usr/bin` 否则 exec.LookPath 找不到 rg 会静默降级回退引擎），
  ≈ 20 MB。开发机 `make release VERSION=v0.1.0` 构建推送 Docker Hub；部署侧只 pull。
- 部署：编排在仓库之外（部署侧只 pull 镜像、起容器）。要点：端口绑回环或内网、token-path
  鉴权（容器环境变量 `FILE_EDIT_MCP_TOKEN`，端点 `/{token}/mcp`；不前置 nginx basic auth）、
  数据子树 bind（见 §0；挂载失效靠宿主机重启容器恢复）。
- CLI：`--allow DIR`（可重复，≥1）、`--transport stdio|http`、`--addr`（http 用）、
  `--keepalive DURATION`（http 用，默认 4m，0 关闭）、`--log-level`、`--version`。
  环境变量：`FILE_EDIT_MCP_TOKEN`（http 必填，token-path 鉴权）、`FILE_EDIT_MCP_INSTRUCTIONS`
  （可选，覆盖 initialize instructions）。

## 11. 任务分解（协调用）

T1 脚手架 → T2 pathguard+errmsg → T3 editengine → T4 lineio → T5 session+cifsops
（T2–T5 可并行）→ T6 组装 read/write/edit/multi_edit → T7 glob+grep（可与 T6 并行）
→ T8 集成测试 → T9 Diff-XYZ 第一层 → T10 Docker/部署/README。
