# RDP 高性能公网中继系统设计文档

**文档版本：** V1.0  
**状态：** 最终设计基线  
**开发语言：** Go  
**核心传输：** QUIC  
**公网端：** Linux Relay  
**被控端：** Windows Agent  
**目标客户端：** Windows mstsc / 标准 RDP Client

---

# 1. 项目概述

## 1.1 项目目标

设计一个专门面向 Microsoft RDP 的公网中继服务。

典型网络环境：

```text
RDP 客户端
    │
    │ Internet
    ▼
公网 Relay
    │
    │ 被控机主动建立的 QUIC 隧道
    ▼
内网 Windows Agent
    │
    ▼
Windows RDP :3389
```

被控 Windows：

- 没有公网 IP；
- 位于公司内网、家庭 NAT、CGNAT 或多级 NAT 后；
- 不需要路由器端口映射；
- 不需要配置 UPnP；
- 不要求 IPv6；
- 只需要能够主动访问公网 Relay。

用户最终仍然使用标准 RDP：

```text
mstsc.exe

公网IP:20001
```

公网服务器同时提供：

```text
TCP 20001
UDP 20001
```

内部最终转发到：

```text
127.0.0.1:3389 TCP
127.0.0.1:3389 UDP
```

Microsoft 官方文档说明，标准 RDP 使用 TCP/UDP 3389，并且自定义 RDP 端口时，TCP 和 UDP 都应使用新的端口。

---

# 2. 核心设计目标

系统必须满足：

1. Windows Agent 主动连接 Relay；
2. 支持 RDP TCP；
3. 支持 RDP UDP；
4. UDP 不经过 TCP 隧道；
5. 公网 TCP/UDP 使用相同 RDP 端口；
6. 一个公网 IP 支持多台 Windows；
7. 一个 Agent 对应固定公网 RDP 端口；
8. Agent 自动重连；
9. Relay 重启后端口映射保持不变；
10. 无需安装专用 RDP 客户端；
11. 支持 IP ACL；
12. 支持高延迟、轻度丢包环境；
13. UDP 优先保证实时性，而不是中继层可靠性；
14. 支持性能指标、日志和故障诊断；
15. 为未来更换 QUIC 实现预留接口。

---

# 3. 非目标

V1 不实现：

```text
RDP 协议解析
RDP 用户名密码代理
RD Gateway
RDP Web Client
NAT 打洞
P2P
TURN
多 Relay HA
无缝 Session Migration
HTTP/3 伪装
流量混淆
端口跳跃
Hysteria Brutal
```

Relay 是：

> RDP TCP/UDP 高性能透明反向中继。

不是：

> 完整 RDP Gateway。

---

# 4. 为什么必须支持 UDP

Microsoft 的 RDP UDP Transport Extension 本身就是为改善 WAN、无线网络等环境下的 RDP 性能而设计。

RDP UDP 包含：

```text
Reliable Mode
Best-Effort Mode
```

其中 Best-Effort 模式不会对丢失数据进行重传，以保持数据及时性。

因此本项目不能设计成：

```text
RDP UDP
   ↓
TCP Tunnel
   ↓
Agent
```

否则会重新引入：

```text
可靠重传
队头阻塞
延迟累积
```

破坏 RDP UDP 本身的设计价值。

---

# 5. 技术选型

## 5.1 开发语言

统一使用：

```text
Go
```

Relay：

```text
Linux Go Binary
```

Agent：

```text
Windows Go EXE
```

主要原因：

- 网络编程成熟；
- goroutine 适合大量连接；
- TCP/UDP API 完整；
- Windows/Linux 跨平台；
- 单文件部署；
- Relay 与 Agent 可以共用协议代码；
- Windows Service 支持成熟；
- QUIC 生态较完善。

Windows Agent 使用：

```text
golang.org/x/sys/windows/svc
golang.org/x/sys/windows/svc/mgr
```

官方 Go 包已经提供 Windows Service 的运行、安装、启动、停止和管理能力。

---

# 6. QUIC 技术选型

V1 使用：

```text
github.com/quic-go/quic-go
```

quic-go 支持：

```text
QUIC
TLS 1.3
Bidirectional Stream
QUIC Datagram / RFC 9221
DPLPMTUD
```



核心映射：

```text
RDP TCP
   ↓
QUIC Stream

RDP UDP
   ↓
QUIC Datagram
```

---

# 7. 关于 quic-go 的重要限制

这里必须作为架构风险处理。

目前 quic-go 官方文档明确指出：

> Datagram 的 SendDatagram / ReceiveDatagram 路径尚未针对高吞吐场景充分优化。

因此：

```text
不能把整个系统和 quic-go 强耦合
```

必须设计：

```go
type Transport interface {
    OpenStream(ctx context.Context) (Stream, error)

    AcceptStream(ctx context.Context) (Stream, error)

    SendDatagram(data []byte) error

    ReceiveDatagram(ctx context.Context) ([]byte, error)

    Close() error

    Stats() TransportStats
}
```

V1：

```text
Transport
    ↓
quic-go
```

以后如果压测发现 Datagram PPS 成为瓶颈，可以替换：

```text
quic-go
   ↓
其他 QUIC 实现
```

而不用重写：

```text
UDP Session
TCP Proxy
Agent Manager
协议层
管理层
```



---

# 8. 拥塞控制策略

V1 不自行实现：

```text
BBR
Brutal
自定义 CC
```

当前 upstream quic-go 实现 RFC 9002 的拥塞控制，并没有提供成熟的可插拔 BBR 配置；BBR 相关能力仍属于其待演进方向。

因此 V1：

```text
使用 quic-go 默认拥塞控制
```

第一阶段优化重点放在：

```text
UDP Datagram
Session Multiplexing
合理 MTU
Buffer
减少 copy
Socket Buffer
降低 allocation
```

而不是自行实现拥塞控制算法。

---

# 9. 总体架构

```text
                     ┌──────────────────┐
                     │    mstsc.exe     │
                     │    RDP Client    │
                     └────────┬─────────┘
                              │
                    TCP + UDP :20001
                              │
                              ▼
             ┌─────────────────────────────┐
             │          RDP Relay          │
             │          Linux              │
             │                             │
             │ TCP Listener                │
             │ UDP Listener                │
             │ Port Manager                │
             │ Agent Manager               │
             │ UDP Session Manager         │
             │ ACL                         │
             │ Metrics                     │
             └──────────────┬──────────────┘
                            │
                            │ QUIC / UDP :443
                            │
                    Internet / NAT
                            │
                            ▼
             ┌─────────────────────────────┐
             │         RDP Agent           │
             │         Windows             │
             │                             │
             │ Windows Service             │
             │ QUIC Client                 │
             │ TCP Proxy                   │
             │ UDP Session Manager         │
             │ RDP Health Check            │
             └──────────────┬──────────────┘
                            │
                    TCP + UDP 3389
                            │
                            ▼
                   Windows TermService
```

---

# 10. 公网端口规划

推荐：

```text
Relay TCP 443
    Web/API，可选

Relay UDP 443
    Agent QUIC

TCP 20000-39999
    RDP TCP

UDP 20000-39999
    RDP UDP
```

因为：

```text
TCP 443
UDP 443
```

属于不同 transport protocol，可以同时监听。

使用 UDP 443 作为 Agent QUIC 入口，比使用：

```text
UDP 4433
```

更有利于穿越企业网络。

---

# 11. Agent 与公网端口绑定

例如：

```text
PC-001 → 20001
PC-002 → 20002
PC-003 → 20003
```

PC-001：

```text
203.0.113.10:20001/TCP
203.0.113.10:20001/UDP
```

PC-002：

```text
203.0.113.10:20002/TCP
203.0.113.10:20002/UDP
```

必须保证：

```text
TCP Port == UDP Port
```

Microsoft 官方自定义 RDP 端口配置同样要求 TCP 与 UDP 防火墙规则针对同一个新端口开放。

---

# 12. Agent QUIC 连接模型

每个 Agent：

```text
只维护 1 条 QUIC Connection
```

例如：

```text
                    QUIC Connection
                           │
          ┌────────────────┼──────────────┐
          │                │              │
   Control Stream    TCP Stream 1   TCP Stream 2
          │
          │
          ├──────── Datagram Session 1
          ├──────── Datagram Session 2
          └──────── Datagram Session 3
```

禁止：

```text
一个 TCP Connection
=
一个 QUIC Connection
```

也禁止：

```text
一个 UDP Session
=
一个 QUIC Connection
```

---

# 13. QUIC Datagram 特性

RFC 9221 Datagram：

```text
加密
受 QUIC congestion control 管理
不保证可靠送达
丢失后不重传
```

这正是 RDP UDP 所需要的中继行为。

因此：

```text
RDP UDP packet
      ↓
QUIC Datagram
```

中间层不得再增加：

```text
ACK
Retransmission
Ordered Delivery
```

---

# 14. Hysteria2 借鉴内容

Hysteria2 的 UDP 设计采用：

```text
Session ID
Packet ID
Fragment ID
Fragment Count
Payload
```

并通过 QUIC unreliable datagram 发送。

大 UDP Packet：

```text
超过 QUIC Datagram 限制
        ↓
应用层分片
```

如果任何 fragment 丢失：

```text
整个原始 UDP Packet 丢弃
```

而不是重传。

本项目采用相同核心思想。

但不直接复制 Hysteria2。

---

# 15. 为什么不直接使用 Hysteria2

Hysteria2 是通用代理：

```text
任意 TCP Destination
任意 UDP Destination
认证
伪装
混淆
地址编码
通用 SOCKS 场景
```

本系统只处理：

```text
RDP
```

目标地址固定：

```text
Agent → 127.0.0.1:3389
```

因此可以删除每包：

```text
Destination Host
Destination Port
Address Length
```

大幅简化协议。

最终形成：

> RDP-Specific QUIC Reverse Tunnel。

---

# 16. 控制面设计

每个 QUIC Connection 建立一个：

```text
Control Stream
```

Control Stream 使用可靠 QUIC Stream。

消息包括：

```text
HELLO

AUTH

REGISTER

REGISTER_ACK

PING
PONG

PORT_ASSIGN

PORT_REVOKE

AGENT_STATUS

CONFIG_UPDATE

UDP_CLOSE

ERROR

DRAIN
```

V1 控制面可以使用：

```text
4 Bytes Length
+
JSON
```

例如：

```json
{
  "type": "REGISTER",
  "device_id": "pc-001",
  "hostname": "DESKTOP-A1",
  "agent_version": "1.0.0"
}
```

数据面禁止 JSON。

---

# 17. Agent 注册流程

流程：

```text
Agent Startup
     │
     ▼
读取 Device ID
     │
     ▼
QUIC Connect Relay:443/UDP
     │
     ▼
TLS Handshake
     │
     ▼
Control Stream
     │
     ▼
AUTH
     │
     ▼
REGISTER
     │
     ▼
Relay 查询端口
     │
     ▼
PORT_ASSIGN
```

响应：

```json
{
  "type": "PORT_ASSIGN",
  "public_host": "203.0.113.10",
  "public_port": 20001
}
```

Agent 日志：

```text
Connected
RDP endpoint: 203.0.113.10:20001
```

---

# 18. 身份认证

V1 使用：

```text
Device ID
+
256-bit Device Secret
```

TLS 已经保护传输过程。

Relay 数据库不得保存：

```text
明文 Secret
```

只保存：

```text
Secret Hash
```

Agent：

```text
C:\ProgramData\RDPRelay\config.yaml
```

必须限制 ACL：

```text
SYSTEM
Administrators
```

后续可以升级为：

```text
mTLS
Device Certificate
```

---

# 19. TCP 数据通道

用户执行：

```text
mstsc 203.0.113.10:20001
```

建立：

```text
Client
  │
  │ TCP 20001
  ▼
Relay
```

Relay：

```text
Accept TCP
    ↓
根据 LocalPort 查找 Agent
    ↓
Agent Online?
    ↓
Open QUIC Bidirectional Stream
```

Stream Header：

```text
Version        uint8
Type           uint8
ConnectionID   uint32
TargetPort     uint16
```

共：

```text
8 Bytes
```

例如：

```text
Version      1
Type         TCP_OPEN
ConnectionID 10086
TargetPort   3389
```

Agent：

```text
Accept QUIC Stream
      ↓
Dial 127.0.0.1:3389
      ↓
TCP_OPEN_OK
      ↓
开始转发
```

---

# 20. TCP 数据路径

最终：

```text
mstsc
  │
  │ TCP
  ▼
Relay TCP Socket
  │
  │ QUIC Stream
  ▼
Agent
  │
  │ TCP
  ▼
127.0.0.1:3389
```

实现模型：

```go
go io.Copy(stream, tcpConn)
go io.Copy(tcpConn, stream)
```

RDP TCP payload 不解析、不修改。

---

# 21. UDP Session 设计

Microsoft RDP UDP Server 默认通过一个 UDP 端口处理 UDP RDP，而客户端的不同 UDP Transport 实例使用独立 UDP socket。

Relay 收到：

```text
113.20.30.40:53128
        ↓
203.0.113.10:20001
```

Session Key 定义：

```text
AgentID
PublicPort
ClientIP
ClientPort
```

例如：

```text
pc-001
20001
113.20.30.40
53128
```

生成：

```text
SessionID = uint32
```

例如：

```text
0x71A24F33
```

---

# 22. UDP Session 不增加 OPEN RTT

为了性能，UDP 数据路径不设计成：

```text
UDP packet
   ↓
UDP_OPEN
   ↓
等待 ACK
   ↓
UDP_DATA
```

否则第一次 UDP Packet 会额外增加 RTT。

采用：

> Lazy Session Creation。

当 Agent 收到一个未知：

```text
SessionID
```

的 UDP_DATA：

```text
发现 Session 不存在
       ↓
自动创建 Session
       ↓
创建本地 UDP Socket
       ↓
立即发送 Payload
```

因此：

```text
UDP_DATA 本身即可隐式 OPEN
```

这是比单独 UDP_OPEN 更适合 RDP 的设计。

---

# 23. UDP Datagram Wire Format

定义固定 Header：

```text
Version       uint8
Type          uint8

SessionID     uint32
PacketID      uint32

FragmentID    uint8
FragmentCount uint8

Payload       []byte
```

固定 Header：

```text
12 Bytes
```

类型：

```text
0x11 UDP_DATA
```

控制面的：

```text
UDP_CLOSE
```

仍走可靠 Control Stream。

---

# 24. 为什么 PacketID 使用 uint32

Hysteria2 使用：

```text
uint16 Packet ID
```

本系统使用：

```text
uint32
```

增加：

```text
2 Bytes
```

但可以大幅降低：

```text
高 PPS
+
Packet ID wraparound
+
Fragment Reassembly
```

之间冲突的风险。

对 RDP 而言额外 2 Bytes 基本可以忽略。

---

# 25. UDP 正向路径

完整路径：

```text
RDP Client

113.20.30.40:53128
        │
        │ UDP
        ▼
203.0.113.10:20001
        │
        ▼
Relay UDP Listener
        │
        ▼
Session Lookup
        │
        ▼
SessionID = 1001
        │
        ▼
Encode UDP_DATA
        │
        ▼
QUIC SendDatagram
        │
        ▼
Agent ReceiveDatagram
        │
        ▼
Session 1001
        │
        ▼
Local UDP Socket
        │
        ▼
127.0.0.1:3389
```

---

# 26. Agent UDP Socket 模型

每个 Session：

```text
一个独立本地 UDP Socket
```

例如：

```text
Session 1001
    ↓
127.0.0.1:51001
    ↓
127.0.0.1:3389
```

另一个：

```text
Session 1002
    ↓
127.0.0.1:51002
    ↓
127.0.0.1:3389
```

禁止所有公网客户端复用同一个本地 source socket。

数据结构：

```go
type UDPSession struct {
    ID uint32

    ClientAddr netip.AddrPort

    Conn *net.UDPConn

    LastSeen atomic.Int64
}
```

---

# 27. UDP 返回路径

Windows RDP：

```text
127.0.0.1:3389
        │
        ▼
Agent Session Socket
        │
        ▼
SessionID
        │
        ▼
UDP_DATA
        │
        ▼
QUIC Datagram
        │
        ▼
Relay
        │
        ▼
Session Lookup
        │
        ▼
ClientAddr
        │
        ▼
113.20.30.40:53128
```

Relay 必须从：

```text
203.0.113.10:20001
```

向客户端返回 UDP。

这样 mstsc 始终认为对端是：

```text
203.0.113.10:20001
```

---

# 28. UDP 不重传策略

例如：

```text
Packet 100 ✓
Packet 101 ✓
Packet 102 ×
Packet 103 ✓
Packet 104 ✓
```

Relay/Agent：

```text
100
101
103
104
```

禁止：

```text
等待 102
重传 102
然后 103
```

QUIC Datagram 本身也是：

```text
丢失后不重传
```



中继层遵循：

> stale data is worse than lost data。

---

# 29. Datagram 尺寸

不能假设公网：

```text
MTU = 1500
```

企业 VPN、PPPoE、Overlay 网络可能更小。

V1 使用保守：

```text
MAX_PAYLOAD = 1150 Bytes
```

即：

```text
12 Byte Header
+
<= 1150 Byte Payload
```

避免 Datagram 过大。

quic-go 本身支持 DPLPMTUD，但目前 Datagram API 对“当前最大可用 Datagram Payload”暴露能力仍有限，因此 V1 使用保守静态值，并通过压测调整。

---

# 30. UDP 分片

例如收到：

```text
RDP UDP Packet = 2600 Bytes
```

则：

```text
PacketID = 10086

Fragment 0
1150 Bytes

Fragment 1
1150 Bytes

Fragment 2
300 Bytes
```

Header：

```text
PacketID      = 10086
FragmentCount = 3
```

分别：

```text
FragmentID 0
FragmentID 1
FragmentID 2
```

通过三个独立 QUIC Datagram 发送。

---

# 31. UDP 重组

Reassembly Key：

```text
SessionID
+
PacketID
```

数据结构：

```go
type Reassembly struct {
    SessionID uint32
    PacketID  uint32

    FragmentCount uint8

    Fragments [][]byte

    CreatedAt time.Time
}
```

只有：

```text
全部 Fragment 到达
```

才能：

```text
Write UDP → 3389
```

如果缺失任意 Fragment：

```text
DROP whole UDP packet
```

这和 Hysteria2 的 UDP fragmentation 策略一致。

---

# 32. Fragment 超时

默认：

```text
100ms
```

配置：

```yaml
udp:
  reassemblyTimeout: 100ms
```

建议允许范围：

```text
50ms - 300ms
```

超过：

```text
直接丢弃
```

绝不触发：

```text
Fragment retransmission
```

---

# 33. Reassembly 安全限制

必须限制：

```text
Maximum UDP Packet = 64 KiB

Maximum Fragment Count = 64

Maximum Reassembly Per Session = 32

Maximum Global Reassembly Memory

Maximum TTL = 300ms
```

异常：

```text
FragmentID >= FragmentCount

FragmentCount == 0

FragmentCount > 64

Packet > 64 KiB
```

直接：

```text
DROP
```

避免内存 DoS。

---

# 34. UDP Session 生命周期

状态：

```text
NEW
 │
 ▼
ACTIVE
 │
 ├── idle timeout
 │
 ├── Agent disconnect
 │
 ├── UDP_CLOSE
 │
 └── RDP association cleanup
 │
 ▼
CLOSED
```

默认：

```text
UDP_IDLE_TIMEOUT = 60s
```

60 秒没有任何收发：

```text
Relay 删除 Session
Agent 删除 Session
Agent Close UDP Socket
```

Hysteria2 同样使用 inactivity 作为 UDP Session 回收的重要方式。

---

# 35. Session 回收

不要：

```text
每个 Session
=
一个 time.Timer
```

大量 Session 会造成 Timer 压力。

建议：

```text
统一 Session Sweeper
```

例如：

```text
每 10 秒
```

扫描：

```text
LastSeen < now - 60s
```

然后回收。

---

# 36. QUIC 心跳

QUIC Connection 使用：

```text
KeepAlive
```

业务控制面另外提供：

```text
PING / PONG
```

推荐：

```text
heartbeat = 10s
```

业务心跳用于：

```text
Agent 在线状态
管理平台
RTT 统计
健康状态
```

而不是替代 QUIC 本身的 connection management。

---

# 37. Agent 自动重连

连接中断：

```text
1s
2s
4s
8s
15s
30s
30s
...
```

增加：

```text
±20% jitter
```

防止 Relay 重启时产生：

```text
Thundering Herd
```

重连成功：

```text
Device ID
    ↓
恢复原公网端口
```

例如：

```text
pc-001
永远保持 20001
```

---

# 38. QUIC 断线后的 RDP 行为

V1 不实现 Session Resume。

如果 Agent QUIC Connection 断开：

```text
现有 TCP Stream
    ↓
全部关闭

现有 UDP Session
    ↓
全部关闭
```

RDP Client：

```text
发生断线
```

Agent 重连后：

```text
新的 RDP 连接可以建立
```

不试图透明恢复旧 TCP Stream。

---

# 39. RDP 健康检测

Agent 周期性检查：

```text
127.0.0.1:3389 TCP
```

状态区分：

```text
Agent Offline

Agent Online
RDP Offline

Agent Online
RDP Online
```

RDP 不可用时：

```text
Agent QUIC 仍然保持
```

Relay 管理层可以明确显示问题所在。

---

# 40. Windows RDP UDP 检查

Microsoft RDP Server 默认：

```text
UDP 3389
```

处理 UDP RDP 流量。

Agent 应启动时检查：

```text
TCP 3389

UDP 3389 listener
```

可以通过 Windows API 或：

```text
Get-NetUDPEndpoint
```

辅助诊断。

---

# 41. 公网 RDP ACL

公网 RDP 最大的安全问题不是 Agent，而是：

```text
20001/TCP
20001/UDP
```

长期暴露 Internet。

V1 必须支持：

```text
Source IP ACL
```

例如：

```yaml
acl:
  allow:
    - 1.2.3.4/32
    - 10.0.0.0/8
```

推荐默认：

```text
DENY ALL
```

显式允许。

---

# 42. 临时授权模式

后续推荐提供：

```text
Allow My Current IP
```

例如用户当前：

```text
1.2.3.4
```

Relay 添加：

```text
1.2.3.4/32
→
20001 TCP/UDP

TTL 30min
```

30 分钟后自动删除。

这样可以显著降低公网 RDP 扫描风险。

---

# 43. UDP DoS 防护

攻击者可以：

```text
向 UDP 20001
大量发送不同 source port
```

诱导创建 Session。

必须限制：

```text
maxSessionsPerIP
maxSessionsPerAgent
maxSessionCreateRate
maxUDPppsPerIP
maxReassemblyMemory
```

建议初始值：

```text
maxSessionsPerIP       16
maxSessionsPerAgent    64
sessionCreateRate      50/s
```

实际值通过压测调整。

---

# 44. Agent 鉴权安全

必须：

```text
TLS Server Verification
```

禁止：

```go
InsecureSkipVerify: true
```

生产 Agent 只信任：

```text
relay.example.com
```

证书。

Device Secret：

```text
至少 256 bit Random
```

禁止：

```text
password123
device-id 本身作为 token
```

---

# 45. 数据加密

Agent ↔ Relay：

```text
QUIC
 ↓
TLS 1.3
```

因此：

```text
TCP Payload
UDP Payload
Control Message
```

均在 QUIC 连接内加密。

Relay 与 RDP Client：

```text
正常 RDP 自身安全机制
```

中继不负责解密 RDP。

---

# 46. Relay 数据库存储

V1：

```text
SQLite
+
WAL
```

## agents

```text
id
device_id
hostname
secret_hash

public_port

enabled

created_at
last_seen
```

## connection_log

```text
id
device_id

protocol

source_ip
source_port

start_time
end_time

rx_bytes
tx_bytes
```

禁止记录：

```text
RDP Password
RDP Username
RDP Screen Content
```

---

# 47. Relay 高性能 UDP 内核配置

Hysteria2 官方性能建议在 Linux 上提高 UDP socket buffer，例如：

```bash
sysctl -w net.core.rmem_max=16777216
sysctl -w net.core.wmem_max=16777216
```

即：

```text
16 MiB
```



本项目 Relay V1 同样使用这一配置作为初始推荐值。

最终数值必须通过实际 PPS 压测确定。

---

# 48. 内存分配优化

UDP hot path 禁止每 Packet：

```go
make([]byte, 2048)
```

优先：

```text
Buffer Pool
```

例如：

```go
var packetPool = sync.Pool{
    New: func() any {
        b := make([]byte, 2048)
        return &b
    },
}
```

流程：

```text
Pool.Get
 ↓
UDP Read
 ↓
Encode Header
 ↓
Send Datagram
 ↓
Pool.Put
```

需要通过：

```text
pprof
```

确认 Pool 是否真正带来收益。

---

# 49. UDP Hot Path 原则

目标：

```text
Receive UDP
      ↓
Session Lookup
      ↓
Write Fixed Header
      ↓
Send Datagram
```

不要：

```text
JSON Marshal

protobuf reflection

map[string]interface{}

多层 object copy

日志格式化
```

数据面协议必须保持固定二进制结构。

---

# 50. Session Map

V1：

```go
map[uint32]*UDPSession
+
sync.RWMutex
```

即可。

客户端 Endpoint Map：

```go
map[SessionKey]*UDPSession
```

SessionKey 建议使用：

```go
type SessionKey struct {
    AgentID    uint64
    PublicPort uint16
    ClientAddr netip.AddrPort
}
```

不要在 hot path 使用字符串：

```text
"1.2.3.4:12345"
```

作为主 Key。

---

# 51. Agent 并发模型

```text
Main
 │
 ├── QUIC Connection Loop
 │
 ├── Control Loop
 │
 ├── Datagram Receive Loop
 │
 ├── Session Sweeper
 │
 └── RDP Health Loop
```

TCP：

```text
1 TCP Session
=
1 goroutine pair
```

UDP：

```text
一个 Datagram Dispatch Loop

+
每个本地 UDP Session
独立 receive loop
```

先保持简单。

压测出现瓶颈以后再优化。

---

# 52. Relay 并发模型

```text
Relay
 │
 ├── QUIC Listener
 │
 ├── Agent Manager
 │
 ├── Port Manager
 │
 ├── TCP Listeners
 │
 ├── UDP Listeners
 │
 ├── Session Manager
 │
 ├── Session Sweeper
 │
 └── Metrics
```

V1：

```text
每 Agent：
1 TCP Listener
1 UDP Socket
```

例如：

```text
PC001 → TCP/UDP 20001
PC002 → TCP/UDP 20002
```

几百、几千设备阶段实现简单可靠。

---

# 53. 大规模端口优化

如果未来达到：

```text
10000+
Agent
```

才考虑：

```text
共享 Socket
SO_REUSEPORT
IP_PKTINFO
eBPF
UDP fan-out
```

V1 不提前复杂化。

---

# 54. QUIC 流控

QUIC Stream 流控主要影响：

```text
RDP TCP
```

Hysteria2 官方文档说明，其默认 Stream receive window 约 8 MiB、Connection receive window 约 20 MiB，并建议保持合理的 stream/connection window 比例。

本项目不要直接复制所有 Hysteria 参数。

建议初始：

```text
Stream Window      8 MiB
Connection Window  20-32 MiB
```

压测：

```text
高 RTT
高带宽
多 Stream
```

后调整。

---

# 55. 日志设计

INFO：

```text
Agent Connected
Agent Disconnected

TCP Session Open
TCP Session Close

UDP Session Open
UDP Session Close

RDP Online
RDP Offline
```

DEBUG：

```text
Reconnect
Fragment Drop
Session Timeout
QUIC Error
```

禁止 INFO：

```text
UDP Packet Received
UDP Packet Sent
```

否则高 PPS 下日志本身会成为瓶颈。

---

# 56. Metrics

Relay 必须暴露：

```text
agents_online

quic_connections

rdp_tcp_active

rdp_udp_sessions

tcp_rx_bytes
tcp_tx_bytes

udp_rx_bytes
udp_tx_bytes

udp_rx_packets
udp_tx_packets

udp_datagram_drop

udp_fragment_created
udp_fragment_timeout

udp_session_created
udp_session_expired

quic_rtt

agent_reconnect_total
```

---

# 57. 推荐 Prometheus 指标

例如：

```text
rdprelay_agent_online

rdprelay_tcp_connections

rdprelay_udp_sessions

rdprelay_udp_packets_total

rdprelay_udp_fragment_timeout_total

rdprelay_bytes_total

rdprelay_quic_rtt_seconds
```

避免：

```text
每 Session ID
每 Source IP
```

作为 Metric Label。

否则容易：

```text
高 cardinality
```

---

# 58. Agent Windows Service

Service：

```text
Name:
RDPRelayAgent

DisplayName:
RDP Relay Agent

StartType:
Automatic
```

命令：

```text
rdp-agent.exe install

rdp-agent.exe uninstall

rdp-agent.exe start

rdp-agent.exe stop

rdp-agent.exe status

rdp-agent.exe run
```

其中：

```text
run
```

以前台方式运行，方便测试。

---

# 59. Agent 配置

推荐：

```yaml
server:
  address: relay.example.com:443

device:
  id: pc-001
  secret: xxxxxxxxxxxxxxxxx

rdp:
  address: 127.0.0.1:3389

transport:
  datagramPayload: 1150

udp:
  sessionIdleTimeout: 60s
  reassemblyTimeout: 100ms
  maxFragments: 64

heartbeat:
  interval: 10s

reconnect:
  maxInterval: 30s

log:
  level: info
```

---

# 60. Relay 配置

```yaml
server:
  quic:
    listen: ":443"

rdp:
  publicHost: "203.0.113.10"

  portRange:
    start: 20000
    end: 39999

udp:
  datagramPayload: 1150

  sessionIdleTimeout: 60s

  reassemblyTimeout: 100ms

  maxFragments: 64

security:
  defaultPolicy: deny

storage:
  type: sqlite

  path: /var/lib/rdp-relay/relay.db

metrics:
  listen: "127.0.0.1:9090"

log:
  level: info
```

---

# 61. Windows 文件目录

程序：

```text
C:\Program Files\RDPRelay\
    rdp-agent.exe
```

运行数据：

```text
C:\ProgramData\RDPRelay\
    config.yaml
    agent.log
```

不要把配置写到：

```text
Program Files
```

---

# 62. Linux Relay 目录

```text
/usr/local/bin/
    rdp-relay

/etc/rdp-relay/
    config.yaml

/var/lib/rdp-relay/
    relay.db

/var/log/rdp-relay/
    relay.log
```

---

# 63. Relay systemd

```ini
[Unit]
Description=RDP Relay
After=network-online.target

[Service]
ExecStart=/usr/local/bin/rdp-relay \
  --config /etc/rdp-relay/config.yaml

Restart=always
RestartSec=3

LimitNOFILE=1048576

[Install]
WantedBy=multi-user.target
```

---

# 64. Docker 部署

Relay 如果使用 Docker：

```yaml
services:
  rdp-relay:
    image: rdp-relay:latest

    network_mode: host

    restart: unless-stopped

    volumes:
      - ./config.yaml:/etc/rdp-relay/config.yaml
      - ./data:/var/lib/rdp-relay
```

推荐：

```text
network_mode: host
```

因为需要：

```text
20000-39999 TCP
20000-39999 UDP
```

大量动态端口。

不建议配置成数万个 Docker `ports:` 映射。

---

# 65. 项目代码结构

推荐 Monorepo：

```text
rdp-relay/
│
├── cmd/
│   ├── relay/
│   │   └── main.go
│   │
│   └── agent/
│       └── main.go
│
├── internal/
│
│   ├── protocol/
│   │   ├── control.go
│   │   ├── tcp.go
│   │   └── udp.go
│
│   ├── transport/
│   │   ├── transport.go
│   │   └── quicgo/
│   │
│   ├── relay/
│   │   ├── agent_manager.go
│   │   ├── port_manager.go
│   │   ├── tcp_listener.go
│   │   └── udp_listener.go
│
│   ├── agent/
│   │   ├── client.go
│   │   ├── reconnect.go
│   │   └── health.go
│
│   ├── tcp/
│   │   └── proxy.go
│
│   ├── udp/
│   │   ├── session.go
│   │   ├── packet.go
│   │   ├── fragment.go
│   │   └── reassembly.go
│
│   ├── service/
│   │   └── windows.go
│
│   ├── storage/
│   │   └── sqlite.go
│
│   └── metrics/
│
├── configs/
│
└── go.mod
```

---

# 66. Transport 抽象

这是代码设计中的重要边界。

```go
type Transport interface {
    OpenStream(
        ctx context.Context,
    ) (Stream, error)

    AcceptStream(
        ctx context.Context,
    ) (Stream, error)

    SendDatagram(
        payload []byte,
    ) error

    ReceiveDatagram(
        ctx context.Context,
    ) ([]byte, error)

    Stats() TransportStats

    Close() error
}
```

V1：

```text
transport/quicgo
```

未来：

```text
transport/msquic
transport/quinn
transport/custom
```

而上层完全不变。

---

# 67. UDP Encoder 必须零反射

例如：

```go
func EncodeUDP(
    dst []byte,
    sessionID uint32,
    packetID uint32,
    fragID uint8,
    fragCount uint8,
    payload []byte,
) []byte
```

直接使用：

```text
binary.BigEndian.PutUint32
```

不要：

```text
reflection
protobuf
JSON
```

---

# 68. UDP Header 字节序

统一：

```text
Network Byte Order
=
Big Endian
```

正式定义：

```text
Offset  Size  Field

0       1     Version

1       1     Type

2       4     SessionID

6       4     PacketID

10      1     FragmentID

11      1     FragmentCount

12      N     Payload
```

协议版本：

```text
Version = 1
```

---

# 69. 协议兼容性

Agent REGISTER：

```text
min_protocol_version
max_protocol_version
```

Relay：

```text
选择双方共同支持版本
```

避免以后：

```text
Relay 升级
=
所有 Agent 必须同时升级
```

---

# 70. Agent 离线行为

Agent 离线：

TCP：

```text
Accept
  ↓
发现 Agent Offline
  ↓
立即 Close
```

UDP：

```text
DROP
```

端口：

```text
继续保留
```

禁止：

```text
PC-001 离线
 ↓
把 20001 分配给 PC-002
```

避免公网地址漂移。

---

# 71. Agent QUIC 被防火墙阻止

QUIC 依赖：

```text
UDP
```

如果企业网络禁止 UDP 443：

```text
Agent 无法建立 V1 QUIC Tunnel
```

V1 明确：

```text
QUIC 不通
=
Agent Offline
```

V2 可以增加：

```text
TLS/TCP fallback
```

但 fallback 模式应该：

```text
RDP UDP Disable
```

让 Windows 自然退化成：

```text
RDP TCP
```

而不是设计：

```text
UDP over TCP
```

---

# 72. 性能测试必须覆盖

使用：

```text
tc netem
```

构造网络。

场景：

```text
RTT 10ms
Loss 0%
```

```text
RTT 50ms
Loss 0.5%
```

```text
RTT 100ms
Loss 1%
```

```text
RTT 150ms
Loss 3%
```

```text
RTT 200ms
Loss 5%
```

---

# 73. 对比组

必须比较：

```text
A. LAN / Direct RDP

B. Relay TCP Only

C. TCP + UDP QUIC Relay
```

重点观察：

```text
拖窗口
滚动页面
浏览器
IDE
视频
全屏动画
文本输入
```

---

# 74. UDP 验证

需要确认：

```text
mstsc
```

实际发送 UDP。

Relay Metrics：

```text
udp_packets > 0
```

同时 Windows RDP 连接信息应显示：

```text
UDP enabled
```

否则不能因为：

```text
RDP 看起来很流畅
```

就认为 UDP 隧道已经生效。

---

# 75. V1 性能验收目标

这些是工程目标，不是理论保证。

Agent 空闲：

```text
CPU < 1%

内存目标 < 50 MiB
```

Relay：

```text
500+ idle Agents
```

应稳定保持。

单 RDP：

```text
50 Mbps+
```

数据路径不得形成明显应用层瓶颈。

Relay 新增处理延迟目标：

```text
p50 < 2ms

p99 < 10ms
```

在：

```text
Relay CPU 未饱和
同地域网络
```

情况下测试。

---

# 76. quic-go Datagram 性能验收门

因为当前 quic-go 官方明确提醒 Datagram 路径未针对高吞吐充分优化，所以 V1 正式上线前必须专门做：

```text
QUIC Datagram PPS Benchmark
```

至少测：

```text
1K PPS
5K PPS
10K PPS
20K PPS
50K PPS
```

观察：

```text
CPU
GC
Allocation
Loss
Latency
Queue
```



如果在目标规模前成为瓶颈：

```text
不得通过无限增加 goroutine 解决
```

而应重新评估：

```text
QUIC implementation
Datagram API
batch IO
runtime allocation
```

---

# 77. Go Profiling

必须开启：

```text
pprof
```

压测观察：

```text
CPU profile

heap profile

alloc_objects

goroutine

mutex

block
```

优化顺序：

```text
先测
 ↓
找 bottleneck
 ↓
优化
```

不要提前进行：

```text
Lock-free
unsafe
复杂对象池
```

---

# 78. V1 开发阶段

## Phase 1：基础隧道

实现：

```text
QUIC Connect
Agent Auth
Control Stream
TCP Proxy
```

目标：

```text
mstsc
通过 Relay TCP 成功连接
```

---

## Phase 2：UDP

实现：

```text
UDP Listener
Session Manager
QUIC Datagram
Agent UDP Socket
```

目标：

```text
mstsc RDP UDP 生效
```

---

## Phase 3：UDP Fragment

实现：

```text
Packet ID
Fragment
Reassembly
Timeout
```

---

## Phase 4：可靠性

实现：

```text
Agent reconnect
Heartbeat
Port persistence
Health check
Session cleanup
```

---

## Phase 5：安全

实现：

```text
TLS validation
Agent secret
ACL
Rate limit
DoS limits
```

---

## Phase 6：性能

实现：

```text
Buffer tuning
pprof
allocation optimization
Datagram benchmark
Linux sysctl
```

---

# 79. V1 不应提前实现的功能

不要提前做：

```text
P2P NAT 穿透

WebRTC

TURN

多节点 HA

BBR

Brutal

eBPF

io_uring

Rust 重写

自研 QUIC

无锁数据结构
```

先把：

```text
RDP TCP + UDP
```

正确、稳定地跑通。

---

# 80. 最终技术基线

最终正式选型：

```text
Language
    Go


Relay
    Linux


Agent
    Windows Service


QUIC V1
    quic-go


Agent Tunnel Port
    UDP 443


RDP Public Ports
    TCP + UDP
    20000-39999


RDP Internal
    TCP + UDP
    127.0.0.1:3389


TCP Transport
    QUIC Bidirectional Stream


UDP Transport
    QUIC Datagram


UDP Design
    Hysteria2-inspired


UDP Header
    12 Bytes


Session ID
    uint32


Packet ID
    uint32


Fragment ID
    uint8


Fragment Count
    uint8


Datagram Payload
    1150 Bytes


Fragment Timeout
    100ms


UDP Session Timeout
    60s


UDP Retransmission
    NONE


Storage
    SQLite WAL


Windows Service
    x/sys/windows/svc


Metrics
    Prometheus


Profiling
    pprof
```

---

# 81. 最终数据流

```text
                         mstsc.exe
                             │
              ┌──────────────┴─────────────┐
              │                            │
             TCP                          UDP
              │                            │
              ▼                            ▼
      Public :20001                Public :20001
              │                            │
              ▼                            ▼
         Relay TCP                   Relay UDP
              │                            │
              ▼                            ▼
        QUIC Stream                QUIC Datagram
              │                            │
              └────────────┬───────────────┘
                           │
                      QUIC UDP 443
                           │
                           │
                         NAT
                           │
                           ▼
                   Windows Agent
                           │
              ┌────────────┴─────────────┐
              │                          │
        QUIC Stream                QUIC Datagram
              │                          │
             TCP                        UDP
              │                          │
              └────────────┬─────────────┘
                           ▼
                    127.0.0.1:3389
                           │
                           ▼
                  Windows RDP Server
```

---

# 82. 最终设计原则

项目开发过程中以下规则视为不可破坏的架构约束：

```text
RDP UDP 不允许通过 TCP Tunnel。

RDP UDP 使用 QUIC Datagram。

中继层不为 UDP 增加可靠重传。

TCP 与 UDP 使用相同公网 RDP 端口。

一个 Agent 默认只维护一条 QUIC Connection。

一个公网 TCP Connection 对应一个 QUIC Stream。

一个公网 UDP Endpoint 对应一个 UDP Session。

一个 UDP Session 对应一个 Agent 本地 UDP Socket。

UDP Session 首包隐式创建，不等待 OPEN ACK。

UDP 数据面只使用固定二进制 Header。

分片缺失直接丢弃整个 UDP Packet。

所有 Session、Fragment、Buffer 都必须存在资源上限。

Agent QUIC Connection 与具体 QUIC 库之间必须有 Transport 抽象。

公网 RDP 默认启用 ACL。

性能问题必须通过 benchmark / pprof 定位。

不因为追求理论性能而提前增加系统复杂度。
```

---

# 83. 项目最终定位

最终产品定义：

> RDP Relay 是一个使用 QUIC 构建、专门针对 Microsoft RDP TCP + UDP 传输特征优化的高性能反向公网中继系统。

其核心区别于 FRP 一类传统反向代理的是：

```text
RDP TCP
    ↓
QUIC Stream

RDP UDP
    ↓
QUIC Datagram
```

并参考 Hysteria2 的：

```text
UDP Session
Packet ID
Fragmentation
Unreliable Datagram
Drop instead of retransmit
```

模型，对 RDP UDP 做专门优化。

整个系统最核心的架构可以浓缩成：

```text
                RDP CLIENT

             TCP         UDP
              │           │
              ▼           ▼
           Stream      Datagram
              │           │
              └─────┬─────┘
                    │
                   QUIC
                    │
                 Internet
                    │
                   QUIC
                    │
              ┌─────┴─────┐
              │           │
           Stream      Datagram
              │           │
             TCP         UDP
              │           │
              └─────┬─────┘
                    ▼
             localhost:3389
```

该架构作为 RDP Relay V1 后续协议设计、代码开发、测试、部署以及性能优化的唯一技术基线。