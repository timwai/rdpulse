# RDPulse 高性能 RDP 穿透与中继系统设计文档

**文档版本：** V2.0  
**状态：** 核心架构已实现，远程真实 NAT 场景持续验证  
**项目名称：** RDPulse  
**开发语言：** Go  
**公网服务端：** Linux  
**客户端：** Windows  
**目标协议：** Microsoft RDP TCP + UDP

> 实现状态（2026-09）：多会话 UDP P2P、按连接鉴权的 TCP 直连、会话级 Trickle Candidate、QUIC Stream/Datagram Relay、独立 TLS/TCP fallback、一次性设备邀请、Controller→Target 授权及连接限流均已实现。TLS/TCP fallback 禁用 RDP UDP；TCP 直连当前使用 LAN/可路由 Candidate。真实双 NAT、CGNAT 和公网丢包场景以环境驱动的集成测试持续验证；能力清单以 README 和自动化测试为准。

---

# 1. 项目定位

RDPulse 是一个专门针对 Microsoft RDP 设计的高性能远程连接网络层。

主要解决：

```text
被控 Windows 位于：

企业内网
家庭 NAT
CGNAT
多层 NAT
无公网 IP

↓

仍然可以进行高性能 RDP
```

整体设计参考 RustDesk 的：

```text
Rendezvous
+
NAT Hole Punching
+
P2P 优先
+
Relay 兜底
```

RustDesk 官方架构中，`hbbs` 负责 ID、Rendezvous 和 Signaling，两端优先尝试 Hole Punching；只有打洞失败后才通过 `hbbr` Relay 转发数据。

RDPulse 在此基础上针对 RDP 增加：

```text
RDP TCP / UDP 独立选路

UDP P2P

TCP P2P

QUIC Stream Relay

QUIC Datagram Relay

TCP/TLS Relay

Hysteria2 风格 UDP Session / Fragment

Windows mstsc 兼容
```

---

# 2. 最终连接优先级

全局优先级：

```text
① P2P Direct
   │
   ├── UDP P2P
   └── TCP P2P
          │
          │ 失败
          ▼

② QUIC Relay
   │
   ├── RDP TCP → QUIC Stream
   └── RDP UDP → QUIC Datagram
          │
          │ UDP / QUIC 不可用
          ▼

③ TCP/TLS Relay
   │
   └── RDP TCP → TLS Stream
          │
          ▼

       RDP UDP Disable
```

最终原则：

> 能 P2P 就不走 Relay；必须 Relay 时优先 QUIC；UDP 被网络封锁后才退化为 TCP/TLS。

---

# 3. TCP 和 UDP 独立选路

不能简单把一个 RDP Connection 当成：

```text
P2P
或
Relay
```

因为实际网络中很可能出现：

```text
TCP P2P 成功
UDP P2P 失败
```

因此必须分别维护：

```text
TCP Path
UDP Path
```

TCP 优先级：

```text
P2P TCP
   ↓
QUIC Stream Relay
   ↓
TLS/TCP Relay
```

UDP 优先级：

```text
P2P UDP
   ↓
QUIC Datagram Relay
   ↓
Disable RDP UDP
```

从而允许：

| 模式 | RDP TCP | RDP UDP |
|---|---|---|
| Direct | P2P | P2P |
| Hybrid A | P2P | QUIC Relay |
| Hybrid B | QUIC Relay | P2P |
| QUIC Relay | QUIC Stream | QUIC Datagram |
| TCP Fallback | TLS/TCP | Disabled |

最佳模式：

```text
TCP → P2P
UDP → P2P
```

最差网络环境：

```text
TCP → TLS Relay
UDP → Disabled
```

Windows RDP 自身继续通过 TCP 工作。

---

# 4. 系统总体架构

```text
                     Public Server
              ┌────────────────────────┐
              │       RDPulse          │
              │                        │
              │   Rendezvous Server    │
              │   Signaling Server     │
              │   QUIC Relay           │
              │   TCP/TLS Relay        │
              └───────────┬────────────┘
                          │
                   Signaling / Relay
                          │
          ┌───────────────┴───────────────┐
          │                               │
          ▼                               ▼
┌────────────────────┐          ┌────────────────────┐
│ Controller Agent   │          │ Controlled Agent   │
│                    │          │                    │
│ Windows            │          │ Windows            │
│ Local RDP Proxy    │          │ RDP Proxy          │
│ NAT Traversal      │          │ NAT Traversal      │
│ Path Manager       │          │ Path Manager       │
└─────────┬──────────┘          └─────────┬──────────┘
          │                               │
          │        P2P UDP / TCP          │
          └───────────────────────────────┘
                                          │
                                          ▼
                                  127.0.0.1:3389
                                          │
                                          ▼
                                   Windows RDP
```

---

# 5. 系统组件

最终包含三个逻辑角色。

## RDPulse Server

部署：

```text
公网 Linux
```

负责：

```text
设备注册

设备在线状态

Rendezvous

NAT 探测

Candidate 收集

UDP Hole Punch 协调

TCP Hole Punch 协调

Session Signaling

Relay 分配

QUIC Relay

TLS/TCP Relay

认证

ACL

Metrics
```

V1 可以作为一个程序：

```text
rdpulse-server
```

内部模块化。

后续规模扩大后可以拆：

```text
rdpulse-rendezvous
rdpulse-relay
```

这与 RustDesk 将 `hbbs` 和 `hbbr` 分成 Rendezvous 与 Relay 两类服务的思想类似。

---

## Controller Agent

运行在：

```text
发起 RDP 的 Windows
```

程序：

```text
rdpulse.exe
```

负责：

```text
本地 RDP Proxy

设备选择

Rendezvous

P2P Hole Punch

路径选择

QUIC Relay

TLS Relay
```

---

## Controlled Agent

运行在：

```text
被控 Windows
```

作为：

```text
Windows Service
```

负责：

```text
保持在线

注册公网 Candidate

参与 Hole Punch

接收 P2P RDP

接收 Relay RDP

转发 TCP/UDP 到 localhost:3389
```

---

# 6. 为什么控制端也必须安装 Agent

如果用户直接：

```text
mstsc 公网IP:20001
```

标准 mstsc 并不知道：

```text
Rendezvous
STUN
NAT Type
Hole Punching
P2P Candidate
QUIC Relay
```

因此无法进行真正的：

```text
P2P
```

如果需要 P2P，必须变成：

```text
mstsc
  │
  ▼
127.0.0.1:13389
  │
  ▼
Controller Agent
  │
  ├── P2P
  │
  └── Relay
```

---

# 7. 两种使用模式

RDPulse 最终可以同时提供两种模式。

## Enhanced Mode

推荐模式。

控制端安装 Agent：

```text
mstsc 127.0.0.1:13389
```

实际：

```text
mstsc
 ↓
Controller Agent
 ↓
自动选路
 ↓
Controlled Agent
 ↓
3389
```

支持：

```text
P2P
QUIC
TCP fallback
```

---

## Compatibility Mode

控制端不安装 Agent：

```text
mstsc 公网IP:20001
```

这时候无法 P2P。

只能：

```text
mstsc
 ↓
公网 Relay
 ↓
Controlled Agent
```

主要用于兼容传统使用方式。

---

# 8. RustDesk 参考架构

RustDesk 的自托管架构主要包括：

```text
hbbs
=
ID / Rendezvous / Signaling

hbbr
=
Relay
```

客户端持续向 ID Server 注册自己的地址信息。

当 A 连接 B：

```text
A
 ↓
hbbs
 ↓
查找 B
 ↓
Hole Punching
```

成功：

```text
A ←──────── P2P ────────→ B
```

失败：

```text
A
 ↓
hbbr
 ↓
B
```

RustDesk 官方当前文档仍明确说明这一连接模式。

RustDesk Server 的 TCP `21116` 也用于 TCP Hole Punching / Connection Service，而 UDP `21116` 用于 ID 注册和心跳。

RDPulse 采用相同思想，但协议和数据面针对 RDP 重做。

---

# 9. 控制面

每台 Agent 启动后连接：

```text
RDPulse Server
```

建立长期：

```text
TLS Control Connection
```

或：

```text
QUIC Control Stream
```

负责：

```text
REGISTER

HEARTBEAT

CONNECT_REQUEST

CONNECT_NOTIFY

CANDIDATE

PUNCH_START

PUNCH_RESULT

RELAY_ALLOCATE

SESSION_CLOSE

PATH_CHANGE
```

控制面必须：

```text
可靠
有序
加密
认证
```

不能使用 unreliable Datagram。

---

# 10. Device Identity

每台设备拥有：

```text
DeviceID
DeviceSecret
```

例如：

```text
DeviceID:

734 329 102
```

或者：

```text
PC-A8F71C
```

设备首次安装：

```text
生成 DeviceID
生成 256-bit Secret
```

Relay 数据库只保存：

```text
Secret Hash
```

不保存明文 Secret。

---

# 11. Agent 在线注册

Agent 启动：

```text
Agent
 ↓
Server
```

发送：

```json
{
    "type": "REGISTER",
    "device_id": "734329102",
    "hostname": "PC-OFFICE",
    "version": "1.0.0"
}
```

Server 保存：

```text
DeviceID
Control Connection
Public IP
LastSeen
NAT Information
Capabilities
```

---

# 12. Candidate 设计

不能假定 TCP 和 UDP 的公网 IP 相同。

每个 Candidate 必须保存完整：

```text
IP
Port
Protocol
Type
Priority
```

数据结构：

```go
type Candidate struct {
    Protocol Protocol

    Type CandidateType

    Address netip.AddrPort

    Priority uint32
}
```

例如：

```text
UDP Candidate

192.168.1.20:45000
203.0.113.8:35124


TCP Candidate

192.168.1.20:45001
198.51.100.5:46321
```

不要设计成：

```text
公网 IP
+
UDP Port
+
TCP Port
```

因为复杂 NAT/CGNAT 环境下不同协议可能走不同公网出口。

---

# 13. NAT 探测

Agent 注册后进行：

```text
UDP Mapping Test
TCP Mapping Test
```

记录：

```text
Local Candidate
Server Reflexive Candidate
```

V1 不需要实现完整 ICE。

只需要：

```text
LAN Candidate
Public Candidate
```

即可。

后续可以加入：

```text
IPv6 Candidate
多个网卡 Candidate
多个公网 Candidate
```

---

# 14. 局域网优先

如果两台设备：

```text
Public IP 相同
```

或者 Server 判断：

```text
可能位于同一个 LAN
```

优先交换：

```text
Local Candidate
```

例如：

```text
Controller
192.168.1.20

Controlled
192.168.1.50
```

直接：

```text
192.168.1.20
   ↕
192.168.1.50
```

避免流量绕公网。

顺序可以理解为：

```text
LAN Direct
 ↓
WAN P2P
 ↓
Relay
```

---

# 15. UDP P2P Hole Punch

Controller 请求：

```text
CONNECT Device B
```

Server 通知双方：

```text
SessionID
Peer Candidate
Session Token
Punch Start Time
```

例如：

```text
Controller public:
203.0.113.20:45121

Controlled public:
198.51.100.30:57218
```

双方同时：

```text
UDP SendTo(peer)
```

---

# 16. UDP Punch Packet

必须与 RDP DATA 区分。

例如：

```text
Magic       uint32
Version     uint8
Type        uint8
SessionID   uint64
Nonce       uint64
MAC         16 bytes
```

Type：

```text
PUNCH
PUNCH_ACK
KEEPALIVE
```

只有验证：

```text
SessionID
+
MAC
```

成功后才接受对端。

禁止仅凭：

```text
Source IP
```

认定为合法 Peer。

---

# 17. UDP P2P 成功

连接变为：

```text
Controller Agent
       │
       │ UDP Direct
       │
       ▼
Controlled Agent
       │
       ▼
UDP :3389
```

这时候：

```text
Relay 不参与数据传输
```

RDP UDP：

```text
RDP UDP
 ↓
UDP Tunnel
 ↓
RDP UDP
```

不需要：

```text
QUIC Datagram
```

因为链路本身已经是 UDP P2P。

---

# 18. P2P UDP 数据协议

P2P UDP 仍然需要最小 Session Header：

```text
Version      uint8

Type         uint8

SessionID    uint32

PacketID     uint32

FragID       uint8

FragCount    uint8

Payload      []byte
```

固定：

```text
12 Bytes
```

与 QUIC Relay 的 UDP Payload 结构尽可能共用。

这样：

```text
P2P
和
Relay
```

可以共享：

```text
fragmentation
reassembly
session manager
```

---

# 19. P2P UDP Keepalive

NAT 映射需要维护。

默认：

```text
15 秒
```

发送：

```text
KEEPALIVE
```

实际值应该允许根据 NAT 类型调整：

```text
10-30 秒
```

Keepalive 包必须非常小。

---

# 20. TCP P2P Hole Punch

UDP Hole Punch 尝试的同时，可以进行：

```text
TCP Hole Punch
```

Server 向两端提供：

```text
TCP Candidate
```

双方尝试：

```text
Simultaneous Open
```

即：

```text
A connect B
同时
B connect A
```

RustDesk Server 当前也明确保留 TCP Hole Punching 服务。

TCP P2P 成功：

```text
mstsc TCP
 ↓
Controller Agent
 ↓
P2P TCP
 ↓
Controlled Agent
 ↓
3389
```

---

# 21. P2P TCP 安全

TCP P2P 建立后不能直接：

```text
裸 TCP
```

建议建立：

```text
TLS 1.3
```

或者使用 Session Token 完成：

```text
Peer Authentication
```

避免攻击者利用 NAT mapping 注入连接。

---

# 22. P2P 与 Relay 并行竞争

不能采用：

```text
UDP P2P 等 10 秒

↓

TCP P2P 等 10 秒

↓

QUIC Relay
```

否则连接体验非常差。

采用：

```text
Happy-Eyeballs 类模型
```

例如：

```text
T = 0ms

UDP P2P ─────────────────►

TCP P2P ─────────────────►


T = 300ms

QUIC Relay ──────────────►


T = 1000ms

TLS Relay ───────────────►
```

谁先可用：

```text
优先建立连接
```

同时继续观察是否出现更优路径。

---

# 23. Path Manager

Controller Agent 内部必须存在：

```text
PathManager
```

维护：

```go
type PathType uint8

const (
    PathDirectUDP PathType = iota

    PathDirectTCP

    PathQUICRelay

    PathTLSRelay
)
```

TCP 当前路径：

```go
TCPPath
```

UDP 当前路径：

```go
UDPPath
```

两者互不强制绑定。

---

# 24. 路径评分

每个 Candidate Path 可以计算：

```text
Priority
RTT
Loss
Jitter
Availability
```

基本优先级：

```text
LAN Direct       1000

UDP P2P           900

TCP P2P           800

QUIC Relay        500

TLS Relay         100
```

但不能仅看 Priority。

例如：

```text
P2P RTT 250ms
Relay RTT 50ms
```

未来可以允许：

```text
Relay 优于 P2P
```

V1 暂时以：

```text
Direct > Relay
```

为主。

---

# 25. QUIC Relay

P2P 不可用时进入：

```text
QUIC Relay
```

Controller：

```text
Controller Agent
        │
        │ QUIC
        ▼
     Relay
        │
        │ QUIC
        ▼
Controlled Agent
```

双方都维护：

```text
QUIC Connection
```

到 Relay。

---

# 26. RDP TCP over QUIC Relay

TCP：

```text
mstsc
 ↓
Controller local TCP
 ↓
QUIC Stream
 ↓
Relay
 ↓
QUIC Stream
 ↓
Controlled Agent
 ↓
TCP :3389
```

一条：

```text
RDP TCP Connection
```

对应：

```text
一条 Relay Logical Stream
```

---

# 27. RDP UDP over QUIC Relay

UDP：

```text
mstsc
 ↓
Controller UDP
 ↓
QUIC Datagram
 ↓
Relay
 ↓
QUIC Datagram
 ↓
Controlled Agent
 ↓
UDP :3389
```

QUIC DATAGRAM 基于 RFC 9221，是：

```text
encrypted
congestion-controlled
unreliable
not retransmitted
```

因此适合 RDP UDP。

---

# 28. Hysteria2 风格 UDP

QUIC Relay UDP 采用：

```text
SessionID

PacketID

FragmentID

FragmentCount

Payload
```

核心原则：

```text
不 ACK

不应用层重传

不等待旧 UDP Packet

丢失 fragment
→
丢弃整个 Packet
```

这样避免：

```text
UDP over reliable stream
```

导致的 Head-of-Line Blocking。

---

# 29. UDP Datagram Header

统一格式：

```text
Offset   Size   Field

0        1      Version

1        1      Type

2        4      SessionID

6        4      PacketID

10       1      FragmentID

11       1      FragmentCount

12       N      Payload
```

固定 Header：

```text
12 Bytes
```

网络字节序：

```text
Big Endian
```

---

# 30. UDP Fragment

默认：

```text
MAX_PAYLOAD = 1150 Bytes
```

例如 RDP UDP：

```text
2600 Bytes
```

拆为：

```text
PacketID 10086

Fragment 0:
1150

Fragment 1:
1150

Fragment 2:
300
```

接收端只有全部到齐：

```text
0
1
2
```

才交给：

```text
RDP UDP Socket
```

否则：

```text
DROP
```

---

# 31. Fragment Timeout

默认：

```text
100ms
```

允许：

```text
50ms - 300ms
```

例如：

```text
0 ✓

1 ×

2 ✓
```

超过：

```text
100ms
```

立即删除：

```text
Packet 10086
```

不要重传 Fragment。

---

# 32. UDP Session 模型

每个 RDP UDP Client Endpoint：

```text
对应一个 Session
```

例如：

```text
Controller:

127.0.0.1:53231
```

对应：

```text
SessionID 1001
```

Controlled Agent：

```text
Session 1001
 ↓
独立 UDP socket
 ↓
127.0.0.1:3389
```

禁止：

```text
所有 Session
共用一个 Local UDP Source Socket
```

---

# 33. UDP Session Lazy Open

首包：

```text
UDP_DATA
SessionID = 1001
```

如果 Agent：

```text
Session 1001 不存在
```

则：

```text
Create UDP Socket
 ↓
Create Session
 ↓
立即 Write Payload
```

不要：

```text
UDP_OPEN
 ↓
ACK
 ↓
UDP_DATA
```

避免额外 RTT。

---

# 34. UDP Session Timeout

默认：

```text
60 秒
```

无收发：

```text
Session Cleanup
```

采用：

```text
统一 Sweeper
```

例如：

```text
每 10 秒扫描一次
```

不要：

```text
每 Session 一个 Timer
```

---

# 35. TCP/TLS Relay

如果：

```text
UDP 443 被阻止
```

意味着：

```text
QUIC 不可用
```

则进入最终：

```text
TLS Relay
```

路径：

```text
mstsc
 ↓
Controller Agent
 ↓
TLS/TCP 443
 ↓
Relay
 ↓
TLS/TCP
 ↓
Controlled Agent
 ↓
TCP 3389
```

---

# 36. TCP/TLS 模式下禁止 UDP-over-TCP

不能：

```text
RDP UDP
 ↓
TCP/TLS Tunnel
```

因为：

```text
UDP
 ↓
TCP reliability
 ↓
HOL blocking
```

反而破坏体验。

因此：

```text
QUIC unavailable
```

后：

```text
RDP UDP Path = Disabled
```

Windows RDP 自身自动依赖 TCP Transport。

---

# 37. Relay 两级设计

Relay 内部：

```text
RelayManager
 │
 ├── QUIC Relay
 │
 └── TLS Relay
```

QUIC：

```text
UDP 443
```

TLS：

```text
TCP 443
```

由于：

```text
TCP 443
和
UDP 443
```

可以同时监听，所以公网服务器只需要一个非常友好的主要服务端口：

```text
443
```

---

# 38. Rendezvous 端口

为了简化第一版实现，建议：

```text
TCP 443
    Control / TLS Relay

UDP 443
    QUIC Relay

UDP 21116
    Rendezvous / NAT Test / Punch Coordination
```

这样：

```text
QUIC
```

与：

```text
自定义 Rendezvous UDP
```

不需要共享同一个 socket。

RustDesk 目前类似地使用 TCP/UDP `21116` 承担注册、心跳、NAT 和连接相关服务。

后续再考虑统一 UDP 443。

---

# 39. Server 端口规划

V1 推荐：

```text
TCP 443
    Control
    TLS Relay

UDP 443
    QUIC Relay

UDP 21116
    NAT Discovery
    Rendezvous
    Hole Punch

TCP 21116
    TCP Hole Punch Coordination
```

如果使用 Compatibility Mode：

```text
TCP 20000-39999

UDP 20000-39999
```

用于直接：

```text
公网IP:端口
```

RDP。

---

# 40. P2P Session 建立流程

完整流程：

```text
Controller
    │
    │ CONNECT B
    ▼
Server
    │
    ├── Check B Online
    │
    ├── Create Session
    │
    ├── Generate Token
    │
    └── Exchange Candidate
            │
            ▼
     Controller + B
            │
            ├── UDP Punch
            │
            ├── TCP Punch
            │
            └── Relay Prepare
```

最后：

```text
PathManager
```

决定实际路径。

---

# 41. 连接建立优化

建议使用：

```text
Relay Warm Standby
```

即：

P2P 正在尝试的时候：

```text
QUIC Relay
```

也可以提前完成：

```text
Authentication
Session Allocation
```

但暂不传业务流量。

如果：

```text
P2P failed
```

Relay 几乎可以立即接管。

---

# 42. Path Upgrade

例如：

```text
T=0

QUIC Relay 成功

↓

RDP 开始
```

随后：

```text
T=700ms

UDP P2P 成功
```

可以：

```text
UDP Path
QUIC Relay
     ↓
P2P UDP
```

因为 UDP 没有 stream state，路径迁移较容易。

---

# 43. TCP Path 不做 V1 无缝迁移

如果 TCP 已经：

```text
QUIC Relay
```

建立 RDP TCP Connection。

后来：

```text
TCP P2P
```

成功。

V1：

```text
不迁移已有 TCP
```

原因：

```text
TCP sequence
QUIC Stream state
buffer
half-close
```

迁移复杂。

只对：

```text
下一次 TCP Connection
```

使用更好的路径。

---

# 44. Path Failover

UDP：

```text
P2P UDP
 ↓
断开
 ↓
QUIC Datagram
```

允许快速切换。

TCP：

```text
P2P TCP
 ↓
断开
```

现有 RDP TCP：

```text
断开
```

RDP 自身可以进行重连。

V1 不尝试透明 TCP Session Resume。

---

# 45. P2P 加密与认证

不能因为 RDP 自己存在安全层，就完全信任网络。

Hole Punch 建立后至少必须验证：

```text
Session Token
```

建议 Session：

```text
128/256-bit random token
```

仅：

```text
Controller
Controlled
```

能够获得。

UDP Punch 和 Data Header 应包含：

```text
Session Identity
+
Authentication Tag
```

---

# 46. QUIC Relay 安全

Agent → Relay：

```text
QUIC
 ↓
TLS 1.3
```

QUIC Datagram 同样：

```text
encrypted
```

且受 QUIC congestion controller 管理，但丢失后不会重传。

---

# 47. TCP Relay 安全

Fallback：

```text
TLS 1.3
```

Agent 必须：

```text
验证 Server Certificate
```

生产环境禁止：

```go
InsecureSkipVerify: true
```

---

# 48. QUIC 技术实现

V1：

```text
github.com/quic-go/quic-go
```

quic-go 是当前成熟的 Go QUIC 实现。

但是必须注意：

目前 quic-go 官方仍明确说明：

```text
SendDatagram / ReceiveDatagram
```

路径尚未达到 Stream 相同的吞吐优化水平。

所以：

> QUIC implementation 不能与上层业务强绑定。

---

# 49. Transport 抽象

定义：

```go
type Transport interface {

    OpenStream(
        ctx context.Context,
    ) (Stream, error)

    AcceptStream(
        ctx context.Context,
    ) (Stream, error)

    SendDatagram(
        data []byte,
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

以后可以替换：

```text
MsQuic

Quinn

自定义 UDP Transport
```

---

# 50. quic-go 拥塞控制

V1 不宣称支持：

```text
BBR
Brutal
```

当前 quic-go 实现 RFC 9002 的 congestion control；可插拔拥塞控制仍属于后续工作。

因此第一版：

```text
使用 quic-go 默认 CC
```

性能重点优化：

```text
P2P

减少 Relay

UDP Datagram

MTU

Socket Buffer

Allocation

GSO
```

---

# 51. QUIC GSO

Linux Relay 可以利用：

```text
UDP GSO
```

quic-go 当前支持 Linux GSO，可显著减少高吞吐情况下的 syscall 开销。

因此 Relay 推荐：

```text
Linux Kernel >= 4.18
```

并避免无意义包装：

```text
*net.UDPConn
```

否则可能导致 quic-go 无法利用部分内核优化。

---

# 52. UDP Buffer

Relay 建议：

```bash
sysctl -w net.core.rmem_max=16777216
sysctl -w net.core.wmem_max=16777216
```

后续根据：

```text
PPS
带宽
内存
```

压测调整。

quic-go 官方也指出 UDP buffer 太小可能导致高带宽 QUIC 传输受到限制。

---

# 53. Windows Agent 本地结构

Controlled：

```text
C:\Program Files\RDPulse\
    rdpulse.exe
```

配置：

```text
C:\ProgramData\RDPulse\
    config.yaml
    rdpulse.log
```

Service：

```text
RDPulse
```

启动方式：

```text
Automatic
```

---

# 54. Agent 运行模式

支持：

```text
rdpulse.exe install

rdpulse.exe uninstall

rdpulse.exe start

rdpulse.exe stop

rdpulse.exe status

rdpulse.exe run
```

其中：

```text
run
```

用于前台调试。

---

# 55. Controller 本地 RDP Proxy

Controller：

```text
TCP 127.0.0.1:13389
UDP 127.0.0.1:13389
```

用户：

```text
mstsc 127.0.0.1:13389
```

如果应用提供 UI，则：

```text
双击 Device
```

自动执行：

```text
mstsc.exe /v:127.0.0.1:13389
```

用户无需了解底层：

```text
P2P
QUIC
Relay
```

---

# 56. 多远程会话

如果同时连接多台 PC：

```text
PC-A → localhost:13001

PC-B → localhost:13002

PC-C → localhost:13003
```

Controller Port Manager 动态分配。

例如：

```text
Device A
127.0.0.1:13001

Device B
127.0.0.1:13002
```

TCP 与 UDP 使用同一个本地端口。

---

# 57. RDP Health Check

Controlled Agent 定期检查：

```text
TCP 127.0.0.1:3389
```

状态：

```text
Agent Offline

Agent Online / RDP Offline

Agent Online / RDP Online
```

不要因为：

```text
3389 unavailable
```

断开 Server Control Connection。

---

# 58. UDP 被完全封锁

如果企业网络：

```text
UDP Internet
全部禁止
```

结果：

```text
UDP P2P ×

QUIC ×
```

系统：

```text
TCP P2P
 ↓
如果失败
 ↓
TLS Relay
```

最终：

```text
RDP UDP Disable
```

仍然可以正常：

```text
RDP TCP
```

这是系统必须保证的最终兜底。

---

# 59. Agent 自动重连

Control Server 断线：

```text
1s

2s

4s

8s

15s

30s

30s...
```

加：

```text
±20% jitter
```

避免服务器恢复后：

```text
大量 Agent 同时重连
```

---

# 60. Server 项目结构

推荐：

```text
rdpulse/
│
├── cmd/
│   ├── server/
│   └── client/
│
├── internal/
│
│   ├── protocol/
│
│   ├── signaling/
│
│   ├── rendezvous/
│
│   ├── nat/
│
│   ├── punch/
│   │   ├── udp.go
│   │   └── tcp.go
│
│   ├── path/
│
│   ├── transport/
│   │   ├── transport.go
│   │   ├── directudp/
│   │   ├── directtcp/
│   │   ├── quicgo/
│   │   └── tls/
│
│   ├── relay/
│
│   ├── tcp/
│
│   ├── udp/
│   │   ├── session.go
│   │   ├── packet.go
│   │   ├── fragment.go
│   │   └── reassembly.go
│
│   ├── rdp/
│
│   ├── service/
│
│   ├── storage/
│
│   └── metrics/
│
└── go.mod
```

---

# 61. 数据库

V1：

```text
SQLite WAL
```

Agent：

```text
device_id
hostname
secret_hash
enabled
created_at
last_seen
```

Session Log：

```text
session_id

controller_id

controlled_id

tcp_path

udp_path

start_time

end_time

tx_bytes

rx_bytes
```

---

# 62. Metrics

关键指标：

```text
agents_online

connections_active

path_direct_udp

path_direct_tcp

path_quic_relay

path_tls_relay

p2p_udp_success_total

p2p_udp_failed_total

p2p_tcp_success_total

p2p_tcp_failed_total

quic_sessions

tls_sessions

udp_packets

udp_fragment_timeout

udp_loss

path_switch_total

rtt
```

---

# 63. 最重要的观测指标

管理界面显示：

```text
Connection:

TCP:
P2P

UDP:
P2P

RTT:
28 ms

UDP Loss:
0.3%

Relay:
No
```

或者：

```text
TCP:
QUIC Relay

UDP:
QUIC Relay

RTT:
83 ms

Relay:
US-West-1
```

或者：

```text
TCP:
TLS Relay

UDP:
Disabled

Reason:
UDP unavailable
```

这样非常利于故障诊断。

---

# 64. P2P 成功率

Server 必须统计：

```text
UDP Punch Success Rate

TCP Punch Success Rate

Relay Rate
```

例如：

```text
UDP Direct:
72%

TCP Direct:
12%

Relay:
16%
```

这是以后优化系统最重要的数据之一。

---

# 65. 安全限制

Rendezvous 必须限制：

```text
每 IP 注册速率

每 Device Connect Rate

Punch Request Rate

失败认证次数
```

Relay：

```text
带宽限制

并发 Session 限制

UDP PPS 限制

Fragment Memory 限制
```

Agent：

```text
SessionID 验证

Token 验证

Source Validation
```

---

# 66. Fragment DoS 防护

限制：

```text
Maximum UDP Packet
64 KiB

Maximum Fragment Count
64

Maximum Reassembly Per Session
32

Maximum Reassembly Lifetime
300ms

Maximum Global Reassembly Memory
固定上限
```

非法：

```text
DROP
```

不返回错误 Datagram。

---

# 67. Compatibility Mode

保留原始需求：

```text
mstsc
 ↓
公网IP:20001
```

Server：

```text
Public TCP 20001
Public UDP 20001
```

转：

```text
QUIC
 ↓
Controlled Agent
```

因此没有安装 Controller Agent 的电脑仍然可以：

```text
公网IP RDP
```

但是：

```text
无法 P2P
```

---

# 68. 推荐产品使用方式

真正推荐的 RDPulse 使用方式：

```text
Controller 安装 RDPulse

↓

输入 Device ID

↓

点击 Connect

↓

RDPulse 自动选择：

LAN
P2P
QUIC
TLS

↓

启动 mstsc
```

这更接近：

```text
RustDesk 用户体验
```

但桌面协议仍然使用：

```text
Microsoft mstsc
```

而不是自己实现屏幕编码和远控协议。

---

# 69. V1 开发阶段

## Phase 1

```text
Device Registration

Control Connection

Controller Local Proxy

Controlled Local Proxy

TCP Relay
```

先跑通：

```text
mstsc
 ↓
Controller
 ↓
Server
 ↓
Controlled
 ↓
3389
```

---

## Phase 2

实现：

```text
QUIC Relay

QUIC Stream

QUIC Datagram

UDP Session
```

验证：

```text
RDP UDP
```

---

## Phase 3

实现：

```text
UDP Rendezvous

Public Candidate

UDP Hole Punch

P2P UDP
```

---

## Phase 4

实现：

```text
TCP Candidate

TCP Hole Punch

P2P TCP
```

---

## Phase 5

实现：

```text
Parallel Race

PathManager

Hybrid Path

QUIC → TLS fallback
```

---

## Phase 6

实现：

```text
ACL

Rate Limit

Metrics

Profiling

UDP Optimization
```

---

# 70. 性能压测

至少测试：

```text
RTT 10ms
Loss 0%

RTT 50ms
Loss 0.5%

RTT 100ms
Loss 1%

RTT 150ms
Loss 3%

RTT 200ms
Loss 5%
```

比较：

```text
Direct LAN

P2P UDP/TCP

QUIC Relay

TLS Relay
```

---

# 71. QUIC Datagram 性能门槛

由于 quic-go 当前 Datagram API 官方仍注明：

```text
not optimized
```

所以必须专项测试：

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

Packet Loss

Latency

Datagram Queue
```

如果 quic-go 成为瓶颈：

```text
不要重写整个 RDPulse
```

只替换：

```text
Transport Implementation
```

---

# 72. 为什么优先 P2P

假设：

```text
Controller 成都

Controlled 成都

Relay 美国
```

全部 Relay：

```text
成都
 ↓
美国
 ↓
成都
```

可能：

```text
300ms+
```

P2P：

```text
成都
 ↓
成都
```

可能：

```text
10-30ms
```

因此 P2P 对 RDP 的价值甚至比：

```text
QUIC 优化
```

更大。

---

# 73. 多 Relay 演进

未来：

```text
Rendezvous
       │
       ├── Relay CN-West
       ├── Relay CN-East
       ├── Relay US-West
       └── Relay SG
```

P2P 失败时根据：

```text
Controller RTT

Controlled RTT

Region
```

选择最佳 Relay。

RustDesk Pro 当前同样支持多个 Relay，并可根据地理位置选择更合适的 Relay。

---

# 74. 最终连接状态机

```text
                     CONNECT
                        │
                        ▼
                   RENDEZVOUS
                        │
              ┌─────────┴──────────┐
              │                    │
              ▼                    ▼
          UDP PUNCH            TCP PUNCH
              │                    │
        ┌─────┴─────┐        ┌─────┴─────┐
        │           │        │           │
      SUCCESS      FAIL    SUCCESS      FAIL
        │           │        │           │
        ▼           │        ▼           │
     P2P UDP        │     P2P TCP        │
                    │                    │
                    └─────────┬──────────┘
                              ▼
                         QUIC RELAY
                              │
                    ┌─────────┴────────┐
                    │                  │
                 SUCCESS             FAIL
                    │                  │
                    ▼                  ▼
              QUIC STREAM         TLS RELAY
              QUIC DATAGRAM            │
                                       ▼
                                 TCP ONLY RDP
```

---

# 75. 实际 TCP Path

```text
              TCP Path

               START
                 │
                 ▼
            LAN Direct
                 │
               FAIL
                 │
                 ▼
             P2P TCP
                 │
               FAIL
                 │
                 ▼
          QUIC Stream Relay
                 │
               FAIL
                 │
                 ▼
            TLS Relay
```

---

# 76. 实际 UDP Path

```text
              UDP Path

               START
                 │
                 ▼
            LAN Direct
                 │
               FAIL
                 │
                 ▼
             P2P UDP
                 │
               FAIL
                 │
                 ▼
        QUIC Datagram Relay
                 │
               FAIL
                 │
                 ▼
             DISABLED
```

这是整个项目最重要的两条路径。

---

# 77. 最终技术选型

```text
Language
    Go


Server OS
    Linux


Client OS
    Windows


Windows Service
    x/sys/windows/svc


Control
    TLS / QUIC Reliable Stream


Rendezvous
    Custom UDP/TCP


P2P UDP
    Custom Datagram Tunnel


P2P TCP
    TCP + Authentication/TLS


QUIC
    quic-go


RDP TCP Relay
    QUIC Stream


RDP UDP Relay
    QUIC Datagram


Fallback
    TLS/TCP


UDP Relay Algorithm
    Hysteria2-inspired


Storage
    SQLite WAL


Metrics
    Prometheus


Profiling
    pprof
```

---

# 78. 最终网络架构

```text
                          RDPulse Server
                ┌────────────────────────────┐
                │                            │
                │ Rendezvous                 │
                │ Signaling                  │
                │ NAT Discovery              │
                │                            │
                │ QUIC Relay                 │
                │ TLS Relay                  │
                │                            │
                └──────────────┬─────────────┘
                               │
                        Relay / Signaling
                               │
            ┌──────────────────┴──────────────────┐
            │                                     │
            ▼                                     ▼
   ┌──────────────────┐                  ┌──────────────────┐
   │ Controller       │                  │ Controlled       │
   │                  │                  │                  │
   │ mstsc            │                  │ RDP :3389        │
   │    │             │                  │       ▲          │
   │    ▼             │                  │       │          │
   │ RDPulse Agent    │                  │ RDPulse Agent    │
   └────────┬─────────┘                  └────────┬─────────┘
            │                                     │
            │                                     │
            └───────── P2P UDP / TCP ─────────────┘
```

正常情况下：

```text
Server
只参与 signaling
```

数据：

```text
Controller
↕
Controlled
```

打洞失败：

```text
Controller
↕
Relay
↕
Controlled
```

---

# 79. 最终架构原则

```text
P2P 永远优先于 Relay。

LAN Direct 优先于公网 P2P。

TCP 与 UDP 必须独立选路。

UDP P2P 不使用 TCP。

RDP UDP Relay 使用 QUIC Datagram。

QUIC Datagram 不做应用层可靠重传。

TCP P2P 失败后使用 QUIC Stream。

QUIC 不可用后使用 TLS/TCP。

TLS Fallback 下关闭 RDP UDP。

一个 UDP Session 对应一个 RDP UDP Socket。

Fragment 丢失直接丢弃原 UDP Packet。

P2P 和 Relay 尽可能并行建立，减少首连等待。

UDP Path 可以动态升级和降级。

V1 不迁移已经建立的 TCP Connection。

Rendezvous 与 Data Plane 分离。

Relay 只应该是兜底，而不是默认数据路径。

QUIC 库必须存在 Transport abstraction。

所有网络优化必须通过真实 RDP + netem 压测验证。
```

---

# 80. 最终项目定义

RDPulse 不再是：

```text
一个 RDP 端口转发工具
```

也不是：

```text
一个简单 QUIC Relay
```

而是：

> **一个借鉴 RustDesk Rendezvous/P2P 架构，并针对 Microsoft RDP TCP + UDP 传输特征进行优化的智能远程连接网络层。**

它的核心可以浓缩成：

```text
                     RDP

             ┌────────┴────────┐
             │                 │
            TCP               UDP
             │                 │
             ▼                 ▼

          P2P TCP           P2P UDP
             │                 │
           FAIL              FAIL
             │                 │
             ▼                 ▼

        QUIC Stream      QUIC Datagram
             │                 │
           FAIL              FAIL
             │                 │
             ▼                 ▼

         TLS Relay          Disabled
             │
             ▼

        RDP TCP Fallback
```

以及：

```text
                  Rendezvous

                /            \
               /              \
              ▼                ▼
        Controller  ← P2P → Controlled
              │                │
              └──── Relay ─────┘
                   fallback
```

该架构作为 RDPulse 后续协议设计、代码开发、测试、部署和性能优化的最终技术基线。
