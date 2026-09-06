# RDPulse: 高性能 RDP 穿透与中继系统 (V2.0)

基于 **QUIC (TLS 1.3) + TLS/TCP fallback + UDP/TCP P2P** 构建，专门针对 Microsoft Remote Desktop Protocol (RDP) 传输特征（TCP + UDP 独立双通道）优化的高性能远程桌面连接系统。

系统参考了 RustDesk 的分布式组网与打洞架构思想，结合 Hysteria2 的零重传 UDP 传输设计，实现 **“P2P 直连优先、QUIC 中继兜底、QUIC 被封锁时退回 TLS/TCP”** 的双通道传输。

> 当前实现边界：UDP P2P 支持同一受控端多会话；TCP 直连支持 LAN/可路由 Candidate，并为每条 RDP TCP 连接独立鉴权建链；QUIC Stream/Datagram Relay、独立 TLS/TCP fallback 和公网端口兼容模式均已闭环。TLS/TCP fallback 按设计禁用 RDP UDP，避免 UDP-over-TCP 队头阻塞。

---

## 🌟 核心特性 (V2.0)

### 1. 最终连接与选路策略
- **TCP 与 UDP 独立双通道选路**：
  - **TCP 路径**：`P2P TCP` → `QUIC Stream Relay` → `TLS/TCP Relay`
  - **UDP 路径**：`P2P UDP` → `QUIC Datagram Relay`；TLS/TCP fallback 下禁用
- **Happy-Eyeballs 并发竞速**：
  - 会话建立时，并发向局域网（LAN）与公网反射（Reflexive）候选地址发起 40 字节紧凑 UDP 打洞探测。
  - 若 300ms 内 P2P 未能就绪，无缝激活预热的 QUIC Relay 通路，实现会话建立零卡顿。
  - P2P 打洞成功后自动热升级路径，降低公网中继服务器带宽与时延开销。

### 2. 双使用模式支持
- **Enhanced Mode (推荐模式)**：
  - 控制端运行 `rdp-agent connect <target_id>`，本地监听 `127.0.0.1:13389` (TCP+UDP)。
  - 自动完成 Rendezvous 探测、Candidate 交换与 P2P 打洞，并自动唤起 Windows 原生 `mstsc.exe /v:127.0.0.1:13389`。
  - 对用户和 Windows 原生 RDP 体验完全透明，尽享 P2P 低至局域网级的极速响应。
- **Compatibility Mode (经典兼容模式)**：
  - 控制端无需安装 Agent，直接在 `mstsc` 中输入 Relay 分配的公网端口（例如 `relay-ip:20001`），完全兼容原 V1.0 QUIC 中继。

### 3. 高性能传输引擎
- **无重传分片与重组**：UDP 流量默认分片（1150 字节）。超时缺失直接丢弃整个 UDP 帧（Drop instead of retransmit），杜绝传统 TCP 代理带来的队头阻塞与延迟累积。
- **Rendezvous & NAT 探测服务**：服务端集成 UDP 21116 端口，提供高效 NAT 映射探测（Reflexive Candidate 反射）。
- **P2P 数据面认证**：打洞报文和后续 UDP 数据均绑定 SessionID，并使用 HMAC-SHA256 防伪；接收端同时校验对端地址和 PacketID 防重放窗口。
- **默认拒绝的设备安全策略**：未知设备必须持有按 DeviceID 绑定、带有效期且只能原子消费一次的 invitation；禁用设备不可重新注册；Controller 只能访问显式授权的 Target。
- **连接级抗滥用**：Relay 鉴权、Enrollment、TLS 握手、公网 TCP 和会话创建均配置并发/速率上限。
- **持久化防漂移**：Relay 采用 SQLite WAL 存储设备与端口映射，断网重启端口保持不变。
- **Windows Service 原生支持**：内嵌服务管理器，支持一键注册、启动和自启。

---

## 📂 项目结构

```text
RDPulse/
├── cmd/
│   ├── relay/              # Relay & Rendezvous 服务端入口
│   │   └── main.go
│   └── agent/              # Agent 客户端入口 (Controller / Controlled)
│       └── main.go
├── internal/
│   ├── protocol/           # 核心协议（Control, TCP Header, UDP Header, Punch 报文）
│   ├── nat/                # NAT 网卡扫描与公网 Candidate 探测
│   ├── rendezvous/         # Rendezvous UDP 21116 反射服务
│   ├── signaling/          # P2P 会话协调器与信令交换
│   ├── punch/              # UDP / TCP P2P 并发打洞引擎与 Keepalive
│   ├── path/               # 路径管理器 (Happy-Eyeballs 并发竞速与动态切换)
│   ├── controller/         # 控制端本地 127.0.0.1:13389 代理与 mstsc 桥接
│   ├── transport/          # 传输抽象接口与 quic-go 实现
│   ├── relay/              # Relay 核心服务（AgentManager, PortManager, TCP/UDP 监听）
│   ├── agent/              # Agent 核心逻辑（Client, Controller, 重连, 健康检测）
│   ├── tcp/                # TCP 流代理转发引擎
│   ├── udp/                # UDP 会话管理、分片拆包与超时重组
│   ├── acl/                # IP CIDR 白名单访问控制
│   ├── service/            # Windows Service 服务封装
│   ├── storage/            # SQLite WAL 数据持久化
│   └── config/             # YAML 配置解析
├── configs/
│   ├── relay.yaml          # Relay 配置文件
│   └── agent.yaml          # Agent 配置文件
├── bin/                    # 预编译二进制文件
└── test/
    ├── e2e_test.go         # V1 中继端到端测试
    └── p2p_e2e_test.go     # V2 P2P + Relay 混合选路端到端测试
```

---

## 🚀 快速开始

### 1. 预编译二进制文件与跨平台支持

`bin/` 目录下已预先构建全平台原生可执行文件（静态编译，无外部 CGO 依赖）：
- **Windows x64**：`rdp-relay.exe`、`rdp-agent.exe`
- **Linux amd64**：`rdp-relay-linux-amd64`
- **Linux arm64 (AArch64)**：`rdp-relay-linux-arm64`
- **Linux armv7 (32位 ARM)**：`rdp-relay-linux-armv7`
- **macOS Apple Silicon (M1/M2/M3/M4)**：`rdp-relay-darwin-arm64`

如需重新编译：
```powershell
# 编译 Windows 版本
go build -ldflags="-s -w" -o bin/rdp-relay.exe ./cmd/relay
go build -ldflags="-s -w" -o bin/rdp-agent.exe ./cmd/agent

# 交叉编译 Linux 与 macOS M4 版本
$env:CGO_ENABLED="0"
$env:GOOS="linux";  $env:GOARCH="amd64"; go build -ldflags="-s -w" -o bin/rdp-relay-linux-amd64 ./cmd/relay
$env:GOOS="linux";  $env:GOARCH="arm64"; go build -ldflags="-s -w" -o bin/rdp-relay-linux-arm64 ./cmd/relay
$env:GOOS="linux";  $env:GOARCH="arm"; $env:GOARM="7"; go build -ldflags="-s -w" -o bin/rdp-relay-linux-armv7 ./cmd/relay
$env:GOOS="darwin"; $env:GOARCH="arm64"; $env:GOARM=""; go build -ldflags="-s -w" -o bin/rdp-relay-darwin-arm64 ./cmd/relay
```

---

### 2. 服务端部署 (Relay + Rendezvous)

在公网服务器上运行（开放 UDP 21116 用于 Rendezvous，443/UDP 用于 QUIC，443/TCP 用于 TLS fallback，以及配置的被控端口范围如 20000-20100）：

```bash
./bin/rdp-relay-linux-amd64 --config configs/relay.yaml
```

系统化守护进程部署可参考 `scripts/rdp-relay.service`。

---

### 3. 被控端部署 (Controlled PC)

编辑 `configs/agent.yaml`：
```yaml
server:
  address: "relay.example.com:443"
  tlsAddress: "relay.example.com:443"
  rendezvousAddress: "relay.example.com:21116"
  caCert: "C:\\RDPulse\\relay-ca.pem"
  insecureSkipVerify: false

device:
  id: "office-pc"
  # 每台设备独立生成的至少 32 字节随机密钥
  secret: ""
  # 仅首次注册使用；必须对应 Relay 中同 DeviceID 的未过期 invitation
  enrollmentToken: ""

rdp:
  address: "127.0.0.1:3389"
```

#### 前台调试运行：
```powershell
./bin/rdp-agent.exe run --config configs/agent.yaml
```

#### 安装为 Windows 系统服务（开机自启）：
```powershell
# 管理员权限运行
./bin/rdp-agent.exe install --config C:\RDPulse\agent.yaml
./bin/rdp-agent.exe start
./bin/rdp-agent.exe status
```

---

### 4. 控制端连接 (Controller PC)

#### 方式 A：Enhanced 极速模式（自动 P2P 打洞）
在控制端 Windows 电脑上：
```powershell
./bin/rdp-agent.exe connect office-pc --config configs/agent.yaml
```
- 控制端 Agent 会在本地监听 `127.0.0.1:13389`；
- 与目标设备进行 UDP 打洞并按连接尝试 TCP 直连，同时保持 Relay 可立即使用；
- 自动启动 `mstsc.exe /v:127.0.0.1:13389` 进入远程桌面；
- 优先走 P2P 直连，打洞不通无感回退 Relay 中继。

#### 方式 B：Compatibility 兼容模式（免客户端）
直接打开系统的 `mstsc.exe`，输入被控端在 Relay 上的公网映射地址：
```text
relay-ip:20001
```

---

## 📖 完整中文配置说明

### 0. 通用约定

- 配置采用 YAML；省略的字段使用程序内置默认值，未列出的未知字段会被忽略。
- 所有时间字段使用 Go duration 文本：`10s`、`500ms`、`2m30s`、`1h`。
- 布尔字段使用 YAML 的 `true` / `false`。
- 时间点字段使用 RFC 3339，例如 `"2030-01-01T00:00:00Z"`，建议用引号包裹。
- Relay 启动时执行严格校验：缺少必填项、证书配置不完整、邀请无效或过期都会直接启动失败并打印原因。
- Agent 校验规则：`server.address`、`device.id` 必填；`device.secret` 至少 32 字节；配置了 `device.enrollmentToken` 时至少 32 字节。
- RDP UDP 数据帧的分片重组上限由协议固定为 64 片，不受配置调大影响。

### 1. Relay 服务端配置（`configs/relay.yaml`）

```yaml
server:
  quic:
    listen: ":443"                 # QUIC UDP 监听地址（host:port），默认 ":443"
    certFile: "/etc/rdp-relay/tls/fullchain.pem"
    keyFile: "/etc/rdp-relay/tls/privkey.pem"
    allowEphemeralCertificate: false
    # 生产必须同时配置 certFile/keyFile。缺证书时 fail-closed 拒绝启动；
    # 只有显式设置 allowEphemeralCertificate: true 才在开发环境临时生成自签证书。
  tls:
    listen: ":443"                 # TLS/TCP fallback 监听地址，可与 QUIC 同端口
    disabled: false                # true 表示关闭 TLS/TCP fallback
  rendezvous:
    listen: ":21116"               # UDP Rendezvous/NAT 反射服务，默认 ":21116"

rdp:
  publicHost: "rdp.example.com"    # 必填；分配给兼容模式的公网主机名/IP
  portRange:
    start: 20000                   # 每个被控端绑定公网端口的起始值
    end: 39999                     # 结束值（含），重启后端口保持稳定

udp:
  datagramPayload: 1150            # 单分片 RDP UDP 最大负载字节数
  sessionIdleTimeout: 60s          # 空闲 UDP 会话超时
  reassemblyTimeout: 100ms         # 分片重组等待时间，超时丢弃整帧
  maxSessionsPerAgent: 256         # 单个被控端公网端口的 UDP 会话上限
  maxSessionsPerIP: 32             # 同一客户端源 IP 的会话上限
  maxSessionCreateRate: 50         # 每秒允许新建会话数

security:
  defaultPolicy: deny              # "deny" 或 "allow"；兼容模式访问控制默认策略
  allow: []                        # 显式放行列表，支持 203.0.113.5/32、10.0.0.0/8 等 CIDR
  enrollmentInvitations: []        # 按设备绑定的首次注册邀请
  # - deviceID: "PC-TARGET-B"
  #   token: "GENERATE_A_UNIQUE_RANDOM_VALUE_OF_AT_LEAST_32_BYTES"
  #   expiresAt: "2030-01-01T00:00:00Z"
  allowAllControllerTargets: false # true 后任何已认证 Controller 可访问任意 Target
  controllerAccess: {}             # 键为 Target 设备 ID，值为允许访问的 Controller ID 列表
  limits:
    maxConnectionsPerIP: 16        # 同一源 IP 到 Relay 控制通道的并发连接数
    maxConnectionRatePerMinute: 60 # 同一源 IP 每分钟新建控制通道上限
    maxEnrollmentRatePerMinute: 5  # 同一源 IP 每分钟邀请注册尝试上限
    maxPublicTCPPerIP: 32          # 同一源 IP 并发公网 TCP 连接上限
    maxPublicTCPRatePerSecond: 20  # 同一源 IP 每秒新建公网 TCP 连接上限

storage:
  type: sqlite                     # 目前仅支持 sqlite
  path: "/var/lib/rdp-relay/relay.db"

metrics:
  listen: "127.0.0.1:9090"         # Prometheus /metrics 与 pprof 监听

log:
  level: info                      # 已定义字段；当前版本日志框架尚未按此过滤
```

补充说明：

- 旧版全局 `security.enrollmentToken` 已移除，配置后会在加载阶段直接报错；请改用 `security.enrollmentInvitations`。
- 每台新设备只允许一条未过期、同 `deviceID` 的 invitation；token 至少 32 字节且应使用密码学随机数。
- invitation 首次注册时被原子消费一次；同设备同 token 已消费后，即使重启 Relay 也不会复活。需要重新开放时，用新 token 轮换该设备记录。
- `controllerAccess["目标ID"] = ["*"]` 表示任意 Controller 可访问该 Target；`controllerAccess["*"] = ["控制器ID"]` 表示该 Controller 可访问任意 Target。`allowAllControllerTargets: true` 等价于后者使用 `"*"`。两者都应只在明确需要时使用。
- `defaultPolicy: deny` 配合 `allow: []` 时，未放行 IP 无法通过兼容模式公网端口访问；Enhanced 模式仍由 Controller 身份与 `controllerAccess` 控制。

### 2. Agent 配置（`configs/agent.yaml`）

```yaml
server:
  address: "relay.example.com:443"     # 必填；QUIC 使用的 UDP 主机:端口
  tlsAddress: "relay.example.com:443"  # TLS fallback 地址；省略时等于 address
  rendezvousAddress: "relay.example.com:21116" # NAT 反射候选探测服务
  caCert: "C:\\RDPulse\\relay-ca.pem"  # Relay 自签/私有 CA 证书 PEM；留空用系统信任库
  insecureSkipVerify: false            # 仅开发/调试临时开启，生产必须为 false
  disableQUIC: false                   # true 时跳过 QUIC，直接尝试 TLS/TCP fallback
  disableTLSFallback: false            # true 时 QUIC 失败后不再尝试 TLS/TCP
  quicDialTimeout: 4s                  # 单次 QUIC 拨号超时

device:
  id: "office-pc"                      # 必填；设备唯一 ID
  secret: ""                           # 至少 32 字节随机密钥；已注册设备直接用于 AUTH
  enrollmentToken: ""                  # 仅首次注册需要，与 Relay 中同 ID invitation 对应

rdp:
  address: "127.0.0.1:3389"            # 被控端本机 RDP 地址；Controller 连接时不影响转发

transport:
  datagramPayload: 1150                # RDP UDP 单分片负载，应与 Relay 保持一致
  disableP2P: false                    # true 时本端不发起/接受 UDP/TCP P2P，强制走 Relay

udp:
  sessionIdleTimeout: 60s
  reassemblyTimeout: 100ms

heartbeat:
  interval: 10s                        # Relay 保活心跳周期

reconnect:
  maxInterval: 30s                     # 断线重连最大退避间隔（从 1s 起指数退避）

log:
  level: info                          # 已定义字段；当前版本日志框架尚未按此过滤
  path: "agent.log"                    # 已定义字段；当前版本未启用文件输出
```

补充说明：

- 同一份 Agent 配置既可用于被控端，也可用于控制端。被控端把 `rdp.address` 指向本机 RDP 服务；控制端执行 `connect <targetID>` 时该字段不参与转发。
- Controller 连接前必须满足授权：控制端设备自身已注册，且目标设备在 Relay `controllerAccess` 中允许该 Controller。
- `device.enrollmentToken` 只在普通鉴权收到 428 challenge（未知设备）后发送，不会在常规 AUTH 中泄漏。
- 若 `server.disableQUIC` 与 `server.disableTLSFallback` 同时为 `true`，Agent 将没有任何 Relay 通道可建立连接，属于预期错误配置。
- 双端 `transport.disableP2P`、`transport.datagramPayload` 建议保持一致；TLS/TCP fallback 按设计禁用 RDP UDP，只承载 TCP 流。

### 3. 最小可用配置示例

Relay 首次启用（证书由外部签发）：

```yaml
server:
  quic:
    listen: ":443"
    certFile: "/etc/letsencrypt/live/rdp.example.com/fullchain.pem"
    keyFile: "/etc/letsencrypt/live/rdp.example.com/privkey.pem"
  tls:
    listen: ":443"
  rendezvous:
    listen: ":21116"
rdp:
  publicHost: "rdp.example.com"
  portRange: { start: 20000, end: 39999 }
security:
  defaultPolicy: deny
  enrollmentInvitations:
    - deviceID: "office-pc"
      token: "REPLACE_WITH_32_PLUS_RANDOM_BYTES"
      expiresAt: "2030-01-01T00:00:00Z"
  controllerAccess:
    office-pc: ["home-laptop"]
storage:
  type: sqlite
  path: "/var/lib/rdp-relay/relay.db"
```

被控端首次注册后，应清空 `device.enrollmentToken` 并重启 Agent，避免后续配置被误用。

---

## 🧪 自动化测试验证

全量自动化测试覆盖了单元协议、P2P 仿真打洞、NAT 反射探测以及真实端到端混合链路：

```powershell
go test -v ./...
```
包含：
- `TestUDPPunchingSimulation`：双对等端 UDP 紧凑报文打洞与 Keepalive 校验
- `TestTCPPunchingSimulation`：Controller/受控端双角色 TCP 鉴权握手
- `TestUDPDispatcherRoutesConcurrentSessions`：共享 UDP socket 的 SessionID 并发分发
- `TestTLSMuxStreamsBidirectional`：独立 TLS/TCP 多路复用传输
- `TestRendezvousProbe`：UDP 21116 反射候选地址探测
- `TestEndToEndRelay`：完整 QUIC Stream (TCP) 与 Datagram (UDP) 中继闭环
- `TestP2PAndControllerEndToEnd`：Enhanced 模式 TCP/UDP P2P、多 Controller 并发会话验证
- `TestForcedQUICRelayControllerEndToEnd`：关闭 P2P 后强制验证 QUIC Stream/Datagram Relay
- `TestForcedTLSRelayControllerEndToEnd`：关闭 QUIC/P2P 后强制验证 TLS/TCP Relay 与 UDP 禁用
- `TestExternalNATEndToEnd`：接入独立公网 Relay 与受控端，验证真实 NAT/CGNAT 下的选路和直连 TCP 鉴权（默认跳过）

真实 NAT 测试需要在与受控端不同的网络运行，并由环境变量显式启用：

```powershell
$env:RDPULSE_EXTERNAL_RELAY_ADDR="relay.example.com:443"
$env:RDPULSE_EXTERNAL_TLS_ADDR="relay.example.com:443"
$env:RDPULSE_EXTERNAL_RENDEZVOUS_ADDR="relay.example.com:21116"
$env:RDPULSE_EXTERNAL_CONTROLLER_ID="external-controller"
$env:RDPULSE_EXTERNAL_CONTROLLER_SECRET="至少 32 字节的设备密钥"
$env:RDPULSE_EXTERNAL_ENROLLMENT_TOKEN="首次注册时使用的 invitation，可留空"
$env:RDPULSE_EXTERNAL_TARGET_ID="office-pc"
$env:RDPULSE_EXTERNAL_CA_CERT="C:\\RDPulse\\relay-ca.pem"
$env:RDPULSE_EXTERNAL_EXPECT_TCP="direct" # any/direct/relay
$env:RDPULSE_EXTERNAL_EXPECT_UDP="direct" # any/direct/relay/disabled
go test -v ./test -run TestExternalNATEndToEnd -count=1
```

若测试环境使用临时证书，可显式设置 `RDPULSE_EXTERNAL_INSECURE_SKIP_VERIFY=true`；还可用 `RDPULSE_EXTERNAL_DISABLE_P2P` 或 `RDPULSE_EXTERNAL_DISABLE_QUIC` 强制覆盖相应回退场景。
