# 容器网络：netns / veth / bridge / iptables

容器网络 = Linux 内核的 network namespace + veth + bridge + iptables 这几个原语的组合。

## 一、Network Namespace 是什么

Linux 把"网络相关的资源"打包成一个叫 network namespace（netns）的东西。每个 netns 里有自己独立的：

- 网卡列表（你的 eth0 / wlan0 在哪个 ns 里）
- 路由表（往哪个 IP 走哪条路）
- iptables 规则
- /proc/net 视图
- 端口号（A ns 占了 80 端口，B ns 还能占）

宿主机本身有一个默认 netns，所有进程默认在这里。

容器只要进入一个独立的 netns，就跟宿主机网络完全隔离——容器看不到宿主机的网卡，也不能直接访问宿主机的 IP。

代码里这一行：

```go
nsFlags := namespace.Default()
if netMode == "host" {
    nsFlags.Network = false
}
```

意思是：默认情况下创建一个新 netns（Network = true → clone() 时带 CLONE_NEWNET），但 host 模式下不创建（共享宿主机 netns）。

## 二、三种网络模式的本质区别

代码里支持这三类：

```go
case "bridge", "host", "none":
```

| 模式   | 容器在哪个 netns                | 网络能力                               | 应用场景                   |
| ------ | ------------------------------- | -------------------------------------- | -------------------------- |
| host   | 宿主机 netns                    | 跟宿主机完全一样，能用所有宿主机的网络 | 性能敏感、需要监听特权端口 |
| none   | 新建一个空 netns                | 只有 loopback，没法上网                | 完全离线/测试              |
| bridge | 新建一个 netns + 接 veth 到网桥 | 通过虚拟网桥转发                       | 默认，最常用               |

### 2.1 host 模式

```bash
mydocker run --network host --image nginx
```

容器进程跑起来直接用宿主机的网卡。容器里 nginx 监听 80 端口，就是宿主机的 80 端口。`hostname -I` 显示宿主机的 IP。完全没隔离。

### 2.2 none 模式

```bash
mydocker run --network none --image alpine -- ip a
```

容器里执行 `ip a` 只能看到 lo（loopback）。`ping 8.8.8.8` 直接 unreachable——它的 netns 里没有任何能上外网的设备。

### 2.3 bridge 模式

bridge 模式最复杂，也是默认模式。容器有自己的 netns，但通过一对虚拟网线连接到一个虚拟交换机，这个交换机再跟宿主机网络打通。

下面专门讲。

## 三、bridge 模式的物理类比

把它想成一个家用路由器：

```text
┌─────────────────── 家庭路由器 ───────────────────┐
│                                                   │
│ LAN 口 1 ──网线── 你的笔记本 192.168.1.10        │
│ LAN 口 2 ──网线── 你的手机   192.168.1.11        │
│ LAN 口 3 ──网线── 你的电视   192.168.1.12        │
│                                                   │
│ WAN 口 ──━━━━━━━━━━━━━━ 外网（运营商）            │
└───────────────────────────────────────────────────┘
```

- 路由器内部 = 一个交换机（让各设备互相通信）
- LAN 口 = 设备接入点
- 每条网线 = 一对物理线缆
- 路由器 IP 192.168.1.1 = 网关，所有设备出网都走它
- WAN 口 = 通过 NAT 把内网 IP 翻译成公网 IP 出去

容器网络就是把这套搬到 Linux 内核里，全部用软件模拟：

| 物理                                     | 虚拟                                                |
| ---------------------------------------- | --------------------------------------------------- |
| 路由器内部交换机                         | bridge（mydocker0，由内核 module 提供）             |
| 一根网线                                 | veth pair（一对虚拟网卡，一头在容器，一头在宿主机） |
| 路由器 IP（网关）                        | bridge 的 IP（如 10.22.0.1）                        |
| 设备 IP（笔记本）                        | 容器 IP（如 10.22.0.2）                             |
| 路由器 NAT                               | iptables MASQUERADE 规则                            |
| 端口转发（路由器把外网 80 → 内网某机器） | iptables DNAT 规则                                  |

## 四、一个具体的 bridge 容器是怎么联网的

跑 `mydocker run --network bridge --image nginx -p 8080:80`，背后发生的事：

### 步骤 1：宿主机上有一个 bridge

第一次跑容器时，CNI 插件会建一个 bridge（如果还没有的话）：

```bash
# 宿主机看
$ ip link show mydocker0
4: mydocker0: <BROADCAST,MULTICAST,UP,LOWER_UP> mtu 1500

$ ip addr show mydocker0
inet 10.22.0.1/16 scope global mydocker0  # bridge 自己的 IP，作为容器网关
```

mydocker0 是个虚拟交换机——不是真的网卡，没接物理网线。它的作用是"让接到它身上的设备能互相通信"。

### 步骤 2：创建一对 veth

veth pair 是 Linux 内核的"虚拟双绞线"——两个网卡设备，从一头进去的数据立刻从另一头出来。

```bash
# 创建一对，名字随意起
$ ip link add veth_host type veth peer name veth_container
```

现在宿主机能看到两个新网卡 veth_host 和 veth_container，它们就是这条虚拟双绞线的两端。

### 步骤 3：一头插在 bridge，一头放进容器

```bash
# 一头连到 bridge（相当于把网线插进路由器）
$ ip link set veth_host master mydocker0
$ ip link set veth_host up

# 另一头扔进容器的 netns
$ ip link set veth_container netns <container_pid>

# 容器内部把它改名 eth0、配 IP、起来
$ nsenter --net=/proc/<pid>/ns/net ip link set veth_container name eth0
$ nsenter --net=/proc/<pid>/ns/net ip addr add 10.22.0.2/16 dev eth0
$ nsenter --net=/proc/<pid>/ns/net ip link set eth0 up

# 容器内部加默认路由（出网走 bridge）
$ nsenter --net=/proc/<pid>/ns/net ip route add default via 10.22.0.1
```

这之后容器的网络情况：

```bash
# 在容器里
$ ip addr
1: lo: <LOOPBACK,UP> ...
inet 127.0.0.1/8 ...
2: eth0@if5: <BROADCAST,UP> ...  # 这是 veth 的容器端
inet 10.22.0.2/16 ...

$ ip route
default via 10.22.0.1 dev eth0   # 出网走 bridge
10.22.0.0/16 dev eth0 ...        # 同 bridge 内的容器直连
```

### 步骤 4：容器能 ping 通别的容器和网关

容器 A（10.22.0.2）ping 容器 B（10.22.0.3）：

1. A 的内核查路由表，发现 10.22.0.3 在 eth0 网段，直接发 ARP 找 B 的 MAC。
2. 数据从 A 的 eth0 发出 → veth pair → 宿主机的 veth_host_A → bridge mydocker0。
3. bridge 看到目标 MAC，从 veth_host_B 发出。
4. veth pair 把数据送到 B 的 eth0。

容器到外网（如 ping 8.8.8.8）：

1. 容器路由表说默认走 10.22.0.1（bridge 的 IP）。
2. 数据从 veth 出来，进 bridge。
3. bridge 把它当成"宿主机收到的本地包"，交给宿主机内核。
4. 宿主机查路由表，发现走 eth0（真实物理网卡）出去。
5. 关键：源 IP 是 10.22.0.2，运营商不认这个内网 IP——这时 iptables 的 MASQUERADE 规则把源 IP 改成宿主机的公网 IP（NAT）。
6. 包发出去，回包按反向路径进来。

### 步骤 5：端口映射 -p 8080:80

宿主机加一条 iptables DNAT 规则：

```bash
iptables -t nat -A PREROUTING -p tcp --dport 8080 \
    -j DNAT --to-destination 10.22.0.2:80
```

意思：所有从外面进来的、目标端口是 8080 的 TCP 包，改写目标地址为 10.22.0.2:80（容器 IP + 容器内端口），然后正常路由（最后通过 bridge → veth → 容器 eth0）。

外面 `curl http://192.168.252.2:8080`：

1. 包到宿主机。
2. iptables PREROUTING 把目标改成 10.22.0.2:80。
3. 路由表查 10.22.0.2 → 走 mydocker0。
4. bridge 转发到对应 veth → 容器 eth0。
5. 容器里的 nginx 监听在 80 → 接受请求。
6. 回包走反向路径，iptables 自动把源地址改回宿主机 IP，对外面透明。

## 五、项目代码的对应

回到 run.go 这段：

```go
if netMode == "bridge" {
    m, err := network.NewManager("", "", nil)
    if err != nil {
        return fmt.Errorf("init network manager: %w", err)
    }
    if !m.Ready() {
        return fmt.Errorf("--network bridge requires CNI config in /etc/cni/net.d/ ...")
    }
    cniMgr = m
    networkSetup = func(netnsPath string) (string, error) {
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        defer cancel()
        return cniMgr.SetupWithPorts(ctx, id, netnsPath, "eth0", nil, ports)
    }
}
```

我们没有手写所有那些 `ip link add` / `ip route add` / `iptables` 命令，而是用 CNI（Container Network Interface）这个标准接口。CNI 是 Kubernetes 主推的网络抽象——插件负责具体怎么建 veth、配 IP，调用方只负责告诉插件"给这个 netns 配网"。

`SetupWithPorts` 内部做了这几件事：

1. 读 `/etc/cni/net.d/*.conflist` 配置（哪个 bridge、IP 段、子网掩码）。
2. 调用 CNI 插件二进制 `/opt/cni/bin/bridge`。
3. 建 veth pair。
4. 一头进容器 netns（netnsPath），一头接到 bridge。
5. 容器内 eth0 配 IP（IPAM 子插件分配，如 host-local）。
6. 加默认路由。
7. 调用 CNI 插件 portmap（如果有 ports）下 iptables DNAT 规则。
8. 返回容器拿到的 IP（如 10.22.0.2）。

所以 `networkSetup` 是一个回调——容器进程已经 clone() 出来、有了自己的 netns，但还没 exec 业务进程之前调用，给这个 netns 配好网。这个时机非常关键：

- 太早（clone 之前）：netns 还不存在。
- 太晚（exec 之后）：业务进程已经在跑，没有网络会立刻失败。

## 六、完整数据流图（bridge + 端口映射）

```text
外面用户 curl http://192.168.252.2:8080
    │
    ▼
┌────────────────────────── 宿主机 ──────────────────────────┐
│                                                            │
│ eth0 (192.168.252.2)                                      │
│    │                                                       │
│    ▼                                                       │
│ iptables PREROUTING（DNAT）                               │
│ 8080 → 10.22.0.2:80                                       │
│    │                                                       │
│    ▼                                                       │
│ 路由表：10.22.0.0/16 → mydocker0                          │
│    │                                                       │
│    ▼                                                       │
│ ╔═══════════════════════════════╗                          │
│ ║ mydocker0 bridge              ║                          │
│ ║ IP: 10.22.0.1（容器网关）     ║                          │
│ ╚═══════════════════════════════╝                          │
│       │ veth_host_A       │ veth_host_B                    │
└───────┼───────────────────┼────────────────────────────────┘
        │ (veth pair)       │ (veth pair)
        │                   │
┌───────▼──────┐      ┌─────▼────────┐
│ 容器 A       │      │ 容器 B       │
│ netns A      │      │ netns B      │
│ eth0:        │      │ eth0:        │
│ 10.22.0.2    │      │ 10.22.0.3    │
│ default →    │      │ default →    │
│ 10.22.0.1    │      │ 10.22.0.1    │
│ nginx :80    │      │ ...          │
└──────────────┘      └──────────────┘
```

## 七、一行检查清单（出问题时怎么看）

```bash
# 1. bridge 在不在
ip link show mydocker0

# 2. 容器有没有拿到 IP
mydocker inspect <id> | grep network_ip

# 3. 容器 netns 里的网络情况
nsenter --net=/proc/<pid>/ns/net ip a
nsenter --net=/proc/<pid>/ns/net ip route

# 4. 容器能不能 ping 网关
nsenter --net=/proc/<pid>/ns/net ping -c1 10.22.0.1

# 5. 容器能不能上外网（NAT 是否生效）
nsenter --net=/proc/<pid>/ns/net ping -c1 8.8.8.8

# 6. iptables DNAT（端口映射）有没有
iptables -t nat -L PREROUTING -n | grep DNAT

# 7. 宿主机 IP 转发有没有开
sysctl net.ipv4.ip_forward  # 应该是 1
```

一句话总结：容器网络 = 把"路由器 + 网线 + NAT"用 Linux 内核的 netns/bridge/veth/iptables 模拟出来。`--network bridge` 给容器一个独立 netns 然后用一对虚拟网线接到虚拟交换机；`--network host` 不建新 netns；`--network none` 建空 netns 啥也没接。我们的代码不直接搞这些原语，而是通过 CNI 插件来执行——插件读配置、建 veth、配 IP、加 iptables 规则，最后返回容器 IP 给我们记录。

## 八、veth pair 是什么

veth pair 是 Linux 内核的"虚拟双绞线"——两个网卡设备，从一头进去的数据立刻从另一头出来。

```bash
# 创建一对，名字随意起
$ ip link add veth_host type veth peer name veth_container
```

### 8.1 veth、eth0、veth_host 是什么关系

先讲"网卡"这个抽象：

网卡（network interface）= Linux 里能收发网络数据的设备，每张网卡：

- 有一个名字（如 eth0、lo、wlan0）
- 可能有一个 MAC 地址
- 可能有一个或多个 IP 地址
- 属于某一个 netns（network namespace）

物理网卡（如 eth0）和虚拟网卡（如 veth、lo、bridge、tun）在 Linux 看来都是网卡，操作命令一样。

veth 永远是成对出现的——一对就是一根虚拟双绞线。它不是单个设备，是两个绑在一起的设备。

```bash
# 一次创建一对
ip link add NAME1 type veth peer name NAME2
```

执行后宿主机出现两个新网卡 NAME1 和 NAME2，它们绑定在一起：从 NAME1 写入的数据会立刻在 NAME2 出现，反之亦然。就像一根网线的两头。

eth0 是 Linux 的约定俗成的第一张以太网卡名字。它没有特殊语义，谁都能叫这个名字——只要在自己的 netns 里唯一就行。

容器里的 eth0 是一张 veth 设备，只是被改名成了 eth0 让容器进程看着舒服（很多程序硬编码 eth0）。

把它三者串起来就是：

```text
┌─────────────────────── 宿主机 netns ──────────────────────┐
│                                                            │
│ 真实物理网卡：eth0 (192.168.252.2)   ← 跟外网通的口        │
│                                                            │
│ 虚拟交换机：mydocker0 (10.22.0.1)    ← bridge             │
│     │                                                      │
│     ├── veth-A-host (随便起的名)      ← veth pair 的一头   │
│     │      │                                               │
│     │      │ （这是一根虚拟网线）                          │
│     │      │                                               │
│     ├── veth-B-host                                        │
│     │      │                                               │
└─────┼──────┼───────────────────────────────────────────────┘
      │      │
      │      │ veth pair 的另一头跑到了容器 netns 里
      ▼      ▼
  ┌──────────────┐    ┌──────────────┐
  │ 容器 A netns │    │ 容器 B netns │
  │              │    │              │
  │ lo           │    │ lo           │
  │ eth0 ←(就是  │    │ eth0         │
  │ veth pair    │    │              │
  │ 的另一头，   │    │              │
  │ 被改名成     │    │              │
  │ eth0)        │    │              │
  │              │    │              │
  │ 10.22.0.2    │    │ 10.22.0.3    │
  └──────────────┘    └──────────────┘
```

关键点：宿主机上看到的 veth-A-host 和容器里的 eth0 其实是同一根 veth pair 的两端。它们跨越了 netns 边界——一头在宿主机 netns 可见，另一头在容器 netns 可见。

可以用 `ip link` 验证这个对应关系：

```bash
# 宿主机
$ ip link show
4: vethABCD@if5: ...  # @if5 表示对端是另一个 ns 里的 ifindex=5

# 进容器
$ nsenter --net=/proc/<pid>/ns/net ip link show
5: eth0@if4: ...      # @if4 对应宿主机那头的 ifindex
```

`@ifN` 这个标记就是告诉你"对端的 ifindex 是多少"，能配对查到。

### 8.2 宿主机上会有很多 veth

跑 N 个 bridge 容器 → 宿主机上有 N 条 veth pair → 宿主机能看到 N 个 veth（每对的另一头都跑到对应容器的 netns 里去了）。

```bash
# 宿主机起了 3 个容器
$ ip link show
1: lo: ...
2: eth0: ...                                      # 物理网卡
3: mydocker0: ...                                 # bridge
4: veth1234@if5: ... master mydocker0 state UP   # 容器 1 的 veth host 端
6: veth5678@if5: ... master mydocker0 state UP   # 容器 2 的 veth host 端
8: vethabcd@if5: ... master mydocker0 state UP   # 容器 3 的 veth host 端
```

注意都标了 `master mydocker0`——表示它们都"插在"同一个 bridge 上。

容器删除时这条 veth pair 会自动消失：

1. 容器 netns 销毁时，里面的网卡（veth 的一端）跟着消失。
2. veth 是成对绑定的，一端消失，另一端自动消失。
3. 所以不需要手动清理 host 端的 veth，CNI plugin 也不用做额外工作。

宿主机的 `ip link` 列表会随容器数量动态增减。Docker 在大规模部署时（几百个容器），宿主机确实会有几百个 veth——但这是正常的，内核能轻松处理。

如果一天没清理过、容器频繁起停，可能看到很多 vethXXX——大概率是孤儿（容器 netns 已经死了但 veth 还没回收）。`ip link delete vethXXX` 手动删就行。

## 九、MASQUERADE 是什么时候改源 IP 的

先讲 MASQUERADE 这条规则本身。CNI bridge 插件初始化时（或者你手动）会下一条：

```bash
iptables -t nat -A POSTROUTING -s 10.22.0.0/16 ! -o mydocker0 -j MASQUERADE
```

读起来：

- `-t nat`：操作 nat 表。
- `-A POSTROUTING`：追加到 POSTROUTING 链（包离开本机前的最后一道）。
- `-s 10.22.0.0/16`：源地址是容器网段。
- `! -o mydocker0`：出去的网卡不是 mydocker0（也就是要出宿主机的包）。
- `-j MASQUERADE`：执行 MASQUERADE 动作（自动把源 IP 改成出口网卡的 IP）。

翻译成大白话：

"如果一个包的源地址是 10.22.0.x（来自某个容器），并且它要从 mydocker0 以外的网卡出去（比如要走 eth0 出宿主机）——把它的源地址改成那张出口网卡的 IP。"

触发时机：包穿过 netfilter 的 POSTROUTING 钩子时。

要明白这个时机，得先看 Linux 网络栈处理一个包的完整流程：

```text
              收到一个包
                  │
                  ▼
         [ PREROUTING ]   ← iptables 第 1 个钩子
                  │
                  │  ┌─ 决策：这个包是给本机的吗？───┐
                  ▼  │                              │
                  ── ┤            是                 │
                     │             ▼                 │
                     │        [ INPUT ]              │
                     │             │                 │
                     │             ▼                 │
                     │      本机进程处理             │
                     │                               │
                     │    否（要转发出去/本机产生的）│
                     │             ▼                 │
                     │       [ FORWARD ]             │
                     │             │                 │
                     ▼             │                 │
                  本机产生         │                 │
                  的包             │                 │
                  [ OUTPUT ]       │                 │
                     │             │                 │
                     ▼             ▼                 │
                  ─────[ POSTROUTING ]──────────────┘  ← iptables 最后一个钩子
                              │
                              ▼
                          出网卡发出
```

MASQUERADE 在 POSTROUTING 这个时机改源 IP——也就是包要离开宿主机的最后一刻。

一个完整例子：容器 ping 8.8.8.8。

第 1 步：包在容器 netns 里产生：

```text
src=10.22.0.2 dst=8.8.8.8
```

容器路由表说默认走 eth0（容器内的 eth0，也就是 veth 的容器端）。

第 2 步：veth 把包送到宿主机端。veth 是双向管道，包瞬间从宿主机的 vethXXX 出来。

第 3 步：进入 bridge mydocker0。所有 vethXXX 都接在 mydocker0 上，bridge 收到这个包。bridge 看 dst MAC，发现不是同 bridge 内任何一个容器的——也不是 bridge 自己的——把包"上交"给宿主机的 IP 层。

第 4 步：宿主机 IP 层路由决策，查宿主机的路由表：

```text
default via 192.168.252.1 dev eth0   ← 默认走真实网卡 eth0
```

决定从 eth0 出去。

第 5 步：进入 POSTROUTING 链（关键时刻）。

```text
包当前是 src=10.22.0.2 dst=8.8.8.8
出口网卡是 eth0（不是 mydocker0）
源地址 10.22.0.2 命中规则 -s 10.22.0.0/16
MASQUERADE 触发：把源 IP 改成 eth0 的 IP，变成 src=192.168.252.2 dst=8.8.8.8
同时记录这个映射到 conntrack 表里：192.168.252.2:某临时端口 ↔ 10.22.0.2:某临时端口
```

第 6 步：包从 eth0 发出。运营商看到的源 IP 是 192.168.252.2（公网认得这个），它会把回包送回宿主机。

第 7 步：回包返回。

```text
8.8.8.8 → 192.168.252.2
进入宿主机 PREROUTING 链
conntrack 看到这个包对应之前那个 NAT 连接，反向改写目标地址：192.168.252.2 → 10.22.0.2
路由查 10.22.0.2 → 走 mydocker0 → 进对应 veth → 容器 eth0
```

容器进程看到回包，全程不知道有 NAT 这回事。

为什么必须做 MASQUERADE：容器 IP（10.22.0.2）是私有 IP——它只在你这台宿主机内的 mydocker0 网段有意义。把这个 IP 发到公网上，运营商根本不认识这个 IP；即使认识，回包也回不来——它不知道 10.22.0.2 在地球上哪里。所以必须用宿主机的真实 IP 当包的源——换张身份证出门。MASQUERADE 就是干这个的。

跟 DNAT 的对比：

| 名称              | 做什么    | 触发时机                | 用在哪                  |
| ----------------- | --------- | ----------------------- | ----------------------- |
| MASQUERADE / SNAT | 改源 IP   | POSTROUTING（出本机前） | 容器出网、上公网        |
| DNAT              | 改目标 IP | PREROUTING（进本机后）  | 端口映射（外面 → 容器） |

两个一起组成"双向 NAT"：

- 进来的：外部:公网IP:8080 → DNAT → 容器:10.22.0.2:80。
- 出去的：容器:10.22.0.2 → SNAT → 外部:宿主机IP。

一句话总结：

| 概念       | 一句话定义                                                                          |
| ---------- | ----------------------------------------------------------------------------------- |
| netns      | 一组独立的网络资源（网卡、路由、iptables）                                          |
| 网卡       | 能收发数据的设备，每张属于某个 netns                                                |
| veth       | 一对绑定的虚拟网卡，从一端进数据立刻从另一端出                                      |
| bridge     | 虚拟交换机，让接到它身上的网卡互通                                                  |
| eth0       | 约定俗成的第一张以太网卡名字，物理虚拟都能叫                                        |
| veth_host  | 我举例时随便起的名字，指 veth pair 留在宿主机的那头                                 |
| MASQUERADE | iptables 在 POSTROUTING 时把出网包的源 IP 改成出口网卡的 IP（容器出网的"换身份证"） |
| DNAT       | iptables 在 PREROUTING 时把进来包的目标 IP 改成容器 IP（端口映射）                  |

容器越多，宿主机的 veth 越多，这是正常现象。容器删除时 veth 自动回收，不需要手动清理。

## 十、为什么 iptables 规则里还要写 ! -o mydocker0

既然 mydocker0 是 Bridge，为什么 iptables 规则里还要写 `! -o mydocker0`（不从它出去）？

这其实是一个“防止误杀”的保护机制。

要想理解这个，我们要先理解 mydocker0 在 Linux 宿主机上的双重身份：

- 在二层（数据链路层）：它是一个虚拟交换机。把所有容器的 veth 网卡连在一起。
- 在三层（网络层）：对于宿主机来说，它同时也是一张普通的网卡（Network Interface），并且配置了容器网段的网关 IP（比如 10.22.0.1）。

现在，我们假设没有 `! -o mydocker0` 这个限制，规则变成只要源 IP 是 10.22.0.x 就做 NAT 转换。我们来看看会发生什么：

场景 A（容器 A 访问外网 8.8.8.8）：包的目的地是公网，路由决定走宿主机的物理网卡 eth0 出去。经过 iptables 时，源 IP 被成功替换成宿主机 IP。这是我们期望的。

场景 B（容器 A 访问同宿主机上的容器 B 10.22.0.3）：包的目的地是同一个网段的容器 B。这个包进入了 mydocker0，然后 mydocker0 发现目标在自己身上，于是把包从 mydocker0 发出去，塞给容器 B 的 veth 端口。

灾难来了：如果没有限制，这个包也会被源地址转换（SNAT）！容器 B 收到的包，源 IP 会变成宿主机网关的 IP（10.22.0.1），而不是容器 A 的真实 IP（10.22.0.2）。这会导致容器间的内网通信丢失真实的源 IP。

结论：

`! -o mydocker0` 的大白话翻译是：“如果这个包只是在咱们容器小区内部串门（最终还是从 mydocker0 这个交换机发给别的容器），那就保留原样；只有当这个包要离开小区，走别的网卡（比如 eth0）去互联网大街上时，才给它换上宿主机的公网身份证。”

## 十一、为什么包从 vethXXX 出来后要先到 mydocker0

包既然已经从宿主机的 vethXXX 出来了，为什么不直接去找宿主机，而是要去找 mydocker0 网桥？

这个问题涉及到了网卡被“奴役（Enslave）”的概念。

在 Linux 中，一旦你把一张网卡（哪怕是 vethXXX）绑定到了一个网桥（Bridge）上，这张网卡就失去了它的“独立自由”，变成了网桥的一个物理端口。

可以用物理机房来打个比方：

- 容器 = 你的独立服务器。
- veth pair = 一根实体的网线。
- vethXXX（宿主机端）= 这根网线插在交换机上的那个水晶头/端口。
- mydocker0 = 一台实体的二层交换机。
- 宿主机的 IP 路由协议栈 = 机房对外的大路由器。

当数据包从容器发出时，流程是这样的：

1. 容器把数据包塞进网线（容器内的 eth0）。
2. 信号通过网线瞬间传到了另一头，也就是宿主机上的 vethXXX。
3. 关键点来了：vethXXX 此时已经插在 mydocker0 这台交换机上了（用 `ip link` 看时，它后面带有 `master mydocker0` 的字样）。
4. 因此，当数据从 vethXXX 这个水晶头冒出来的时候，它直接进入了交换机（mydocker0）的背板，而不是进入宿主机的全局网络。

那 mydocker0 交换机收到包后会怎么做？

- 如果目标 MAC 是另一个容器，交换机直接把包丢进另一个插在它身上的 vethYYY 端口里。（全程不打扰宿主机的大路由器。）
- 如果容器是要上网，它填写的 MAC 地址是默认网关的 MAC 地址（也就是 mydocker0 自己在宿主机上那张网卡的 MAC）。这时候，mydocker0 才会把包“向上”提交给宿主机的 IP 路由协议栈，让宿主机去查路由表，最终转交给 eth0。

结论：

不是包不想直接去找宿主机，而是 vethXXX 已经被编入了 mydocker0 的编制，变成了它的一个接口。从这个接口出来的数据，第一接手人必然是 mydocker0 自身的二层转发逻辑。

## 十二、run.go 里网络相关的完整调用链

在 run.go 里跟网络有关的就这几件事：

1. 解析 flag（`--network`、`-p`）。
2. 决定要不要创建新 netns（`nsFlags.Network = false` 表示 host）。
3. 创建 CNI manager 客户端，注册回调 `networkSetup`。
4. 注册 iptables DNAT 规则（端口映射）。

run.go 自己不直接做 veth、bridge、iptables——这些活全交给：

- CNI 插件二进制（`/opt/cni/bin/bridge`、`/opt/cni/bin/host-local`、`/opt/cni/bin/portmap`）做底层 `ip link` / route / iptables 操作。
- `pkg/network` 包做 CNI 调用封装 + 部分自己写的 iptables 规则。

完整调用链：

```text
mydocker run --network bridge -p 8080:80
        │
        ▼
┌─────────────────────────────────────────────────┐
│ cmd/mydocker/run.go                             │
│ 1. 解析 flag                                    │
│ 2. 创建 network.Manager（读 CNI 配置）          │
│ 3. 注册 networkSetup 回调                       │
│ 4. 调 container.Start(cfg)                      │
└────────────┬────────────────────────────────────┘
             │
             ▼
┌─────────────────────────────────────────────────┐
│ pkg/container/container_linux.go                │
│ 1. clone() 出新进程，带 CLONE_NEWNET（新 netns）│
│ 2. 在合适时机调 cfg.NetworkSetup(netnsPath)     │
│    ↑↑↑ 这里回调 run.go 注册的函数               │
└────────────┬────────────────────────────────────┘
             │
             ▼
┌─────────────────────────────────────────────────┐
│ pkg/network/cni.go - Manager.SetupWithPorts()   │
│ 1. 读 /etc/cni/net.d/*.conflist 配置            │
│ 2. 用 libcni 调用每个插件                       │
└────────────┬────────────────────────────────────┘
             │
             ▼
┌─────────────────────────────────────────────────┐
│ /opt/cni/bin/bridge   ← 真正干活的进程          │
│ 1. 创建 mydocker0 bridge（如果没有）            │
│ 2. 创建 veth pair                              │
│ 3. 一头连 bridge，一头进容器 netns              │
│ 4. 调 IPAM 子插件分配 IP                       │
│ 5. 容器 netns 内：配 IP、加默认路由             │
│ 6. 下 MASQUERADE iptables 规则（容器出网用）   │
└────────────┬────────────────────────────────────┘
             │
             ▼
┌─────────────────────────────────────────────────┐
│ /opt/cni/bin/host-local  ← IPAM 插件            │
│ 1. 从子网池中分配一个 IP（如 10.22.0.5）        │
│ 2. 写入本地数据库 /var/lib/cni/networks/...     │
└─────────────────────────────────────────────────┘
             │
             ▼
┌─────────────────────────────────────────────────┐
│ /opt/cni/bin/portmap   ← 端口映射插件           │
│ 1. 下 iptables DNAT：8080 → 10.22.0.5:80        │
│ 2. 下 iptables MASQUERADE（hairpin 模式）       │
└─────────────────────────────────────────────────┘
```

我们的代码本身只是一个 CNI 调用客户端——把 podID、netns 路径、端口映射打包成 `libcni.RuntimeConf`，调 `m.cni.AddNetworkList()`，剩下全交给 CNI 插件。

### 我们没自己实现的部分（外部插件做了）

下面这些动作都不是我们写的代码做的，是 `/opt/cni/bin/` 下面的插件二进制做的。

bridge 插件（`/opt/cni/bin/bridge`）的工作：

```bash
# 检查 bridge 设备 mydocker0 是否存在，不在就创建
ip link add name mydocker0 type bridge
ip link set mydocker0 up
ip addr add 10.22.0.1/16 dev mydocker0

# 创建 veth pair
ip link add veth-host-XXX type veth peer name eth0

# 把容器端 veth 挪进容器 netns
ip link set eth0 netns /proc/<pid>/ns/net

# host 端连到 bridge
ip link set veth-host-XXX master mydocker0
ip link set veth-host-XXX up

# 在容器 netns 内配 IP 和路由
nsenter --net=... ip link set eth0 up
nsenter --net=... ip addr add 10.22.0.5/16 dev eth0
nsenter --net=... ip route add default via 10.22.0.1

# 在宿主机下 MASQUERADE iptables 规则（容器出网用）
iptables -t nat -A POSTROUTING -s 10.22.0.0/16 ! -o mydocker0 -j MASQUERADE

# 开启转发
sysctl -w net.ipv4.ip_forward=1
```

host-local 插件（`/opt/cni/bin/host-local`）的工作（IPAM）：

1. 从配置的子网中找一个空闲 IP（如 10.22.0.5）。
2. 记到本地数据库 `/var/lib/cni/networks/<network>/<ip>`，避免被重复分配。
3. 容器删除时（CNI DEL）从数据库释放。

portmap 插件（`/opt/cni/bin/portmap`）的工作（端口映射）：

1. 接收 CapabilityArgs 里的 `portMappings: [{HostPort: 8080, ContainerPort: 80, ...}]`。
2. 在 nat 表的 PREROUTING 链下 DNAT：

```bash
iptables -t nat -A CNI-DN-XXX -p tcp --dport 8080 -j DNAT --to-destination 10.22.0.5:80
```

3. 在 OUTPUT 链下同样规则（让本机也能 curl 127.0.0.1:8080）。
4. 在 POSTROUTING 链下 hairpin 模式 MASQUERADE（让容器自己 curl 自己的映射端口能通）。

### 我们项目里跟 iptables 相关的"自己写的部分"

注意 run.go 里这一行：

```go
if len(ports) > 0 && rec.NetworkIP != "" {
    if err := network.AddPortMappings(rec.NetworkIP, id, ports); err != nil {
        ...
    }
}
```

我们自己也写了一份 iptables 规则代码，作为对 portmap CNI 插件的补充（或者备份）。这是因为：

- 不是所有发行版都装了 portmap 插件。
- 即使装了，rule 命名规则不可控，删除时不好定位。

install-cni.sh 28 - 91 这就是 CNI 配置——一个 plugin 链：

1. bridge ← 建 veth/bridge/IP/路由/MASQUERADE
2. portmap ← 端口映射 DNAT
3. firewall ← 默认放行容器流量

每个插件做自己那一份事，按顺序调用。

我们这版的策略是：核心隔离逻辑自己做，底层操作交给 CNI 插件。这跟 K8s/containerd 是同一个套路（containerd 也是调 CNI，不自己拉 veth）。

一句话总结：

run.go 里跟网络相关的代码不是"实现 bridge 网络"——它只是"决定要不要 netns + 注册一个回调，让别人去做 bridge"。真正"做"bridge 的是 CNI 插件二进制（外部进程）。我们包装的只有：

- 解析参数。
- 创建/不创建 netns（namespace flag）。
- 调用 libcni 让它帮我们调插件。
- 端口映射（部分自己写 iptables，部分靠 portmap 插件）。

## 十三、总结图景

把它们串起来，数据流转的完整链路是这样的：

1. 容器内部：数据包从容器内的网卡（eth0）发出。
2. 网线传输：通过 veth pair 这根虚拟网线，瞬间传到宿主机上的对端网卡（veth_host）。
3. 接入网桥：因为 veth_host 已经被奴役（Enslave）给了网桥，所以包直接进入了网桥这个虚拟交换机。
4. 网桥分发：
   - 如果包是给另一个容器的，网桥直接把它塞进另一个 veth_host 网卡（走二层交换）。
   - 如果包是给外网的，网桥通过自己的网卡身份（如 10.22.0.1），把包上报给宿主机路由器，由宿主机转发出去（走三层路由）。
