# PowerGo · 抛二钩

> 跨机器 AI 执行钩子 —— 让 AI 安全地操作另一台电脑上的文件、命令和 Python。

**PowerGo** 由 **Power**Shell 和 **Go**lang 组合而来：用 Go 编写的轻量执行器，提供类似 PowerShell 的远程执行能力，但更聚焦于「让 AI Agent 能调用」这一场景。
中文名 **抛二钩** —— 一台机器抛出问题，另一台机器钩住并执行。

面向的真实问题是：**有些机器跑不了现代 AI 工具**（例如只能用 Windows 7 的老机器、工控机、内网隔离机），但它上面有你需要操作的文件、软件和环境。PowerGo 在那台机器上放一个单文件执行器，就能让局域网内任何一台能跑 AI 的电脑远程驱动它。

---

## 核心特性

- **单文件零依赖** — 纯静态编译，`CGO_ENABLED=0`，Windows 7 双击即用
- **零配置发现** — UDP 广播自动找到局域网内的执行器，IP 变了会自动重连
- **一次配对** — 6 位配对码换长期凭据，此后所有命令无需再带地址和 Token
- **双入口** — 命令行 `PGAcli` 供人使用，MCP 适配层 `PGAmcpd` 供 AI 客户端使用
- **中文友好** — GBK ↔ UTF-8 自动转码，中文路径、`dir` 输出、Python 打印都不乱码
- **路径沙箱** — 所有文件操作被限制在配置的根目录白名单内，符号链接逃逸也会拦截
- **Windows 7 兼容** — 锁定 Go 1.20 工具链构建，覆盖 Win7 SP1 到 Win11

---

## 架构

```text
┌──────────────────────────────────┐         ┌──────────────────────────────────┐
│  控制端 (Windows 10 / 11)         │         │  被控端 (Windows 7 SP1 及以上)    │
│                                  │         │                                  │
│   AI 客户端 ──stdio──┐            │         │                                  │
│                     ├── PGAmcpd ──┼── HTTP ─┼──▶ PGCact                         │
│   人 ───────────────┴── PGAcli    │  :7777  │      │                           │
│                                  │         │      ├──▶ 文件读写                 │
└──────────────────────────────────┘         │      ├──▶ 命令 / 脚本             │
              ╰────── UDP 7778 自动发现 ─────┼──────┤      └──▶ Python              │
                                             └──────────────────────────────────┘
```

三个程序，职责单一：

| 程序 | 位置 | 职责 |
| :--- | :--- | :--- |
| **PGCact** | 被控端 | HTTP + JSON-RPC 2.0 服务，提供 21 个能力 |
| **PGAcli** | 控制端 | 命令行客户端，供人手动操作 |
| **PGAmcpd** | 控制端 | MCP 适配层，把 21 个能力暴露给 AI 客户端 |

`PGAcli` 与 `PGAmcpd` 是并列关系，共用同一套凭据。MCP 不可用时命令行随时可兜底。

---

## 快速开始

### 第一步：被控端启动

把 `PGCact_386.exe`（32 位系统）或 `PGCact_amd64.exe` 拷到目标机器，双击运行。

首次启动会自动生成 `pgcact.json`，控制台会打印类似内容：

```text
  PowerGo (抛二钩) PGCact v1.0.0
  ------------------------------------------------------------
  配置文件    D:\PowerGo\pgcact.json
  监听地址    0.0.0.0:7777
  本机 IP     192.168.1.50
  节点 ID     node_3216fcb5e608
  Token       5dfd80d6...（48 位，无需手抄）
  允许根目录  [C:\ D:\ E:\ F:\]
  ------------------------------------------------------------
  配对码      482913
  在控制端执行 PGAcli pair，输入上面这 6 位数字即可接入
  广播发现    UDP 7778 已开启
  ------------------------------------------------------------
```

放行端口（管理员权限执行一次）：

```bat
netsh advfirewall firewall add rule name="PowerGo TCP" dir=in action=allow protocol=TCP localport=7777
netsh advfirewall firewall add rule name="PowerGo UDP" dir=in action=allow protocol=UDP localport=7778
```

> 想让执行器开机自动运行，可写入注册表启动项（不需要管理员权限）：
> ```bat
> reg add "HKCU\Software\Microsoft\Windows\CurrentVersion\Run" /v PowerGo /t REG_SZ /d D:\PowerGo\PGCact_386.exe /f
> ```

### 第二步：控制端配对

```powershell
.\PGAcli.exe pair
```

它会广播搜索局域网内的执行器，找到后要求输入被控端显示的 6 位配对码。配对成功后凭据保存在 `%USERPROFILE%\.powergo\credentials.json`，**配对码会自动作废并换成新的**。

### 第三步：开始使用

```powershell
.\PGAcli.exe info
.\PGAcli.exe ls C:\
.\PGAcli.exe exec "ipconfig"
```

不再需要任何地址或 Token 参数。

---

## PGAcli 命令参考

### 节点管理

| 命令 | 说明 |
| :--- | :--- |
| `PGAcli pair [配对码]` | 发现并配对执行器，配对码可省略改为交互输入 |
| `PGAcli discover` | 列出局域网内所有在线执行器（不配对） |
| `PGAcli nodes` | 查看本地已配对的节点 |
| `PGAcli forget <主机名\|节点ID>` | 删除某个节点的凭据 |

### 文件操作

| 命令 | 说明 |
| :--- | :--- |
| `PGAcli ls <path> [-a]` | 列目录，`-a` 含隐藏项 |
| `PGAcli cat <path> [offset] [limit]` | 输出文件内容，适合管道；`offset` 为起始行号 |
| `PGAcli read <path>` | 输出内容 + 元信息（大小、行数、是否截断） |
| `PGAcli write <path> [内容\|-]` | 覆盖写入，内容为 `-` 时从 stdin 读 |
| `PGAcli append <path> [内容\|-]` | 追加写入 |
| `PGAcli push <本地> <远程>` | 上传文件（3 MB 分块，无体积上限） |
| `PGAcli pull <远程> <本地>` | 下载文件 |
| `PGAcli rm <path> [-r]` | 删除，非空目录必须显式 `-r` |
| `PGAcli mkdir <path>` | 创建目录（递归） |
| `PGAcli mv <src> <dst> [-f]` | 移动 / 重命名，跨盘自动降级为复制 |
| `PGAcli cp <src> <dst> [-f]` | 复制文件或目录树 |
| `PGAcli find <root> [pattern] [-c 关键字]` | 递归搜索，自动跳过 `.git`、`node_modules` 等 |

### 执行

| 命令 | 说明 |
| :--- | :--- |
| `PGAcli exec <命令...>` | 同步执行，输出实时流式回传 |
| `PGAcli spawn <命令...>` | 后台执行，立即返回任务 ID |
| `PGAcli tasks` | 列出后台任务 |
| `PGAcli log <id> [offset]` | 增量拉取任务输出 |
| `PGAcli kill <id>` | 终止任务及其子进程树 |
| `PGAcli py <代码>` | 执行 Python（每次新进程） |
| `PGAcli py -f <脚本>` | 执行本地 Python 文件 |
| `PGAcli repl <代码> [-s 会话名] [-reset]` | 常驻 Python 会话，变量跨调用保留 |

### 系统

| 命令 | 说明 |
| :--- | :--- |
| `PGAcli info` | 主机名、系统、CPU、内存、磁盘、Python 版本 |
| `PGAcli ps [过滤词]` | 进程列表 |
| `PGAcli health` | 探活，不需要 Token |
| `PGAcli tools` | 列出服务端全部工具及 JSON Schema |
| `PGAcli rpc <method> [json]` | 直接调用任意 JSON-RPC 方法 |

### 全局参数

| 参数 | 说明 |
| :--- | :--- |
| `-addr <url>` | 手动指定地址，跳过自动发现（跨网段时用） |
| `-token <token>` | 手动指定 Token，跳过配对 |
| `-no-discover` | 禁用广播发现，只用本地凭据 |
| `-disc-port <端口>` | 发现端口，默认 7778 |
| `-json` | 输出原始 JSON |
| `-no-stream` | 关闭流式输出，等命令结束一次性返回 |
| `-timeout <秒>` | 请求超时 |

### 常用示例

```powershell
# 看被控端状况
PGAcli info

# 实时看命令输出
PGAcli exec "ping -n 4 127.0.0.1"

# 跑一段 Python
PGAcli py "import sys; print(sys.version)"

# 常驻会话，变量保留
PGAcli repl "data = [i*2 for i in range(10)]"
PGAcli repl "print(sum(data))"        # 输出 90

# 长任务后台跑
PGAcli spawn "python train.py"
PGAcli log t_d7690d9928f4dd73         # 拉取输出
PGAcli kill t_d7690d9928f4dd73

# 传文件
PGAcli push .\build.zip D:\deploy\build.zip

# 管道配合
PGAcli cat D:\app.log | Select-String "ERROR"
```

---

## MCP 接入

`PGAmcpd` 是标准的 MCP stdio 服务端，可被任何支持 MCP 的 AI 客户端调用。

### 客户端配置

在客户端的 MCP 配置文件中加入：

```json
{
  "mcpServers": {
    "powergo": {
      "command": "D:\\PowerGo\\PGAmcpd.exe",
      "args": []
    }
  }
}
```

> 各客户端的配置文件位置不同：Claude Desktop 在 `%APPDATA%\Claude\claude_desktop_config.json`，
> Cursor 在 `.cursor/mcp.json`，WorkBuddy 在 `%USERPROFILE%\.workbuddy\mcp.json`。
> 保存配置后需在客户端的连接器管理界面「信任」这个服务，部分客户端需要重启。

### 前置条件

`PGAmcpd` 复用 `PGAcli` 的配对凭据，因此**先在控制端执行一次 `PGAcli pair`**。

### 工具命名

MCP 规范要求工具名匹配 `^[a-zA-Z0-9_-]{1,64}$`，所以内部方法名会做转换：

| 内部方法 | MCP 工具名 |
| :--- | :--- |
| `fs.read` | `fs_read` |
| `exec.run` | `exec_run` |
| `python.repl` | `python_repl` |

### 调试

```powershell
.\PGAmcpd.exe -debug
```

调试日志输出到 **stderr**（stdout 是协议通道，不能污染）。

---

## 能力清单

被控端共暴露 21 个方法，`PGAcli` 和 MCP 共用同一套：

| 分组 | 方法 |
| :--- | :--- |
| 文件 | `fs.read` `fs.write` `fs.append` `fs.list` `fs.stat` `fs.mkdir` `fs.remove` `fs.move` `fs.copy` `fs.search` |
| 执行 | `exec.run` `exec.spawn` |
| 任务 | `task.list` `task.log` `task.kill` |
| Python | `python.detect` `python.exec` `python.repl` |
| 系统 | `sys.info` `sys.procs` `sys.env` |

---

## 配置文件

被控端首次启动会在可执行文件同目录生成 `pgcact.json`：

```json
{
  "listen": "0.0.0.0:7777",
  "token": "自动生成的 48 位十六进制",
  "allowed_roots": ["C:\\", "D:\\"],
  "allow_ip": [],
  "default_timeout_sec": 60,
  "max_timeout_sec": 1800,
  "max_read_bytes": 4194304,
  "max_output_bytes": 8388608,
  "python_path": "",
  "work_dir": "D:\\PowerGo",
  "log_file": "pgcact.log",
  "node_id": "node_3216fcb5e608",
  "pair_code": "482913",
  "pairing_enabled": true,
  "discovery_port": 7778
}
```

| 字段 | 说明 |
| :--- | :--- |
| `listen` | 监听地址，`0.0.0.0:7777` 表示所有网卡 |
| `allowed_roots` | **文件操作白名单**，越界一律拒绝。收紧这里是最有效的防护 |
| `allow_ip` | 允许连接的来源 IP，支持 `192.168.1.0/24` 网段。为空表示不限制 |
| `max_*` | 各类上限，防止单次请求过大 |
| `python_path` | 指定 Python 解释器；留空则依次尝试 `python` / `python3` / `py` |
| `work_dir` | 默认工作目录，`exec` 和脚本的相对路径基准 |
| `node_id` | 节点稳定标识，**重装系统才会变**，凭据按它匹配 |
| `pairing_enabled` | 是否开放配对码接入；设为 `false` 后只能手动配 Token |
| `discovery_port` | UDP 发现端口；设为 `0` 关闭广播发现 |

---

## 安全模型

PowerGo 的定位是「可信局域网内的远程执行」，它的安全边界是**网络边界 + 配对信任**。

### 三层防护

**1. 首次信任靠物理接触**

配对码只打印在被控端的控制台窗口上。能物理接触到那台机器的人才能看到，相当于"首次信任由物理访问建立"。配对成功后配对码立即轮换，旁观者记下的旧码会失效。

**2. 凭据按节点绑定**

Token 与 `node_id` 绑定存放在 `%USERPROFILE%\.powergo\credentials.json`。IP 变化不会影响凭据有效性。

**3. 路径沙箱**

所有文件操作先过 `allowed_roots` 白名单校验，包括符号链接和目录联接的真实路径解析。

### 暴力破解防护

配对接口对单个 IP 限制 5 次失败，超过后锁定 60 秒。6 位配对码在该限制下无法被穷举。

### 已知的安全权衡

- **HTTP 明文传输** — 局域网内可被嗅探。跨不可信网段请自行加隧道（如 WireGuard、SSH 端口转发）
- **持有 Token 即拥有全权** — 在 `allowed_roots` 范围内的读写、执行能力没有二次确认
- **无命令级过滤** — `exec` 不拦截任何命令，安全依赖路径沙箱和使用者判断

### 建议的加固配置

面向生产环境时：

```json
{
  "allow_ip": ["192.168.1.10"],
  "allowed_roots": ["D:\\Work"],
  "pairing_enabled": false
}
```

`allow_ip` 限制来源、`allowed_roots` 收窄可写范围、关闭配对后只能手动配 Token。

---

## 兼容性

| 项目 | 支持范围 |
| :--- | :--- |
| 被控端系统 | Windows 7 SP1 / 8 / 10 / 11 / Server 2008 R2 及以上 |
| 控制端系统 | Windows 10 / 11（MCP 适配层只出 64 位） |
| 架构 | 386（32 位）、amd64 |
| Go 版本 | **1.20.14**（最后一个支持 Windows 7 的版本） |
| Python | 3.7+，需自行安装在 PATH 或配置 `python_path` |

> Windows 7 用户请使用 `PGCact_386.exe`。Go 1.21 起不再支持 Windows 7，因此本项目锁定 1.20.14 工具链构建。

---

## 构建

需要本机已安装 Go 1.21+（用于自动拉取 1.20.14 工具链）：

```bat
build.bat
```

或手动：

```bash
set GOTOOLCHAIN=go1.20.14
set CGO_ENABLED=0
set GOOS=windows

set GOARCH=386
go build -trimpath -ldflags "-s -w" -o dist/PGCact_386.exe ./cmd/pgcact

set GOARCH=amd64
go build -trimpath -ldflags "-s -w" -o dist/PGCact_amd64.exe ./cmd/pgcact
go build -trimpath -ldflags "-s -w" -o dist/PGAcli.exe ./cmd/pgacli
go build -trimpath -ldflags "-s -w" -o dist/PGAmcpd.exe ./cmd/pgamcpd
```

### 项目结构

```text
powergo/
├── cmd/
│   ├── pgcact/           # 被控端执行器
│   ├── pgacli/           # 命令行客户端
│   └── pgamcpd/          # MCP 适配层
├── internal/
│   ├── config/           # 配置加载与凭据生成
│   ├── discovery/        # UDP 广播发现
│   ├── remote/           # HTTP 客户端与凭据存储
│   ├── rpc/              # JSON-RPC 2.0 消息定义
│   ├── sandbox/          # 路径白名单
│   ├── server/           # HTTP 路由、鉴权、配对
│   ├── task/             # 后台任务与环形缓冲
│   ├── tool/             # 21 个能力的实现
│   └── winenc/           # GBK ↔ UTF-8 转换
└── build.bat
```

---

## 协议细节

控制端与被控端之间走 **JSON-RPC 2.0 over HTTP**，与 MCP 同构。

| 端点 | 用途 |
| :--- | :--- |
| `POST /rpc` | 方法调用入口 |
| `POST /rpc?stream=1` | 流式模式，返回 NDJSON，每行一条消息 |
| `GET /health` | 探活，不需要鉴权 |
| `GET /tools` | 工具清单（含 JSON Schema） |
| `POST /pair` | 配对，唯一不需要 Token 的写接口 |

发现协议：UDP 7778，客户端广播 `{"magic":"POWERGO","kind":"probe","nonce":"..."}`，服务端单播回应节点信息。

错误码：

| 码 | 含义 |
| :--- | :--- |
| `-32700` | JSON 解析失败 |
| `-32600` | 请求格式错误 |
| `-32601` | 方法不存在 |
| `-32602` | 参数错误 |
| `1001` | 鉴权失败 |
| `1002` | 路径越界 / 权限不足 |
| `1003` | 目标不存在 |
| `1004` | 执行超时 |
| `1005` | 状态冲突 |

---

## License

[MIT](LICENSE)
