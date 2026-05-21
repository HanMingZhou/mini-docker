# 共享 namespace 的实现机制
要回答这个问题，需要理解三个 Linux 内核概念，然后看 mini-docker 在哪一行调用了对应的系统调用。

## 一、Linux 内核里"namespace"是什么
每个进程在内核里都有一组指针，指向 7 类 namespace 对象：
进程 PCB (task_struct)
  └── nsproxy
       ├── mnt_ns   (文件系统挂载视图)
       ├── pid_ns   (进程号视图)
       ├── net_ns   (网卡/路由/iptables/端口)
       ├── uts_ns   (hostname/domainname)
       ├── ipc_ns   (SysV IPC、POSIX 消息队列)`
       ├── user_ns  (uid/gid 映射)
       └── cgroup_ns
两个进程指针指向同一个 namespace 对象 ⇒ 它们看到同一份资源，反之看到不同的副本。

每个 namespace 对象在 /proc 里被表示为一个文件：
/proc/<PID>/ns/net  → 一个 inode，代表"net namespace 实例 X"
/proc/<PID>/ns/ipc  → ...
/proc/<PID>/ns/uts  → ...
/proc/<PID>/ns/pid  → ...
/proc/<PID>/ns/mnt  → ...
如果两个进程的 /proc/<pid>/ns/net 指向同一个 inode，它们就在同一个 net namespace 里。
```bash
# 验证：同一个 Pod 里两个容器
$ ls -L /proc/12345/ns/net   # 容器 A
net:[4026532001]
$ ls -L /proc/12346/ns/net   # 容器 B
net:[4026532001]   # ← 数字相同！
```

只有两个动作需要理解：
动作	                Linux 系统调用	        类比
新建一栋房，住进去	        clone() 带 CLONE_NEW* 标志	盖一栋新房，新生儿直接落户
走进一栋已有的房	        setns()	给老房子配钥匙，从外面走进来
容器要么"新建房子住进去"，要么"走进已有的房子"。仅此两种。
Pod 的概念是"一组容器共享网络"。所以谁先把这栋"共享网络房"盖出来呢？—— sandbox（pause 进程）。

RunPodSandbox 时：
  内核执行：clone(CLONE_NEWNET | CLONE_NEWIPC | CLONE_NEWUTS)
  →  内核新建三栋房：网络房、IPC 房、UTS 房
  →  pause 进程住进这三栋房
  →  pause 进程睡死不退出  →  房子永远不被拆
pause 进程的 PID 就是这栋房子的"门牌号"。

Linux 把这个门牌号暴露在 /proc 下：
/proc/<pause-PID>/ns/net   ← 网络房的钥匙（一个文件）
/proc/<pause-PID>/ns/ipc   ← IPC 房的钥匙
/proc/<pause-PID>/ns/uts   ← UTS 房的钥匙
任何人只要拿到这把"钥匙"（打开这个文件），就能走进同一栋房。

动作 A —— 在 clone() 时只盖该盖的房
父进程 mydocker-cri 调 exec.Command(...).Start()，背后是 clone()。
如果什么都不管，clone flags 默认会传 CLONE_NEWPID | CLONE_NEWNS | CLONE_NEWNET | CLONE_NEWIPC | CLONE_NEWUTS，结果五栋房全是新盖的——网络房也是新的，sandbox 那栋共享房就用不上了。

所以我们先把 NET/IPC/UTS 三个 flag 抹掉：
```go
// 计算 clone flags：如果某个 ns 要 join 已有的，就不创建新的
cloneFlags := cfg.Namespaces.CloneFlags()
if cfg.JoinNS != nil {
    for nsType := range cfg.JoinNS {
        cloneFlags = clearCloneFlag(cloneFlags, nsType)
    }
}
 
cmd := exec.Command(initBinary(cfg), "init")
cmd.SysProcAttr = &syscall.SysProcAttr{
    Cloneflags: cloneFlags,
}
```
clearCloneFlag 只是用位运算把对应位清零。clone() 实际只剩 CLONE_NEWPID | CLONE_NEWNS。

调用结果：
子进程出生时，自己有新 PID 房和 Mount 房；网络/IPC/UTS 三栋房沿用了父进程（mydocker-cri）的房——还不是 sandbox 的房，但没盖错房。
动作 B —— 子进程启动后，手动"走进"sandbox 的房
子进程刚出生，立刻去开门：
```go
// joinNamespaces 使用 setns 加入指定的 namespace。
func joinNamespaces(nsMap map[string]string) error {
    // 顺序：net, ipc, uts（pid 不在这里处理，mount 也不——我们自己做 pivot_root）
    order := []string{"net", "ipc", "uts"}
    for _, ns := range order {
        path, ok := nsMap[ns]
        if !ok {
            continue
        }
        fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
        if err != nil {
            return fmt.Errorf("open %s (%s): %w", ns, path, err)
        }
        if err := unix.Setns(fd, nsCloneFlag(ns)); err != nil {
            _ = unix.Close(fd)
            return fmt.Errorf("setns %s: %w", ns, err)
        }
        _ = unix.Close(fd)
    }
    return nil
}
```
每栋房只有两行代码：
```go
fd := open("/proc/<sandbox-PID>/ns/net")   // 拿到网络房的钥匙
setns(fd, CLONE_NEWNET)                    // 用钥匙开门，走进去
```
执行完三次（net、ipc、uts），子进程就搬家到了 sandbox 那三栋房里。它的 PID 房和 Mount 房还是自己刚盖的，没动。

最后 pivot_root 换 rootfs（在自己的 mount 房里随便折腾，不影响别人），然后 execve 变成 nginx。
一张图概括两步
sandbox 启动后：
 
   ┌──────────────────────┐
   │  网络房  (id=4001)   │ ← pause 进程住这里
   │  IPC 房  (id=4002)   │
   │  UTS 房  (id=4003)   │
   └──────────────────────┘
   钥匙在: /proc/<pause-PID>/ns/{net,ipc,uts}
 
 
StartContainer：
 
   动作 A: clone() 时把 NEWNET/NEWIPC/NEWUTS 抹掉
           ─────────────────────────────────►
           子进程出生，自带新 PID 房 + Mount 房
                       net/ipc/uts 暂时跟着父进程
 
   动作 B: 子进程立刻 setns(open(钥匙路径))
           ─────────────────────────────────►
           子进程搬进 sandbox 那三栋房
 
   最终：
        ┌────────────────────────────────┐
        │ 网络房 4001 ─┐                  │
        │ IPC 房 4002 ─┼─ pause、nginx、 │
        │ UTS 房 4003 ─┘   curl 都住这里  │
        ├────────────────────────────────┤
        │ PID 房 X    │ Mount 房 X       │ ← nginx 自己的
        │ PID 房 Y    │ Mount 房 Y       │ ← curl 自己的
        └────────────────────────────────┘
一句话本质
共享 namespace = 让你的进程指针指向已有的 namespace 对象。

实现就两个动作：
clone() 时别新建（清掉 CLONE_NEW* 位）
setns() 走进已有的（打开 /proc/<sandbox-PID>/ns/<type> 拿到 fd 后调 setns）




## 二、Pod 沙箱的角色
回想 sandbox 是怎么创建的

沙箱 = 一个常驻的 pause 进程 + 独立的 network/UTS/IPC namespace。
启动 sandbox 时用 clone(2) 系统调用，传入 CLONE_NEWNET | CLONE_NEWIPC | CLONE_NEWUTS，内核会新建三个 namespace 对象，把 pause 进程指过去。pause 进程一直 sleep 不退出，namespace 对象的引用计数就永远 ≥ 1，不会被销毁。
之后 sandbox 把 pause 的 PID 记下来：
```go
Metadata     Metadata          `json:"metadata"`
State        State             `json:"state"`
PID          int               `json:"pid"`          // pause 进程的宿主机 PID
NetnsPath    string            `json:"netns_path"`   // /proc/<pid>/ns/net，便于业务容器 setns
IP           string            `json:"ip,omitempty"` // CNI 分配的 IPv4，无则空
```
/proc/<sb.PID>/ns/net 这个文件路径就成了"指向沙箱 net namespace 的句柄"。

## 三、StartContainer 怎么让容器进去
代码有两步关键动作：
### 第 1 步：告诉内核 clone 时不要新建这三个 ns
在父进程（mydocker-cri）里：
```go
joinNS := map[string]string{
    "net": fmt.Sprintf("/proc/%d/ns/net", sb.PID),
    "ipc": fmt.Sprintf("/proc/%d/ns/ipc", sb.PID),
    "uts": fmt.Sprintf("/proc/%d/ns/uts", sb.PID),
}
...
Namespaces: namespace.Flags{
    PID:     true,
    Mount:   true,
    Network: true, // will be cleared by JoinNS
    IPC:     true, // will be cleared by JoinNS
    UTS:     true, // will be cleared by JoinNS
},
```
注释 "will be cleared by JoinNS" 是核心机关。来看实现：
```go
// 计算 clone flags：如果某个 ns 要 join 已有的，就不创建新的
cloneFlags := cfg.Namespaces.CloneFlags()
if cfg.JoinNS != nil {
    for nsType := range cfg.JoinNS {
        cloneFlags = clearCloneFlag(cloneFlags, nsType)
    }
}
 
cmd := exec.Command(initBinary(cfg), "init")
cmd.SysProcAttr = &syscall.SysProcAttr{
    Cloneflags: cloneFlags,
}
```
Cloneflags 是 Go 标准库对 Linux clone(2) 的封装。clearCloneFlag 用 Go 的位运算 &^（AND-NOT）把对应 bit 清零：
```go
func clearCloneFlag(flags uintptr, ns string) uintptr {
    switch ns {
    case "net":
        return flags &^ uintptr(syscall.CLONE_NEWNET)
    case "ipc":
        return flags &^ uintptr(syscall.CLONE_NEWIPC)
    case "uts":
        return flags &^ uintptr(syscall.CLONE_NEWUTS)
```
结果：exec.Command(...).Start() 实际触发的 clone 调用，flags 只包含 CLONE_NEWPID | CLONE_NEWNS，没有 CLONE_NEWNET / IPC / UTS。

内核看到这组 flag 后：

给新进程新建 PID 和 mount namespace（独立进程视图、独立文件系统视图）
复制父进程指针给 net/ipc/uts namespace（也就是 mydocker-cri 自己的，还不是 sandbox 的）
所以这一步只是"先不要新建"，还没真的进 sandbox 的 namespace。

### 第 2 步：子进程启动后用 setns(2) 主动加入
子进程（init 进程）启动后立刻调用 initProcess：
```go
// 加入已有 namespace（CRI 场景：加入沙箱的 netns/ipc/uts）
if len(payload.JoinNS) > 0 {
    if err := joinNamespaces(payload.JoinNS); err != nil {
        return fmt.Errorf("join namespaces: %w", err)
    }
}
```
joinNamespaces 是真正干活的地方：
```go
// joinNamespaces 使用 setns 加入指定的 namespace。
func joinNamespaces(nsMap map[string]string) error {
    // 顺序：net, ipc, uts（pid 不在这里处理，mount 也不——我们自己做 pivot_root）
    order := []string{"net", "ipc", "uts"}
    for _, ns := range order {
        path, ok := nsMap[ns]
        if !ok {
            continue
        }
        fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
        if err != nil {
            return fmt.Errorf("open %s (%s): %w", ns, path, err)
        }
        if err := unix.Setns(fd, nsCloneFlag(ns)); err != nil {
            _ = unix.Close(fd)
            return fmt.Errorf("setns %s: %w", ns, err)
        }
        _ = unix.Close(fd)
    }
    return nil
}
```
逐行翻译：
unix.Open("/proc/<sb.PID>/ns/net", O_RDONLY) —— 打开 sandbox 那个 net namespace 文件，拿到一个 fd。
unix.Setns(fd, CLONE_NEWNET) —— 核心系统调用。告诉内核："把当前进程的 net namespace 指针，改为这个 fd 所代表的那一个"。
ipc、uts 同理。
setns(2) 之后，子进程的内核 task_struct 里：
```bash
nsproxy.net_ns 指向 sandbox 的 net_ns ✅
nsproxy.ipc_ns 指向 sandbox 的 ipc_ns ✅
nsproxy.uts_ns 指向 sandbox 的 uts_ns ✅
nsproxy.pid_ns 是 clone 时新建的（独立进程视图） ✅
nsproxy.mnt_ns 是 clone 时新建的（独立文件系统视图） ✅
```
正好对应你的描述：共享 net/ipc/uts，独立 pid/mount。

### 第 3 步：pivot_root + execve
```go
// pivot_root + /proc /sys /dev（extraMounts 已在父进程预挂到 merged）
if err := rootfs.Setup(payload.Rootfs, nil); err != nil {
    return fmt.Errorf("rootfs setup: %w", err)
}
...
if err := syscall.Exec(bin, payload.Cmd, envSlice); err != nil {
```
在自己独立的 mount namespace 里 pivot_root，把根目录换成镜像 rootfs；最后 execve 替换成业务进程（nginx / sh / 你的应用）。

## 四、串起来：一次 StartContainer 的完整时序
mydocker-cri (父)                 init 子进程                     业务进程
─────────────────────────────────────────────────────────────────────────
1. exec.Command(init, "init")
   Cloneflags = CLONE_NEWPID|NS
                 (NET/IPC/UTS 已被 clear)
2. cmd.Start()
   ───── clone(2) ─────────────►   诞生
                                   nsproxy 状态：
                                   - pid_ns: 新建 ✅
                                   - mnt_ns: 新建 ✅
                                   - net_ns: 继承父（cri）❌
                                   - ipc_ns: 继承父（cri）❌
                                   - uts_ns: 继承父（cri）❌
3. 父写 JSON payload(JoinNS)
   到 pipe ─────────────────────► 读到 payload
 
                                4. for ns in [net, ipc, uts]:
                                     fd = open(/proc/<sb.PID>/ns/<ns>)
                                     setns(fd, CLONE_NEW<NS>)
                                   nsproxy 状态：
                                   - net_ns: → sandbox ✅
                                   - ipc_ns: → sandbox ✅
                                   - uts_ns: → sandbox ✅
 
                                5. pivot_root(rootfs)
                                   挂 /proc /sys /dev（独立 mnt_ns 里）
 
                                6. execve(/usr/bin/nginx, ...) ──► nginx
                                   ↑ 内存映像被替换，但 nsproxy 不变
最后业务进程（nginx）继承了 init 设置好的所有 namespace 指针。

## 五、可观测的效果
假设同一个 Pod 里跑两个容器 A（nginx）和 B（curl-sidecar），它们的 pause 进程 PID 是 1000，nginx 是 1001，curl 是 1002：
验证	    命令	                            预期
共享 netns	ls -L /proc/{1000,1001,1002}/ns/net	三个 inode 数字相同
共享 utsns	ls -L /proc/{1000,1001,1002}/ns/uts	三个 inode 数字相同
独立 pidns	ls -L /proc/{1001,1002}/ns/pid	nginx 与 curl 不同
独立 mntns	ls -L /proc/{1001,1002}/ns/mnt	nginx 与 curl 不同
localhost 互通	在 curl 容器里 curl 127.0.0.1:80	命中 nginx
独立 ps	nginx 容器里 ps、curl 容器里 ps	各自只看到自己的进程
独立 rootfs	nginx 容器里 ls / 是 nginx 镜像，curl 容器里是 curl 镜像	互不可见

localhost 互通的内核解释：因为指向同一个 net_ns，意味着同一份网卡列表（同一个 lo）、同一个路由表、同一个 TCP 连接表、同一段 IP（Pod IP）。nginx 把 80 端口绑到 0.0.0.0 时，curl 容器一发 127.0.0.1:80，内核在共享的 lo 设备上把包送给同一个 net_ns 里监听的 socket——也就是 nginx。

## 六、几个容易被忽略的细节
### 1. clone vs setns 为什么要分开
理论上完全可以全用 setns，但 mount namespace 必须早进入（pivot_root 依赖独立的 mnt_ns），而 setns(CLONE_NEWNS) 在 Go 多线程运行时里有大坑（见 pkg/nsenter 的 unshare CLONE_FS 注释）。所以本项目策略是：能在 clone 时新建的就 clone 时建，需要"加入已有"的就 setns。

### 2. 为什么不 join PID namespace？
K8s 默认 Pod 内多容器不共享 PID namespace（避免一个容器看到别人的进程）。PID: true 会让每个容器有自己的 init=1 进程视图。如果 Pod spec 里设置 shareProcessNamespace: true，CRI 层应该把 pid 也加进 joinNS——本项目目前没实现这块。

### 3. 为什么不 join mount namespace？
Mount namespace 必须和镜像 rootfs绑定。每个容器跑不同的镜像，pivot_root 必须在自己独立的 mnt_ns 里做，否则会污染其它容器看到的文件系统。

### 4. CNI 网络是怎么和 sandbox 的 netns 关联的？
CNI 在 RunPodSandbox 阶段调用，作用对象就是 /proc/<pause-pid>/ns/net：CNI 插件在那个 netns 里建 veth pair、配 IP、加路由。等 StartContainer 让业务容器 setns 进同一个 netns 时，网络已经布好了，容器一启动就能直接用 Pod IP 通信。

一句话本质
"共享 namespace" = 多个进程的内核 nsproxy 指针指向同一个 namespace 对象。 
mini-docker 的实现 = clone() 时不要新建（清掉 CLONE_NEW* 位）+ 子进程启动后用 setns(open("/proc/<sb.PID>/ns/<ns>")) 主动加入沙箱的 namespace。