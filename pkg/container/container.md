# 容器到底是什么

## 一、容器不是虚拟机。它本质就是一个普通的 Linux 进程，但被 Linux 内核用三件事"包"了起来：

Namespace（命名空间）：让这个进程"看不到"宿主机的其他东西。比如它有自己的 PID 1，自己的网卡，自己的主机名、挂载点。
一共 7 种 namespace：pid / net / mnt / uts / ipc / user / cgroup。
Cgroup（控制组）：限制这个进程能用多少 CPU、内存。
Rootfs（根文件系统）：通过 pivot_root 把它的 / 换成另一个目录（比如解压好的 alpine 镜像），让它以为整个文件系统都不一样了。

## 二、这个文件的整体流程（双进程模型）

Docker、runc 都用同一种套路：父进程 + init 子进程。

```text
父进程 (mydocker run)
│
│ 1. os.Pipe() 创建一根管道
│ 2. exec.Command("/proc/self/exe", "init") 启动自己
│ 并设置 Cloneflags = CLONE_NEWPID|NEWNS|NEWNET|...
│ 3. cmd.Start() ← 内核此刻 clone 出子进程，并把它放进新 namespace
│
├──→ 子进程（"init" 子命令）
│ 阻塞在 read(pipe) ← 等父进程发指令
│
│ 4. 父进程：配置 cgroup（把子进程 PID 加进去）
│ 5. 父进程：配置网络（调 CNI，对子进程的 netns 做手脚）
│ 6. 父进程：通过 pipe 把 payload（rootfs/cmd/env...）发给子进程
│
└──→ 子进程被唤醒：
sethostname → rootfs.Setup(pivot_root) → chdir → exec 用户命令
```

因为有些事情必须父进程做（比如往 cgroup 里加 PID、调 CNI 设置网络），但又必须等子进程已经在新 namespace 里之后才能做。
所以子进程要先"卡住"，等父进程在外面把环境准备好，再放它走。这个"卡住"就是靠管道阻塞读实现的。

## 三、逐段讲解

### 1. initPipeFD = 3 和 initPayload

```go
// 父子之间通过一对 pipe 传递 "启动指令"（JSON 编码的 initPayload）。
// FD 3 = pipe 的读端（由父进程通过 ExtraFiles 注入子进程）。
const initPipeFD = 3
```

FD（File Descriptor）：进程里每个打开的文件/管道/socket 都有一个整数编号。0=stdin，1=stdout，2=stderr，从 3 开始就是用户自己的。
父进程通过 cmd.ExtraFiles = []\*os.File{r} 把管道读端塞给子进程，Go 规定它在子进程里就是 FD 3。
initPayload 是 JSON 结构，把"要执行什么命令、用哪个 rootfs"等参数从父传给子。

### 2. start() —— 父进程入口

#### (a) 参数校验 + 起 cgroup 名字（34–51 行）

不重要，跳过。

#### (b) 创建管道、计算 clone flags（54–66 行）

```go
cloneFlags := cfg.Namespaces.CloneFlags()
if cfg.JoinNS != nil {
   for nsType := range cfg.JoinNS {
      cloneFlags = clearCloneFlag(cloneFlags, nsType)
   }
}
```

CloneFlags() 返回一堆 CLONE_NEWPID | CLONE_NEWNS | ...，告诉内核 "fork 时给我建一套新 namespace"。
但有些 namespace 不想新建，而要"加入"已有的——这就是 K8s Pod 的情景：一个 Pod 里多个容器共享同一个 netns。
所以如果 JoinNS 里指定了 net，就要把 CLONE_NEWNET 从 flags 里去掉，否则会建一个新的网络空间，而不是加入沙箱的。

#### (c) 启动 init 子进程（68–72 行）

```go
cmd := exec.Command(initBinary(cfg), "init")
cmd.SysProcAttr = &syscall.SysProcAttr{
   Cloneflags: cloneFlags,
}
cmd.ExtraFiles = []*os.File{r}
```

initBinary(cfg) 默认是 /proc/self/exe，即当前这个 mydocker 二进制自己。再传参 "init"，子进程启动后会走到 initProcess() 函数。
Cloneflags 是关键：Go 的 os/exec 把它转成 clone(2) 系统调用的 flag，内核在创建子进程的瞬间就把它扔进新 namespace 了。
ExtraFiles 让子进程继承管道读端为 FD 3。

#### (d) stdio 重定向（74–113 行）

detach 模式：日志写入文件；前台 tty 模式：连到当前终端。逻辑直白。

#### (e) cmd.Start() —— 触发 fork（115 行）

这一刻，内核 clone() 出子进程，子进程立刻在新 namespace 里运行 initProcess()，第一行就是 io.ReadAll(pipe) → 阻塞等待。

#### (f) 配置 cgroup（120–141 行）

```go
if err := cg.AddProc(cmd.Process.Pid); err != nil {
```

创建 cgroup（写文件，比如 /sys/fs/cgroup/mydocker/<name>/）。
Apply 写资源限制（内存上限、CPU 配额）。
AddProc 把子进程 PID 写进 cgroup.procs 文件 —— 这一步必须在父进程做，因为子进程在新 PID namespace 里看不到自己的"真实 PID"。

#### (g) 网络配置（145–156 行）

```go
// 网络配置：此时 cmd 已启动，netns 存在；init 子进程正阻塞在 pipe 读上，
// 还没 exec 用户命令。这是调用 CNI ADD 的最佳时机。
```

子进程已经有了独立的 netns（在 /proc/<pid>/ns/net），但里面只有 lo（loopback），没有真正的网卡。
父进程调 CNI 插件（比如 bridge），让插件在子进程的 netns 里造一对 veth、配 IP、加路由。
之所以必须现在做：子进程还没 exec，netns 是空的、安全的。等用户命令跑起来再改网络就晚了。

#### (h) 发 payload，子进程被唤醒（158–172 行）

json.NewEncoder(w).Encode(payload) 把启动参数写进管道，子进程的 ReadAll 立刻拿到数据返回，往下走。

#### (i) 返回 Handle

Handle 是父进程拿在手里的"句柄"，里面有 cmd（用于 Wait）、cgroup（用于销毁）、PID、cgroup 路径、容器 IP。

### 3. wait() 和 release()

```go
func (h *Handle) wait() (int, error) {
err := h.cmd.Wait()
...
if derr := h.cgroup.Destroy(); derr != nil {
```

Wait() 阻塞到容器进程退出，然后关闭日志文件 + 销毁 cgroup。
release() 用在 detach 模式：父进程不 wait，直接放手，让子进程"游离"在系统里（后续 ps/logs 通过 PID 找它）

### 4. initProcess() —— 子进程入口（在新 namespace 里执行）

这是子进程第二次进入 mini-docker 程序时跑的。 container_linux.go:213-227

```go
   func initProcess() error {
   pipe := os.NewFile(uintptr(initPipeFD), "init-pipe")
   ...
   raw, err := io.ReadAll(pipe)         // ← 这里阻塞，等父进程发 payload
   ...
   json.Unmarshal(raw, &payload)
```

读取并解析父进程发来的 JSON 指令。

```go
// 加入已有 namespace（CRI 场景：加入沙箱的 netns/ipc/uts）
if len(payload.JoinNS) > 0 {
if err := joinNamespaces(payload.JoinNS); err != nil {
```

如果是 Pod 里的容器，调 setns(2) 加入沙箱的 net/ipc/uts。
sethostname 设置容器主机名（uts namespace 里独立）。
rootfs.Setup 做 pivot_root：把 / 替换成镜像目录，并挂载 /proc /sys /dev。这是最关键的一步，容器的"文件系统隔离"就靠它。
container_linux.go:255-270
// 合并环境变量：父 env + payload.Env（后者优先覆盖）
合并环境变量，去重。 container_linux.go:272-282

```go
bin, err := exec.LookPath(payload.Cmd[0])
...
if err := syscall.Exec(bin, payload.Cmd, envSlice); err != nil {
```

syscall.Exec = execve(2) 系统调用。它会用用户的命令（比如 /bin/sh）替换掉当前进程的内存映像，PID 不变。
从这一刻起，进程就不再是 mydocker 了，而是 sh/nginx/whatever。容器就"诞生"了。

### 5. joinNamespaces / nsCloneFlag / clearCloneFlag

辅助函数：
joinNamespaces：打开 /proc/<sandbox-pid>/ns/net 这类文件，调 setns() 加入。
nsCloneFlag：字符串名 → 内核常量（"net" → CLONE_NEWNET）。
clearCloneFlag：从 flags 里把某个位清除掉（用于"不创建、要加入"的场景）。&^ 是 Go 的 AND-NOT 运算符。

## 四、几个最容易卡住的"专业词"翻译

| 术语        | 通俗解释                                                |
| ----------- | ------------------------------------------------------- |
| clone flags | 告诉内核 fork 子进程时顺便创建哪些新 namespace 的开关位 |
| pivot_root  | 把进程的"根目录 /"切换到另一个目录，类似豪华版 chroot   |
| setns       | 让当前进程"跳进"另一个已存在的 namespace                |
| netns       | network namespace 的简写，一套独立的网卡/路由/iptables  |
| CNI         | 容器网络接口标准，K8s 用它给容器配网                    |
| cgroup      | 内核的资源限制机制，本质是 /sys/fs/cgroup 下的文件系统  |
| stdio       | 标准输入(0)/输出(1)/错误(2) 三个文件描述符              |
| pipe        | 内核里的一段缓冲区，一端写一端读，用于进程间通信        |
| detach      | 后台模式，父进程启动完就撒手，不等子进程                |
| TTY         | 终端设备，前台模式下容器的 stdio 接到你的终端，能交互   |
| payload     | "载荷"，就是要传过去的数据本身                          |

## 五、一句话总结

父进程用 clone 开一个套着新 namespace 的子进程让它先卡住 → 父进程在外面给它配好 cgroup 和网络 → 通过管道发指令 → 子进程做 pivot_root 换根
然后 exec 成用户命令。容器就跑起来了。

## 六、子进程继承管道读端为 FD 3 是怎么做到

背景：FD 在 fork/exec 时的继承规则
Linux 的进程模型里有一条重要规则：子进程默认继承父进程所有打开的文件描述符（FD），除非该 FD 被标记了 O_CLOEXEC（"exec 时关闭"）。

但"继承"不等于"编号一样"。父进程里的某个 pipe 可能是 FD 7，到子进程里也是 7。
问题是：子进程怎么知道要从 FD 几去读？ 总不能用环境变量传一个数字过去吧（虽然也行，但难看）。

Go 的 cmd.ExtraFiles 做了什么

```go
   cmd.ExtraFiles = []*os.File{r}  // r 是 pipe 读端
```

这一行让 Go 在底层做了三件事（在 os/exec/exec.go 里）：

fork 之前：把 r 这个 \*os.File 加到一个内部数组里。
fork 之后、exec 之前（在子进程里执行）：调 dup2(原FD, 目标FD) 系统调用，把这个 fd 强制复制到一个指定编号。
编号规则：cmd.ExtraFiles[i] 在子进程里就是 FD 3 + i。
为什么从 3 开始？因为 0/1/2 被 stdin/stdout/stderr 占了，3 是第一个可用的"自定义" FD。

用一张图说明

```text
父进程：
os.Pipe() 返回 r, w → 内核分配 FD 比如 r=7, w=8
cmd.ExtraFiles = []\*os.File{r}

cmd.Start() 触发 fork：
┌─ fork 出子进程，FD 全部复制过来（子进程里 r 也是 7）
│
└─ Go 在子进程里立刻调 dup2(7, 3)，让 FD 3 指向同一根管道
然后 close(7)，再 execve(...)

子进程里：
os.NewFile(uintptr(3), "init-pipe") ← 直接拿 FD 3 包成 \*os.File

父进程：
cmd.ExtraFiles = []\*os.File{r}

子进程：
pipe := os.NewFile(uintptr(initPipeFD), "init-pipe")
```

父子之间靠 initPipeFD = 3 这个约定好的常量对上号。这是一种朴素但极其稳健的"协议"，runc/Docker 也是同样做法。

## 七、pivot_root 把 / 替换成镜像目录是什么意思？

先类比一下
你电脑现在的 / 下有 /bin /etc /home /usr ...。这是你整个进程视野里看到的文件系统。

容器要做的事是：让容器进程看到的 / 变成另一个目录（比如 /var/lib/mydocker/containers/abc/merged，那里面解压了 alpine 镜像）。
这样容器里 ls / 看到的就是 alpine 的内容，完全意识不到宿主机的存在。

旧的工具叫 chroot，但它有逃逸漏洞。pivot_root 是它的"豪华安全版"。
pivot_root(new_root, put_old) 的语义
这个系统调用做两件事（原子的）：
把进程的"根目录 /"指向 new_root。
把老的根目录挂载到 new_root/put_old 这个位置（也就是塞到一个子目录里）。
之后我们再把 put_old umount 掉、删除目录，老的根就彻底从这个 mount namespace 里消失了。

项目里的实际步骤

```go
func setup(newRoot string, extraMounts []Mount) error {
// 0. 把整棵树改成 private，否则 pivot_root 里的 mount 会泄漏到宿主机
if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil {
```

我把每一步翻译成"人话"：

| 步骤 | 代码                                             | 干了什么                                      | 为什么要做                                                                                  |
| ---- | ------------------------------------------------ | --------------------------------------------- | ------------------------------------------------------------------------------------------- |
| 0    | Mount("", "/", "", MS_PRIVATE\|MS_REC, "")       | 把当前 mount namespace 里所有挂载标记为"私有" | 否则容器里的挂载会传播回宿主机，污染主机                                                    |
| 1    | Mount(newRoot, newRoot, "", MS_BIND\|MS_REC, "") | 把 newRoot 目录 bind 到自己                   | pivot_root 要求 new_root 必须是一个挂载点。普通目录不行，所以自己 bind 一下让它"变成"挂载点 |
| 2    | MkdirAll(newRoot/.pivot_root)                    | 在新根里建个子目录                            | 用来临时安放老根（put_old 必须在 new_root 内部）                                            |
| 3    | PivotRoot(newRoot, pivotDir)                     | 切换！新根成为 /，老根挂到 /.pivot_root       | 核心一步                                                                                    |
| 4    | Chdir("/")                                       | 把工作目录切到新的 /                          | 否则当前目录还指向老根的某个位置                                                            |
| 5    | Unmount("/.pivot_root", MNT_DETACH) + Remove     | 卸载老根，彻底看不见宿主机                    | 安全：容器里就算是 root 也访问不到宿主机文件                                                |
| 6    | mountDefaults()                                  | 挂 /proc /sys /dev                            | 容器里这些目录是空的，得挂上才能用 ps、cat /proc/...                                        |

/proc 为什么必须重新挂？
/proc 是个伪文件系统，内容由内核根据"当前 pid namespace"动态生成。容器在新 PID namespace 里，必须在容器里重新 mount -t proc proc /proc，这样 ps 才只看到容器自己的进程（PID 1, 2, ...），看不到宿主机的几千个进程。

### （一）一句话总结

pivot_root = "把进程的根目录原地换成另一个目录，并把老的根从视野里抹掉"。这是容器实现"文件系统隔离"的关键一步，光靠 namespace 是不够的，还得真的换根。

## 八、cgroup 详解

### 一、cgroup 是什么

cgroup（control group）= Linux 内核提供的"资源限制 + 统计"机制。

它跟 namespace 是两套独立的东西：
namespace 管"看到什么"（隔离视野）
cgroup 管"能用多少"（限制资源）
容器 = namespace + cgroup + rootfs 三件套。

### 二、cgroup v2 的核心模型：一棵目录树

打开 /sys/fs/cgroup，它本身就是个挂载点（类型是 cgroup2fs）。这个目录就是整棵 cgroup 树的根。

```text
/sys/fs/cgroup/ ← 根 cgroup（包含系统所有进程）
├── cgroup.procs ← 哪些 PID 属于本组（写文件 = 加入）
├── cgroup.controllers ← 本层可用的控制器：cpu memory io ...
├── cgroup.subtree_control ← 给"子目录"启用哪些控制器（关键！）
├── memory.max ← 内存上限（写一个数字就是字节数）
├── cpu.max ← CPU 配额："quota period"
│
├── mydocker/ ← 自己 mkdir 出来的子 cgroup（=parent）
│ ├── cgroup.subtree_control
│ └── abc123/ ← 又一个子目录（=容器自己的叶子 cgroup）
│ ├── cgroup.procs ← 把容器进程 PID 写这里
│ ├── memory.max ← 写 "104857600" = 100MB 上限
│ └── cpu.max ← 写 "50000 100000" = 给 50% CPU
```

关键点：cgroup 就是文件系统。 创建 cgroup = mkdir，限制资源 = 往文件里 echo 一个数字，加入进程 = 把 PID echo 到 cgroup.procs。没有什么神奇的 API

容器的 cgroup → 是 cgroup 子系统 的功能
cgroup namespace → 是 namespace 子系统 的功能
只是历史包袱让它俩在 Linux 内核里一起出现而已。它们各自单独存在，可以分开理解。

假设你 K8s 集群里跑了个 nginx 容器。

1. "容器的 cgroup" 在做什么（资源限制）
   这个 nginx 进程被分配到一个叫 xxx.scope 的目录下：

```text
   /sys/fs/cgroup/
   └── kubepods.slice/
   └── pod_abc.slice/
   └── cri-containerd-nginx.scope/ ← nginx 进程的 cgroup 节点
   ├── memory.max = 512MB ← 内核：你最多用 512MB
   └── cgroup.procs ← nginx PID 写在这里
```

如果 nginx 试图分配第 513MB，内核直接 OOM 它。
这就是"容器的 cgroup"——它是一个目录 + 一些文件，内核读这些文件来实施限制。没有它，nginx 就能吃满整台机器内存。

2. "cgroup namespace" 在做什么（视野遮罩）
   nginx 进程在容器里跑 cat /proc/self/cgroup，会看到什么？
   没装 cgroup namespace 时：

```bash
# 容器内
$ cat /proc/self/cgroup
0::/kubepods.slice/kubepods-burstable.slice/pod_abc.slice/cri-containerd-nginx.scope
```

问题：nginx 看到了一堆它本不该知道的东西——kubepods.slice、pod_abc、cri-containerd。这些是宿主机怎么组织 K8s 的内部细节，泄露给容器了。

装了 cgroup namespace 之后：

```bash
# 容器内（一模一样的命令）
$ cat /proc/self/cgroup
0::/                         ← 看到一个根，仅此而已
```

容器以为自己就在 cgroup 树根，啥都不知道。
关键：第 2 步没有改变 nginx 实际能用多少内存
cgroup namespace 只改了 nginx 看到的字符串。它的实际限制还是 512MB——内核照样在
memory.max这个真实位置 enforce。

在 mini-docker 里对应到代码

```go
// 1. "容器的 cgroup" —— 创建目录、写限制、登记 PID
cg := cgroup.NewWithConfig(...)        // mkdir /sys/fs/cgroup/.../<容器>/
cg.Apply(cfg.Resources)                // echo 512M > memory.max
cg.AddProc(cmd.Process.Pid)            // nginx PID → cgroup.procs

// 2. "cgroup namespace" —— 在 fork 时加一个 flag
cmd.SysProcAttr = &syscall.SysProcAttr{
    Cloneflags: ... | syscall.CLONE_NEWCGROUP,    // ← 这一位 = 给个新眼罩
}
```

一个是几次文件读写，一个是fork 时的一个 bit flag。完全独立的两件事。

#### 命令 1：我的文件系统看起来啥样？

```bash
$ pwd
/

$ ls /
bin  etc  home  proc  sys  tmp  usr  var       ← 这是 pivot_root 切来的镜像 rootfs

# 命令 2：我在 cgroup 体系里挂哪儿？
$ cat /proc/self/cgroup
0::/kubepods.slice/.../nginx.scope               ← 这是 cgroup 树里的位置
```

注意第 2 条的输出：路径
nginx.scope
不是一个真实存在的目录路径！它是 cgroup 子系统自己的"地址"，跟你 pivot_root 切到的 rootfs 完全无关。

那 pivot_root 切了什么？切的不是这个
pivot_root 切的是第一种路径——你 ls / 看到的那个根。它把"原本是宿主机镜像目录的 /var/lib/.../merged"变成了容器视角下的 /。

但它不动 cgroup 体系。/proc/self/cgroup 这个文件永远反映的是 cgroup 子系统自己的视图，跟你的文件根在哪没关系。

验证一下：装了 pivot_root 但没装 cgroup namespace 会怎样
历史上 cgroup namespace 是 Linux 4.6 才加的（2016 年）。在它之前，所有容器都已经在用 pivot_root 了——但是 /proc/self/cgroup 一直泄露！

```bash
# Linux 4.5 之前的 docker 容器里：
$ ls /
bin  etc  ...                         ← pivot_root 把 / 切对了 ✅

$ cat /proc/self/cgroup
0::/docker/abc123def456...            ← 但 cgroup 路径还是泄露 ❌
                                      宿主机的"docker"父级被容器看到
```

文件系统隔离完美，cgroup 视图却暴露宿主机的内部组织——这就是为什么 4.6 又加了 cgroup namespace 来补上这个洞。

为什么 pivot_root 不能顺便把它一起切了
因为 /proc/self/cgroup 不是一个普通文件。
/proc 是个虚拟文件系统，里面的"文件"内容是内核临时生成的字符串。当你 cat /proc/self/cgroup，内核做的事是：

```bash
// 伪代码
查 current task → 读 task->cgroups 里挂在哪个 cgroup 节点
                  → 把节点的全路径序列化成字符串
                  → 返回给用户
pivot_root 改的是 mount table。mount table 里只能记"哪个目录挂哪个 fs"——它没有能力告诉内核"以后 task->cgroups 字段读出来的字符串前缀要裁掉"。这是另一个维度的事，必须由专门的 namespace 来管。
```

一个具体类比
想象你在一栋公司大楼里。
pivot_root/mount namespace = 把你带到一间假装是整栋楼的小会议室。你打开会议室的门看到的所有空间都是这个小屋——你以为整栋楼就这么大。
cgroup namespace = 修改你工牌上的部门信息。原本工牌写"研发部 → 平台组 → 容器组"，现在改成"我在公司根目录"。走的还是那条路、跟同样的人打交道，只是工牌名片上的"我属于哪儿"变了。
它们在你的"自我认知"里修改了不同的部分：
一个改"周围的物理空间"
一个改"对自己组织位置的描述"
容器要"装得像个独立机器"，两个都得改。
因为 cgroup 树不是文件系统空间——它是 cgroup 子系统自己内部的路径概念。/proc/self/cgroup 这个文件泄露的不是文件路径，而是 cgroup 路径——pivot_root 救不了。

一句话：cgroup = 一组进程 + 这组进程的资源限制
把它理解成 Linux 给进程加的一张"标签"：
"这一组进程"加在一起最多用 1GB 内存、最多用 50% CPU。
仅此而已。所有"复杂"的东西都是从这个简单概念发散出去的。
假设你要从零设计这个机制，你会怎么做？
第 1 步：你需要表达"这一组进程"
怎么把"一组进程"在内核里表达出来？
最简单的办法——做个目录，把进程 PID 列在里面：

```text
我的目录 /web/
├── procs: 1234 5678 9012 ← 这三个进程是一组
├── memory.max: 1G ← 它们加起来最多 1GB
└── cpu.max: 50% ← 它们加起来最多 50% CPU
```

这就是一个 cgroup。
⚡ 一个 cgroup ≠ 一棵树，一个 cgroup 就是一个目录。

第 2 步：你需要多组怎么办？
用户跑了 web 进程组，又跑了 db 进程组——再做一个目录就行：

```text
/web/ memory.max: 1G ← cgroup 1
/db/ memory.max: 2G ← cgroup 2
```

到此为止，cgroup 还是平铺的，不是树。每个目录互不相干。

第 3 步：什么时候出现"树"？
业务复杂了。
你想说："web 这一类整体"加起来不能超 4GB，而 web 类下面又有 nginx 和 apache 两个具体的——nginx 1G、apache 2G。
光有平铺 cgroup 表达不出这种"组的组"。所以加了一个能力：目录可以套目录。

```text
/web/ memory.max: 4G ← "web 这一类"上限
├── nginx/ memory.max: 1G ← web 下面具体的
└── apache/ memory.max: 2G ← web 下面具体的

/db/ memory.max: 2G ← 另一类
```

这下出现了"树"——只是因为 cgroup 目录可以嵌套了。
⚡ "cgroup 树" = "cgroup 目录的嵌套结构"。不是一个新东西，是 cgroup 本身的目录嵌套结果。

第 4 步：树的好处是什么？
孩子节点的限制叠加在父节点限制之下：

```text
/web/ memory.max: 4G
├── nginx/ memory.max: 1G
└── apache/ memory.max: 2G
```

意思是：
nginx 自己最多 1G
apache 自己最多 2G
nginx + apache 加起来最多 4G（被 /web/ 顶住）
如果 nginx 在用 3G、apache 想申请 2G，会因为 /web/ 总额超了被拒——即使 apache 自己的 2G 没用满。

这是 K8s 实现 QoS 的根基：

```text
/kubepods/ 全集群可用 8G
├── besteffort/ 这一类整体 4G ← 低优先级 Pod 加起来不能超 4G
└── burstable/ 这一类整体 6G
```

平铺做不到这种"组的组"配额，所以才有了树。

所以"cgroup 树"到底是啥？
就是机器上所有 cgroup 目录因为可以嵌套，自然形成的一棵目录树。它不是一个独立概念。

```text
/sys/fs/cgroup/ ← 这个目录就是树根
├── system.slice/ ← 里面的子目录就是子节点
│ └── nginx.service/
├── kubepods.slice/
│ └── pod_abc/
│ └── nginx.scope/
```

打开 /sys/fs/cgroup/，你看到的目录结构就是 cgroup 树本身——它不抽象、不藏在内核里，就是文件系统里你 ls 能看到的那些目录。

回到容器场景
启动容器 = 在 cgroup 树某个父节点下新建一个目录：

```text
启动前：
/sys/fs/cgroup/└── kubepods.slice/└── pod_abc/ (空)

启动 nginx 后：
/sys/fs/cgroup/└── kubepods.slice/└── pod_abc/
└── nginx.scope/ ← mkdir 出来的
├── memory.max: 512M
└── procs: 1234
```

nginx 进程被"放进"了 nginx.scope 这个 cgroup——也就是它的 PID 写在那个目录的 procs 文件里。从此内核就用这个 cgroup 的限制管它。

一图把"cgroup"和"cgroup 树"区分开

```text
单个 cgroup cgroup 树
（一个目录） （所有 cgroup 目录嵌套形成的全图）

┌─────────┐ /sys/fs/cgroup/
│ /web/ │ ├── system.slice/
│ - 1234 │ ├── user.slice/
│ - 5678 │ └── kubepods.slice/ ←
│ mem 1G │ ├── besteffort/ ←─ 多个 cgroup 嵌套
└─────────┘ └── burstable/ ← 起来就是树
└── pod_abc/
└── nginx.scope/
```

cgroup = 给一组进程戴的一个"资源限额标签"，在 Linux 里实现成一个目录。 cgroup 树 = 因为这些目录可以嵌套，自然画出的那棵图。

先说 cgroup namespace 干了啥
把 /proc/self/cgroup 的输出前缀截掉：

```bash
# 没装 cgroup ns
$ cat /proc/self/cgroup
0::/kubepods.slice/.../pod_abc.slice/cri-containerd-nginx.scope

# 装了 cgroup ns
$ cat /proc/self/cgroup
0::/                                # 简洁干净，看不到上下文
```

仅此而已——只改一行字符串输出。
那"不要它会怎么样"——分四个真实问题讲
问题 1：信息泄露——容器知道宿主机的内部"组织架构"
之前讲过，cgroup 树长得像组织架构图。容器看到 /kubepods.slice/... 这条满路径，相当于看到：
"我现在跑在 K8s 集群里"
"我是个 Pod，UID 是 abc"
"我用的运行时是 cri-containerd"
"我是 burstable QoS 类的"
这本身不是漏洞，但是侦察情报：

攻击者拿下了一个用户容器（比如通过 web 应用 RCE）
cat /proc/self/cgroup 一秒钟告诉他"哦，这是 K8s 集群"
接下来的渗透就有方向了：试 SA token、试 metadata API、试 CRI socket
如果他看到 /system.slice/docker-xxx，他就知道是单机 docker，渗透手法换一套
跟 uname / /proc/version 一样，单独不致命，是攻击链的第一步。

问题 2：容器内的工具拿到错误结果——这个真会出 bug
想象一个 Java 容器跑 top、htop、或者用 cgroup 信息做 GC 决策。这些工具是怎么"知道自己资源限制"的？
它们会去读 cgroup 文件——但怎么找到自己的 cgroup 文件？

```bash
// 工具的伪代码：找到我自己的 memory.max
1. cat /proc/self/cgroup           // 我在哪个 cgroup？
   → "0::/kubepods.slice/.../nginx.scope"
2. open /sys/fs/cgroup + 上面那个路径 + "/memory.max"
   → /sys/fs/cgroup/kubepods.slice/.../nginx.scope/memory.max
3. 读出来：512M    ✅ 限制正确
```

但容器里的 /sys/fs/cgroup/ 是经过 mount namespace 切的——通常只暴露容器自己的子树：
容器里 ls /sys/fs/cgroup/
memory.max cpu.max cgroup.procs ... ← 看似在根目录直接有文件
容器里的 /sys/fs/cgroup/ 实际是宿主机 /sys/fs/cgroup/kubepods.slice/.../nginx.scope/ 重新挂在 /sys/fs/cgroup/。
没装 cgroup namespace 时，第 1 步告诉工具"我在
nginx.scope
"，第 2 步它就去拼路径——
/sys/fs/cgroup/ + /kubepods.slice/.../nginx.scope/memory.max
= /sys/fs/cgroup/kubepods.slice/.../nginx.scope/memory.max
但容器里的 /sys/fs/cgroup/ 已经被裁过了，这个长路径在容器里根本不存在！工具读不到自己的 memory.max，要么报错，要么 fallback 用宿主机总内存当上限——你 nginx 容器以为自己有 32GB，实际给的是 512MB，跑两步直接 OOM。

装了 cgroup namespace，/proc/self/cgroup 输出 /，工具拼出来
memory.max
——这正好是容器里能看到的那个文件。对得上了。
这才是 cgroup namespace 真正的实用价值：让"通过 /proc/self/cgroup 找自己 cgroup 文件"的逻辑在容器里自洽。

问题 3：跨容器迁移检查会失败
CRIU（Container Runtime in Userspace）这种容器热迁移工具，会快照容器的所有状态，包括 cgroup 路径，然后到目标机器恢复。
没 cgroup namespace 时：
源机器容器的 cgroup 路径是
pod_abc.scope
迁到目标机器后，目标机器没有这个父级（可能用的是 /system.slice/...），CRIU restore 失败
有 cgroup namespace 时：
容器看到的 cgroup 路径就是 /
不依赖宿主的具体组织方式，迁哪都能恢复
问题 4：嵌套容器（容器里跑 docker）做不了
DinD（Docker in Docker）需要在容器里再跑一个 docker daemon，里面再起容器。子 docker 也想 mkdir 自己的 cgroup 子目录：

没 cgroup namespace 时，子 docker 在容器里看到自己已经在
docker-xxx.scope
这个叶子节点上了——cgroup v2 不允许在已经有进程的叶子节点下面再 mkdir 子目录（"内部节点不能有进程"约束）。结果子 docker 没法创建任何 cgroup → 嵌套容器没资源限制。

有 cgroup namespace 时，子 docker 看到自己挂在 /，可以理直气壮地在自己的"根"下面 mkdir 任意子目录——实际这些子目录创建在宿主的 /system.slice/docker-xxx.scope/foo/，但子 docker 不知道也不需要知道。嵌套容器正常工作。

一个具体演示
如果你 lima 里那个 nginx 容器还在跑：

```bash
# 进 VM
limactl shell mydocker

# 看 nginx 容器内的视图
sudo nsenter -t $(pgrep -f 'nginx: master' | head -1) -m -p -u -i -- bash -c '
  echo "=== cat /proc/self/cgroup ==="
  cat /proc/self/cgroup
  echo
  echo "=== ls /sys/fs/cgroup/ ==="
  ls /sys/fs/cgroup/ | head
'
```

如果 mydocker 没给容器加 CLONE_NEWCGROUP（默认就没加），你会看到：

```bash
=== cat /proc/self/cgroup ===
0::/system.slice/.../mydocker-some-name.scope     ← 暴露宿主路径
```

如果你要给 nginx 容器读自己的 memory.max，按这条路径拼出来的文件在容器里根本不存在（容器内 /sys/fs/cgroup/ 是裁过的）——上面问题 2 的实景。

所以"看到一堆它本不该知道的东西"具体指啥
不是"看到个具体路径字符串"那么轻——真正的伤害是：

| 问题                  | 严重程度 | 实际后果                                |
| --------------------- | -------- | --------------------------------------- |
| 信息泄露              | 低       | 攻击者侦察情报                          |
| cgroup 文件路径对不上 | 高       | Java/Go runtime 拿不到正确内存限制，OOM |
| 容器迁移失败          | 中       | CRIU 之类的 restore 出错                |
| 嵌套容器无法限资源    | 高       | DinD 里的容器随便吃宿主资源             |

cgroup namespace 不是为了"防偷看"——它是让容器里的工具能正确工作的关键基础设施。

没 cgroup namespace 时，容器里的进程问"我在哪？"得到的是宿主视角的全路径（
xxx.scope
）； 但它能访问的 /sys/fs/cgroup/ 是容器视角（已经被裁短）。 两套视角对不上，工具就完蛋。 cgroup namespace 把"我在哪"也改成容器视角（/），两套视角对得上了。

### 三、v2 最容易卡住的点：subtree_control

一个 cgroup 想用某个控制器（比如 memory），它的父目录必须先在 cgroup.subtree_control 里写 +memory，把这个控制器"下发"给子目录

举例：你想给 /sys/fs/cgroup/mydocker/abc123/memory.max 写值，那么：
/sys/fs/cgroup/cgroup.subtree_control 里必须有 memory（开给 mydocker）
/sys/fs/cgroup/mydocker/cgroup.subtree_control 里必须有 memory（开给 abc123）
这时 abc123 目录里才会出现 memory.max 文件，可以写。
这正是你代码里 ensureControllerAt 干的事：

```go
func ensureControllerAt(dir, name string) error {
```

```go
// 逐级从 cgroupV2Root 到 dir，一层层 enable
```

逐级往下启用，因为内核就是这么强制的（v1 没这个要求，v2 加上是为了避免一棵树同时被多种控制器拖累性能）

### 四、对照你代码看完整流程

start() 调了 3 个方法：NewWithConfig → Apply → AddProc。

#### Step 1：NewWithConfig —— 只算路径，不操作

```go
func newCgroupfsManager(cfg Config) (Manager, error) {
     parent := strings.TrimPrefix(cfg.Parent, "/")
     path := filepath.Join(cgroupV2Root, parent, cfg.Name)
return &v2Manager{name: cfg.Name, path: path}, nil
}
```

仅仅算出最终目录路径，比如 /sys/fs/cgroup/mydocker/abc123。还没有真去碰文件系统。

#### Step 2：Apply(resources) —— 创建 cgroup + 写限额

```go
func (m *v2Manager) Apply(r Resources) error {
   // 父级必须在 cgroup.subtree_control 里显式 enable 子 cgroup 要用的控制器。
   if err := ensureControllerAt(filepath.Dir(m.path), "memory"); err != nil {
return err
}
if err := ensureControllerAt(filepath.Dir(m.path), "cpu"); err != nil {
   return err
}
if err := os.MkdirAll(m.path, 0755); err != nil {
    ...
}
if r.MemoryBytes > 0 {
    if err := writeFile(filepath.Join(m.path, "memory.max"), ...); err != nil {
```

翻译成 shell 等价命令：
ensureControllerAt 干的事

```bash
echo "+memory" > /sys/fs/cgroup/cgroup.subtree_control
echo "+memory" > /sys/fs/cgroup/mydocker/cgroup.subtree_control
echo "+cpu"    > /sys/fs/cgroup/cgroup.subtree_control
echo "+cpu"    > /sys/fs/cgroup/mydocker/cgroup.subtree_control
```

MkdirAll 干的事

```bash
mkdir -p /sys/fs/cgroup/mydocker/abc123
```

写资源限制

```bash
echo 104857600 > /sys/fs/cgroup/mydocker/abc123/memory.max     # 100MB
echo "50000 100000" > /sys/fs/cgroup/mydocker/abc123/cpu.max   # 50% CPU
```

cpu.max 的两个数字含义：每 period 微秒里，最多用 quota 微秒 CPU 时间。50000 100000 = 每 100ms 里最多用 50ms = 半个核

#### Step 3：AddProc(pid) —— 把进程关进笼子

```go
func (m *v2Manager) AddProc(pid int) error {
return writeFile(filepath.Join(m.path, "cgroup.procs"), strconv.Itoa(pid))
}
```

等价于：

```bash
echo 12345 > /sys/fs/cgroup/mydocker/abc123/cgroup.procs
```

从这一刻起，PID 12345 及其所有子进程（除非被显式移走）都被这个 cgroup 的限制约束。内存超 100MB 就被 OOM kill，CPU 用超就被节流。

#### Step 4：Destroy() —— 容器退出时清理

```go
func (m *v2Manager) Destroy() error {
if err := os.Remove(m.path); err != nil && !os.IsNotExist(err) {
```

```bash
rmdir /sys/fs/cgroup/mydocker/abc123
```

注意只有 cgroup 里没有进程时才能删（容器都退出了，所以可以删）

### 五、为什么必须在父进程里 AddProc

```go
if err := cg.AddProc(cmd.Process.Pid); err != nil {
```

cmd.Process.Pid 是宿主机视角看到的子进程 PID（比如 12345）。而 cgroup 文件系统在宿主机里，它认的就是宿主机的 PID
如果让子进程自己 echo $$ > cgroup.procs，它在自己的 PID namespace 里看到的 $$ 是 1，写进去就完全错了（PID 1 是宿主机的 systemd）。所以只能父进程做

### 六、v1 vs v2

你代码里有这一行：

```go
if !isV2() {
   return nil, errors.New("cgroups v2 not available; v1 support not implemented yet")
}
```

v1：每个控制器一棵独立的树，挂在不同目录（/sys/fs/cgroup/memory、/sys/fs/cgroup/cpu...），管理起来很乱。
v2：所有控制器统一一棵树，就是 /sys/fs/cgroup。现代发行版（Ubuntu 22.04+、Fedora 31+、CentOS 9）默认都是 v2。
你的 mini-docker 只支持 v2，简单干净。

### 七、自己动手验证（在 Linux 上）

```bash
# 1. 看看你的系统是不是 v2
mount | grep cgroup
# cgroup2 on /sys/fs/cgroup type cgroup2 (rw,...)

# 2. 手动建个 cgroup 限制一个 shell
sudo mkdir /sys/fs/cgroup/test
echo "+memory" | sudo tee /sys/fs/cgroup/cgroup.subtree_control
echo 50000000 | sudo tee /sys/fs/cgroup/test/memory.max   # 50MB

# 3. 把当前 shell 扔进去
echo $$ | sudo tee /sys/fs/cgroup/test/cgroup.procs

# 4. 跑个吃内存的程序，看它被 OOM kill
python3 -c 'x=[0]*10**8'   # Killed
```

这就是 cgroup 的全部"魔法"——一切都是文件读写

## 九、overlay 和 pivot_root 的关系：两件事，缺一不可

### 一、用一个比喻先建立直觉

把容器想象成一个"假装住进新房子"的过程：

| 步骤     | 类比                                       | 对应技术                      |
| -------- | ------------------------------------------ | ----------------------------- |
| 造房子   | 把一堆建材（镜像层）拼出一栋完整的房子     | overlayfs（产出 merged 目录） |
| 搬进去住 | 让你的"户口"指向这栋新房子，旧家从此看不见 | pivot_root                    |

merged 只是宿主机文件系统里的一个普通目录而已（路径形如 /var/lib/mydocker/containers/abc/merged）。如果不做 pivot_root，容器进程的 / 还是宿主机的 /——它依然能 ls /etc、cat /etc/shadow，看到宿主机的所有东西

pivot_root 才是把这个目录挂上"根"的位置，让容器进程从此"看不见"宿主机

### 二、两个文件分别在做什么

overlay_linux.go 的工作（在父进程里）
目标：拼出一份"看起来像 alpine 镜像"的目录树。
执行前：

```text
/var/lib/mydocker/containers/abc/
├── upper/ （空）
├── work/ （空）
└── merged/ （空）

/var/lib/mydocker/images/alpine/
├── layer1/content/ （只读，里面有 /bin /etc /usr ...）
├── layer2/content/
└── layer3/content/

mount("overlay", merged, "overlay", 0, "lowerdir=l3:l2:l1,upperdir=upper,workdir=work")
```

执行后：

```text
merged/ ← 现在 ls 它，看到的就是 alpine 完整的文件系统
├── bin/ etc/ usr/ var/ ...
```

重点：merged 仍然是宿主机文件系统树的一个分支。它的绝对路径是 /var/lib/mydocker/containers/abc/merged。容器进程如果只走到这一步，它的 / 还是宿主机的根。

pivot_root 之前（容器进程视角）：

```text
/
├── bin/ ← 宿主机的 /bin
├── etc/ ← 宿主机的 /etc（能看到宿主机的所有秘密！）
├── var/lib/mydocker/containers/abc/merged/ ← 我们的 alpine 在这儿
│ ├── bin/ etc/ ...
└── ...
```

pivot_root 之后：

```text
/
├── bin/ ← 现在是 alpine 的 /bin
├── etc/ ← 现在是 alpine 的 /etc
└── ...
（宿主机的根从视野里消失，访问不到了）
```

### 三、为什么必须分两步？能不能合一步？

不能。 因为 namespace 和挂载操作有约束：

overlay mount 必须在父进程做（在 start() 流程里早于 fork init 子进程的某个时机），因为它涉及读取镜像元数据、创建目录等准备工作。
pivot_root 必须在子进程做，且子进程必须已经在新的 mount namespace 里——否则它的 pivot_root 会把宿主机的根也换掉，灾难性后果。
所以流程必然是：
父进程：

1. overlay mount → 产出 merged 目录
2. clone(CLONE_NEWNS|...) fork 出子进程，子进程进入新 mount namespace
3. 通过 pipe 把 merged 路径告诉子进程

子进程（在新 mount namespace 里）：

4. pivot_root(merged, .pivot_root) ← 用 merged 当新根
5. exec 用户命令

### 四、串起整个项目的全景图

```text
┌─────────────────────────────────────────────────────────────┐
│ 父进程（mydocker run） │
│ │
│ ① image 包：prepareRootfs() │
│ └─ syscall.Mount("overlay", merged, "overlay", ...) │
│ 把多层镜像合成一个 merged 目录 │
│ │
│ ② container 包：start() │
│ ├─ os.Pipe() │
│ ├─ exec.Command("/proc/self/exe", "init") │
│ │ SysProcAttr.Cloneflags = NEWNS|NEWPID|NEWNET|... │
│ ├─ cmd.Start() ← 内核 clone 出子进程 │
│ ├─ cgroup.Apply / AddProc(子进程PID) │
│ ├─ CNI 配网络 │
│ └─ 通过 pipe 发 payload{Rootfs: merged, Cmd: ...} │
└─────────────────────────────────────────────────────────────┘
│ pipe
▼
┌─────────────────────────────────────────────────────────────┐
│ 子进程（mydocker init，在新 namespace 里） │
│ │
│ ③ container 包：initProcess() │
│ ├─ 读 pipe 拿 payload │
│ └─ rootfs.Setup(merged) ← 这里发生 pivot_root │
│ │
│ ④ rootfs 包：setup() │
│ ├─ Mount("/", MS_PRIVATE|MS_REC) 防挂载泄漏 │
│ ├─ Mount(merged, merged, MS_BIND) 让 merged 变成挂载点 │
│ ├─ PivotRoot(merged, merged/.pivot_root) ← 换根！ │
│ ├─ Chdir("/") │
│ ├─ Unmount("/.pivot_root") 抹掉宿主机老根 │
│ └─ mountDefaults() 挂 /proc /sys /dev │
│ │
│ ⑤ syscall.Exec(用户命令) │
│ 此刻进程"摇身一变"成了 sh / nginx / 你跑的任何东西 │
└─────────────────────────────────────────────────────────────┘
```

一句话：
overlay 准备的是素材（一个长得像镜像的目录）
pivot_root 做的是生效（把这个目录真正挂到容器进程的"根"位置）

### 小结

overlay 的 merged = 把镜像各层合并出的"看起来像 alpine 根目录的素材目录"，但它仍然位于宿主机文件系统树里。
pivot_root = 把这个素材目录真正挂到容器进程的 /，并抹掉宿主机老根，让容器进程从此与宿主机文件系统视野隔离。
两步顺序：父进程先做 overlay → fork 出带新 mount namespace 的子进程 → 子进程做 pivot_root。少任何一步容器都无法正确隔离。

### payload.Rootfs 的没赋值

赋值了，只是赋值发生在更上游。 我们顺着链路看一遍：

第一站：run.go 调 prepareRootfs 拿到 mergedRoot

```go
cfg := container.Config{
    ID:           id,
    Name:         name,
    Rootfs:       mergedRoot,
    Hostname:     o.hostname,
    WorkingDir:   finalWD,
    Cmd:          finalCmd,
```

mergedRoot 就是 image.prepareRootfs() 返回的那个 overlay 合并目录，形如：
/var/lib/mydocker/containers/abc123/merged
第二站：container.start() 把它放进 payload

```go
payload := initPayload{
    Rootfs:     cfg.Rootfs,
}
```

cfg.Rootfs 就是上面塞进来的 mergedRoot。

第三站：子进程读 payload，传给 rootfs.Setup

```go
if err := rootfs.Setup(payload.Rootfs, nil); err != nil {
```

完整链路图

```text
run.go:
mergedRoot, _ = image.prepareRootfs(...) ← overlay 合成 merged 目录
│
▼
container.Config{ Rootfs: mergedRoot }
│
▼ start(cfg)
initPayload{ Rootfs: cfg.Rootfs }
│
▼ json 编码 → pipe → 子进程
initProcess() 解码 payload
│
▼
rootfs.Setup(payload.Rootfs, nil)
│
▼
PivotRoot(newRoot, ...) ← newRoot 就是 merged
```

所以之前讲的"overlay 准备素材，pivot_root 把素材挂为根"在代码里就是通过这个 Rootfs 字段串起来的。

### extraMounts 传 nil

看 setup 函数最后一段注释（：
我们改成"在父进程里 bind mount 到 merged/"，这里 extraMounts 就无事可做。
也就是说：volume（-v 参数）实际上是在父进程里、overlay 完成之后、fork 之前，由 rootfs.ApplyBindMounts(merged, mounts) 直接 bind 到 merged/<target> 路径下。这样等子进程 pivot*root 后，volume 自然就出现在容器视野里了。
子进程这一步就什么都不用干，所以传 nil 占位，函数里直接 * = extraMounts 忽略。这是为了保持 API 对称（万一以后想换回"子进程内挂载"模式），不算冗余设计。

### setup 函数详细讲解

```go
func setup(newRoot string, extraMounts []Mount) error
```

我把它拆成 7 个步骤，每一步讲做了什么 + 为什么必须这么做。

#### Step 0：把整棵挂载树标为"私有"

```go
// 0. 把整棵树改成 private，否则 pivot_root 里的 mount 会泄漏到宿主机
```

```go
if err := syscall.Mount("", "/", "", syscall.MS_PRIVATE|syscall.MS_REC, ""); err != nil
```

背景：mount 传播（mount propagation）
Linux 的每个挂载点都有一种**"传播类型"**，决定它和其他 mount namespace 之间挂载事件是否互相影响：
shared：在 namespace A 里挂一个东西，namespace B 里也会自动出现这个挂载（默认是这个，systemd 启动时把整棵树改成了 shared）。
private：完全独立，互不影响。
slave：单向，从主到从。
为什么要 private？
我们的 init 子进程虽然用 CLONE_NEWNS 进了一个新的 mount namespace，但因为系统默认的挂载是 shared 的，子进程后面在容器里做的 mount 操作（比如挂 /proc /sys）会"传播"回宿主机！这会污染宿主机，也可能导致 pivot_root 直接失败。
所以第一件事就是把整棵树递归地（MS_REC）改成 private：

```go
mount("", "/", NULL, MS_PRIVATE|MS_REC, NULL)
```

这条语义是："把当前 mount namespace 里 / 下的所有挂载点改成 private"。改完后，子进程后续做的任何挂载都关在自己的 namespace 里出不去。
这一步是 runc/Docker 的标配，几乎是任何容器实现的"第一行代码"。

#### Step 1：把 newRoot bind mount 到自己

```go
// 1. bind mount newRoot 到自身，满足 pivot_root 的要求
```

```go
if err := syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil
```

pivot_root 系统调用的硬性要求
man page 里写得很清楚：new_root 必须是一个挂载点（mount point），不能是普通目录。

但 merged 此刻只是 overlay 挂载里的一个目标目录，对父进程来说它是挂载点，可经过 MS_PRIVATE|MS_REC 之后，到底它是不是被识别成"独立挂载点"会有歧义。最稳妥的办法是：
bind mount 它到自己——mount --bind /a/b /a/b。
这是一个特殊技巧：把目录 bind 到自己路径上，等于"强制让这个路径成为一个挂载点"，不改变内容。这样 pivot_root 的前置条件就满足了。

#### Step 2：在 newRoot 里建一个 .pivot_root 子目录

```go
pivotDir := filepath.Join(newRoot, ".pivot_root")
if err := os.MkdirAll(pivotDir, 0700); err != nil
```

pivot_root(new_root, put_old) 调用要求 put_old 路径必须在 new_root 内部（这样老根才能被"塞"进新根的某个子目录里）。
我们建 merged/.pivot_root 作为容纳老根的临时位置。0700 权限只让 root 访问，反正一会儿就删了。

#### Step 3：执行 pivot_root

```go
if err := syscall.PivotRoot(newRoot, pivotDir); err != nil
```

这是核心一步。系统调用语义：

```text
调用前：
/ ← 宿主机老根
└── merged/ ← 即将成为新根的目录
└── .pivot_root/

调用后：
/ ← 现在是原来的 merged
└── .pivot_root/ ← 老根被塞到这里
├── bin/ etc/ home/ ... （宿主机的 / 内容全在这）
```

注意从这一刻起，进程视野里的 / 就变了。它再 ls /etc 看到的就是 alpine 的 /etc，而不是宿主机的。

#### Step 4：Chdir("/")

```go
if err := syscall.Chdir("/"); err != nil
```

pivot_root 不会自动改变进程的 cwd（current working directory）。如果调用前 cwd 是 /some/path，那它现在指向的还是老根上的某个 inode，并没指向新根。这会导致后续相对路径全错。

所以显式 chdir("/") 把 cwd 也搬到新根。

#### Step 5：卸载并删除老根

```go
const oldRoot = "/.pivot_root"
if err := syscall.Unmount(oldRoot, syscall.MNT_DETACH); err != nil
```

/.pivot_root 现在挂的是宿主机老根。如果不卸载，容器 root 用户可以 cd /.pivot_root && ls，宿主机文件系统全暴露——这是容器逃逸级别的安全问题。
MNT_DETACH（lazy unmount）：即使有进程还在使用这个挂载点（理论上不可能，但保险），也允许"延迟卸载"——立刻从命名空间移除，等没人用时再真清理。
然后 os.Remove(oldRoot) 删掉那个空目录，连入口都不留。
执行完后，容器进程完全访问不到宿主机文件系统了。

#### Step 6：挂载 /proc /sys /dev

```go
if err := mountDefaults(); err != nil {
rootfs_linux.go:78-90
mounts := []m{
    {"proc", "/proc", "proc", syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV, ""},
    {"sysfs", "/sys", "sysfs", syscall.MS_NOSUID | syscall.MS_NOEXEC | syscall.MS_NODEV | syscall.MS_RDONLY, ""},
    {"tmpfs", "/dev", "tmpfs", syscall.MS_NOSUID | syscall.MS_STRICTATIME, "mode=755"},
}
```

镜像里这三个目录通常是空的（或者只有占位文件），需要在容器里重新挂上伪文件系统：

| 挂载  | 类型                  | 作用                                                                                                              |
| ----- | --------------------- | ----------------------------------------------------------------------------------------------------------------- |
| /proc | proc（伪文件系统）    | 内核动态生成的进程信息。容器在新 PID namespace 里，必须重挂 /proc，这样 ps 才只看到容器自己的进程                 |
| /sys  | sysfs                 | 内核对象树（设备、模块等）。挂只读，容器不该乱改                                                                  |
| /dev  | tmpfs（内存文件系统） | 设备节点目录。先挂个空 tmpfs，常用设备（/dev/null、/dev/zero）一般再 mknod 或 bind 进来（你这个项目目前简化处理） |

flag 含义：
MS_NOSUID：忽略 setuid/setgid 位（防止容器里通过 /proc 之类提权）
MS_NOEXEC：这个文件系统上的文件不可执行
MS_NODEV：忽略其中的设备节点
MS_RDONLY：只读挂载
MS_STRICTATIME：每次访问都更新 atime（POSIX 行为）
这些都是安全加固，业界通用做法。

#### Step 7：忽略 extraMounts（已经在父进程里挂好了）

```go
_ = extraMounts
return nil
```

如前面解释：volume 是父进程在 overlay 之后、fork 之前用 ApplyBindMounts(merged, mounts) 挂到 merged/<target> 的。pivot_root 后这些挂载就自然出现在新根里。子进程这里啥都不用做。

一张表总结整个 setup

| 步骤 | 一句话                      | 关键 syscall                                     |
| ---- | --------------------------- | ------------------------------------------------ |
| 0    | 让挂载操作不泄漏到宿主机    | mount("", "/", "", MS_PRIVATE\|MS_REC, "")       |
| 1    | 让 newRoot 成为合法挂载点   | mount(newRoot, newRoot, "", MS_BIND\|MS_REC, "") |
| 2    | 准备老根的安放位置          | mkdir(newRoot/.pivot_root)                       |
| 3    | 真正切根                    | pivot_root(newRoot, .pivot_root)                 |
| 4    | 切 cwd 到新根               | chdir("/")                                       |
| 5    | 抹掉老根，防逃逸            | umount("/.pivot_root", MNT_DETACH) + rmdir       |
| 6    | 挂 /proc /sys /dev          | mount(...) × 3                                   |
| 7    | volume 已在父进程挂好，跳过 | —                                                |

完成这 7 步，子进程的"文件系统视野"就是一个干净、隔离、配齐了 /proc 的容器世界，下一步 syscall.Exec 就可以让用户命令在里面跑了。

### 两个"bind mount"的细节深挖

这两个点都涉及到 bind mount，但用途完全不同。我先把 bind mount 这个概念讲透，然后两个场景就好理解了。

#### 一、先把 bind mount 讲清楚

普通 mount vs bind mount
普通 mount：把一个设备/文件系统挂到目录上。

```bash
mount -t ext4 /dev/sda1 /mnt        # 把 sda1 这块盘挂到 /mnt
mount -t proc proc /proc            # 把 proc 伪文件系统挂到 /proc
```

bind mount：把一个已存在的目录/文件"复制"挂到另一个位置。不复制数据，只是让两个路径指向同一份内容。

```bash
mount --bind /home/alice/data /mnt/data
# 现在 /home/alice/data 和 /mnt/data 是同一个东西
# 在任何一边写文件，另一边都能看到
```

bind mount 的关键性质

```bash
是同一份数据的两个入口（类似硬链接，但作用在整个目录树上）。
bind mount 后，目标路径就成了一个"挂载点"（mount point）。在 /proc/mounts 里能看到一条记录。
目标路径上原本的内容会被"遮盖"：如果 /mnt/data 原来有文件 foo.txt，bind 之后这些文件还在磁盘上，但你 ls /mnt/data 看不见，看到的是 /home/alice/data 的内容。umount 之后又恢复可见
```

#### 二、场景 A：mount --bind /merged /merged（把目录 bind 到自己）

为什么有这种"自己 bind 到自己"的写法？
pivot_root 系统调用有个硬性要求：
new_root 必须是一个 mount point（挂载点），而不仅仅是一个普通目录。

我们的 merged 目录是怎么来的？看：

```text
/var/lib/mydocker/containers/abc/
├── upper/ ← 普通目录
├── work/ ← 普通目录
└── merged/ ← 父进程在这里挂了 overlay，所以这是个挂载点 ✓
```

理论上 merged 已经是挂载点了（父进程做了 mount -t overlay ... merged），那为什么子进程还要再 bind 一次？

真正的原因：mount namespace 边界 + 传播类型
回顾一下流程：

```text
父进程：

1. 在父 mount namespace 里 mount overlay → merged 成为挂载点
2. fork 子进程（CLONE_NEWNS）
   ↓
   子进程拥有自己的 mount namespace（一开始是父的拷贝）
   ↓
   子进程做 mount("", "/", MS_PRIVATE|MS_REC, ...)
   ↓
   整棵挂载树被改成 private
```

经过 MS_PRIVATE|MS_REC 处理后，merged 这个挂载点在某些内核版本/某些场景下，可能因为传播类型变更或祖先挂载点共享关系等原因，不能直接作为 pivot_root 的 new_root。pivot_root 内部会做一系列检查（其中一条是 new_root 和 put_old 不能在同一个挂载上），有时会失败。

解决办法：bind 自己一次

```go
syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND|syscall.MS_REC, "")
```

这一行的效果是：在当前 mount namespace 里，强制在 newRoot 路径上叠加一个全新的、独立的挂载点。

打个比方：

```text
bind 之前：
merged 是一个"挂载点"，但它的"出身"是父 namespace 复制来的，
和上层挂载点之间有共享关系。

bind 之后（mount --bind merged merged）：
merged 上叠加了一层 bind 挂载，
这一层是"全新的、独立的"挂载，没有任何复杂关系。
pivot_root 一看："好，这就是个干净的挂载点"，通过。
```

这是 runc/Docker 都在用的标准技巧，被叫做"self-bind"。你可以把它理解为："我不管你之前是什么状态，我现在用 bind mount 在你身上盖一层全新的挂载戳，从此你 100% 是个合法挂载点"

#### 三、场景 B：父进程提前把 volume bind 到 merged/<target>

这是回答"为什么子进程的 extraMounts 传 nil 就行"的关键。
问题：volume 怎么进到容器里？
假设用户执行：

```bash
mydocker run -v /home/alice/code:/app alpine sh
```

意思是"把宿主机的 /home/alice/code 映射到容器里的 /app"。

错误的直觉做法：子进程里再挂
你可能想："那就在子进程做完 pivot_root 之后，再 mount --bind /home/alice/code /app 一下不就行了？"

问题来了：子进程已经 pivot_root 完毕，老根（宿主机）已经被 umount 并从视野里抹掉。这时候它根本访问不到 /home/alice/code——这条路径已经不存在于它的 mount namespace 里了！

所以这条路走不通（除非把宿主机 bind 路径在 pivot_root 之前先弄进新根，但那等于本方案）。

正确做法：在父进程里、pivot_root 之前就把 volume "嵌"进 merged
父进程在调用 image.prepareRootfs 之后，紧接着调 rootfs.ApplyBindMounts(merged, mounts)：

```go
func ApplyBindMounts(merged string, mounts []Mount) error {
    for _, m := range mounts {
        ...
        target := filepath.Join(merged, rel)
        ...
        flags := uintptr(syscall.MS_BIND | syscall.MS_REC)
        if err := syscall.Mount(m.Source, target, "", flags, ""); err != nil
```

它做的事情：
执行前（父 mount namespace 里）：

```text
/var/lib/mydocker/containers/abc/merged/ ← overlay 合并出的 alpine 根
├── bin/ etc/ usr/ ...
└── app/ ← 镜像里可能没有这个目录

/home/alice/code/ ← 宿主机上的真实目录
├── main.go
└── README.md
```

执行后：

```text
mkdir merged/app（如果不存在）
mount --bind /home/alice/code merged/app
↓
/var/lib/mydocker/containers/abc/merged/
├── bin/ etc/ usr/
└── app/ ← 现在能看到 main.go README.md
├── main.go
└── README.md
```

然后 fork、子进程 pivot_root

```text
父进程 fork (CLONE_NEWNS)
│
▼
子进程拥有自己的 mount namespace（继承自父，包含刚才的 bind mount）
│
▼
子进程做 pivot_root(merged, .pivot_root)
│
▼ 此刻容器视野里：
/ ← 原来的 merged
├── bin/ etc/ usr/
└── app/ ← bind 进来的内容自动跟着过来
├── main.go
└── README.md
```

关键洞察：bind mount 是 mount namespace 里的一条挂载记录。pivot_root 不是"删除挂载然后重建"，它只是改变了"根"的指向，原本挂载树里的所有挂载点（包括我们刚才挂的 volume）全部都还在，只是路径前缀变了：

之前是 /var/lib/mydocker/containers/abc/merged/app
pivot*root 后是 /app
所以子进程啥都不用做，volume 自动出现在容器里。这就是为什么 extraMounts 传 nil、setup 里 * = extraMounts 直接忽略。

#### 四、对比表：两个 bind mount 干了啥

| 场景                                                 | 代码位置          | 谁来做                    | 目的                                                        |
| ---------------------------------------------------- | ----------------- | ------------------------- | ----------------------------------------------------------- |
| self-bind：mount --bind merged merged                | setup() 第 1 步   | 子进程（pivot_root 之前） | 把 merged 强制变成"干净的挂载点"，满足 pivot_root 前置条件  |
| volume-bind：mount --bind 宿主机路径 merged/容器路径 | ApplyBindMounts() | 父进程（fork 之前）       | 把宿主机目录"嵌"进合并目录，pivot_root 后自动出现在容器视野 |

#### 五、为什么必须父进程提前挂，不能子进程挂？时序图

```text
时间轴 →
父进程：
├─ prepareRootfs: mount overlay → merged 有内容了
├─ ApplyBindMounts: mount --bind /home/alice/code merged/app ★ 关键
├─ fork(CLONE_NEWNS) ──→ 子进程出生，继承挂载树（含 ★）
├─ (cgroup, CNI, pipe...) │
│
子进程：
├─ 等 pipe
├─ rootfs.Setup(merged, nil)
│ ├─ MS_PRIVATE|MS_REC
│ ├─ self-bind merged
│ ├─ pivot_root ← ★ 的挂载点跟着过来了
│ ├─ chdir /
│ ├─ umount /.pivot_root
│ └─ mount /proc /sys /dev
└─ exec 用户命令
现在 ls /app 能看到 main.go
```

如果父进程不挂、想等子进程挂，子进程 pivot_root 后宿主机路径已经看不到了，没法再挂——所以必须父进程提前挂。

#### 一句话总结

self-bind（merged bind 自己）：是一个"挂载点合法化"小技巧，让 pivot_root 的前置条件铁定满足。
volume bind 提前到父进程做：因为子进程 pivot_root 后看不到宿主机路径了，挂不了；提前在父进程挂到 merged/<target>，bind 关系会跟着 mount namespace 继承到子进程，pivot_root 后挂载点路径前缀自动从 merged/app 变成 /app，volume 就"自动出现"在容器里。
如果对 mount namespace 的"挂载继承"、MS_PRIVATE/MS_SHARED/MS_SLAVE 传播规则还想深入，告诉我，我可以画个更详细的例子

## 十、CgroupParent 和 CgroupDriver 的赋值来源

这两个字段在不同入口场景下赋值方式完全不同。mini-docker 有两条主要入口：CLI（mydocker run）和 CRI（K8s 通过 gRPC 调用）。

### 一、CLI 场景（mydocker run）：两个字段都不赋值，用零值

看 run.go 构造 container.Config 的地方：

```go
cfg := container.Config{
    ID:           id,
    Name:         name,
    Rootfs:       mergedRoot,
    Hostname:     o.hostname,
    WorkingDir:   finalWD,
    Cmd:          finalCmd,
    ...
    Resources: cgroup.Resources{
        MemoryBytes:     memBytes,
        ...
    },
```

这里压根没出现 CgroupParent 和 CgroupDriver 字段。Go 结构体未显式赋值的字段保留零值：

CgroupParent → ""（空字符串）
CgroupDriver → ""（cgroup.Driver 底层是 string）
这两个零值会怎么被处理？
进到 cgroup.NewWithConfig 后：

Driver 为空时：会走 driver 自动探测分支（你 cgroup.go 里应该有类似 autoDetectDriver 的逻辑）。

Parent 为空时：在 newCgroupfsManager 里：

```go
func newCgroupfsManager(cfg Config) (Manager, error) {
    parent := strings.TrimPrefix(cfg.Parent, "/")
    path := filepath.Join(cgroupV2Root, parent, cfg.Name)
```

TrimPrefix("", "/") 还是 ""，filepath.Join("/sys/fs/cgroup", "", cfg.Name) 就是 /sys/fs/cgroup/<容器名>——直接挂在 cgroup 根下。

结论：CLI 场景里，容器的 cgroup 路径是 /sys/fs/cgroup/<容器名>，driver 自动探测。普通用户跑 mydocker run 不需要关心这两个参数。

### 二、CRI 场景（K8s/Kubelet 调用）：两个字段都由 K8s 传进来

这才是这两个字段存在的真正原因。

CgroupParent 的赋值链路
起点：Kubelet 通过 gRPC 调 RunPodSandbox，请求里带 LinuxPodSandboxConfig.CgroupParent：

```go
var cgroupParent string
var hostNetwork bool
if req.Config.Linux != nil {
    cgroupParent = req.Config.Linux.CgroupParent
    ...
}
...
    LogDir:       req.Config.LogDirectory,
    Labels:       req.Config.Labels,
    Annotations:  req.Config.Annotations,
    CgroupParent: cgroupParent,
```

→ 存到 sandbox 元数据里：

```go
State:        StateReady,
PID:          1,
NetnsPath:    "/proc/1/ns/net",
CgroupParent: opts.CgroupParent,
```

→ 创建容器时从 sandbox 拷到 store 记录：

```go
State:        store.StateCreated,
CreatedAt:    now,
LogPath:      logPath,
CgroupParent: sb.CgroupParent,
CgroupDriver: string(s.cgroupDriver),
Resources:    resourcesFromCRI(cfg.Linux),
```

→ 启动容器时从 store 记录拷到 container.Config：

```go
LogPath:      rec.LogPath,
CRILog:       true,
JoinNS:       joinNS,
CgroupParent: rec.CgroupParent,
CgroupDriver: cgroup.Driver(rec.CgroupDriver),
```

典型值：Kubelet 发过来的会形如 kubepods/burstable/pod<uid>，最后容器 cgroup 落到 /sys/fs/cgroup/kubepods/burstable/pod<uid>/<容器名>。

CgroupDriver 的赋值链路
起点：mydocker-cri 启动时通过命令行参数传入：

```go
pf.StringVar(&cgroupDriver, "cgroup-driver", "systemd",
    "cgroup driver: systemd or cgroupfs; must match Kubelet's --cgroup-driver")

root.AddCommand(newServeCmd(&socket, &streamingAddr, &streamingBaseURL, &cniConfDir, &cniBinDirs, &cgroupDriver))
```

→ 传给 cri.Server：

```go
if cfg.CgroupDriver == "" {
    cfg.CgroupDriver = cgroup.DriverSystemd
}
rt.cgroupDriver = cfg.CgroupDriver
```

→ 创建容器时写入 store 记录（见上面 c:\project\mini-docker/pkg/cri/container.go:169）→ 启动时拷进 container.Config。

典型值：systemd（默认，与 Kubelet 默认值一致）或 cgroupfs。必须和 Kubelet 配置一致，否则资源限制写错地方导致失效。

### 三、一张总表

| 场景               | CgroupParent 来源                                                                                 | CgroupDriver 来源                                                                                  |
| ------------------ | ------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| CLI (mydocker run) | 不赋值，零值 ""                                                                                   | 不赋值，零值 ""（自动探测）                                                                        |
| CRI (K8s)          | Kubelet 发过来的 LinuxPodSandboxConfig.CgroupParent，经 sandbox → store record → container.Config | mydocker-cri --cgroup-driver 启动参数（默认 systemd），经 server → store record → container.Config |

### 四、为什么这么设计？

CLI 用户只想跑个测试容器，cgroup 放哪、用什么 driver 不重要，零值 + 自动探测最省事。
K8s 必须由 Kubelet 决定：
Kubelet 把所有 Pod 组织成 kubepods/<QoS>/pod<uid> 这样的层级，CRI 实现必须照办，否则 Kubelet 没法做总体资源核算（节点级 reservation、QoS 强制）。
cgroup driver 必须和 Kubelet 一致，否则 systemd 和 cgroupfs 各写一份就乱套了（这是 K8s 排查频率最高的坑之一，你的 scripts/kubelet-integration.md 里也提到了）。
所以这两个字段在 container.Config 里存在，是为 CRI 服务的；CLI 模式下它们是"没用但要留着"的占位。


---

# Runc

## 一、容器的本质

容器 = 一个普通的 Linux 进程 + 三件套包装：

| 组件 | 作用 | 关键技术 |
|------|------|---------|
| **Namespace** | 隔离视野（看不到外面） | pid / net / mnt / uts / ipc / user / cgroup |
| **Cgroup** | 限制资源（CPU、内存等） | /sys/fs/cgroup 下的目录树 |
| **Rootfs** | 切换根目录（看到不同的文件系统） | overlay + pivot_root |

记住：**namespace 管"看到什么"，cgroup 管"能用多少"，rootfs 管"在哪儿"**。

---

## 二、双进程启动模型

```
父进程 (mydocker run)                  子进程 (mydocker init)
─────────────────────                  ─────────────────────
1. os.Pipe() 建管道
2. exec.Command(self, "init")
   + Cloneflags 新 namespace
3. cmd.Start() ──→ 内核 clone ──→ 出生在新 ns 里
                                    阻塞读 pipe ⏸
4. cgroup 创建 + AddProc(子PID)
5. CNI 配网络
6. 通过 pipe 发 payload ──────→ 解除阻塞
7. 返回 Handle                      7a. setns 加入 sandbox ns
                                    7b. sethostname
                                    7c. pivot_root 换根
                                    7d. 挂 /proc /sys /dev
                                    7e. syscall.Exec 变身用户命令
```

**为什么子进程要先卡住？** 因为 cgroup 加 PID、CNI 配网络这些事必须父进程做（子进程在新 PID ns 里看不到自己的真实 PID），但又必须等子进程已经在新 namespace 里之后才有意义。pipe 阻塞读就是同步机制。

---

## 三、关键技术点速记

### 1. 父子通信：FD 3 + 管道

- 父进程：`cmd.ExtraFiles = []*os.File{r}`
- 子进程：通过 `os.NewFile(3, ...)` 拿到管道读端
- Go 在子进程 fork 后用 `dup2` 强制让管道变成 FD 3

### 2. clone flags 的两种用法

| 场景 | 处理 |
|------|------|
| 普通容器：建独立 ns | `Cloneflags |= CLONE_NEWPID|CLONE_NEWNET|...` |
| Pod 容器：加入沙箱 ns | 从 Cloneflags 里 `clearCloneFlag` 去掉对应位，子进程用 `setns` 加入 |

### 3. pivot_root 换根七步

| 步 | 做什么 | 为什么 |
|----|--------|--------|
| 0 | mount("", "/", "", MS_PRIVATE\|MS_REC) | 防止挂载传播泄漏到宿主机 |
| 1 | mount --bind newRoot newRoot | 让 newRoot 成为合法挂载点 |
| 2 | mkdir newRoot/.pivot_root | 给老根准备安放位置 |
| 3 | pivot_root(newRoot, .pivot_root) | 真正换根 |
| 4 | chdir("/") | 把 cwd 也搬到新根 |
| 5 | umount(/.pivot_root) + 删除 | 抹掉宿主机老根，防逃逸 |
| 6 | 挂 /proc /sys /dev | 镜像里这些目录是空的 |

### 4. cgroup v2 操作模式

cgroup 就是文件系统：

| 操作 | 等价 shell |
|------|-----------|
| 创建 cgroup | `mkdir /sys/fs/cgroup/.../<name>` |
| 启用控制器 | `echo "+memory" > .../cgroup.subtree_control`（**父级必须先 enable**）|
| 限制内存 | `echo 100M > memory.max` |
| 限制 CPU | `echo "50000 100000" > cpu.max`（每 100ms 用 50ms = 半核）|
| 加入进程 | `echo <pid> > cgroup.procs` |
| 销毁 cgroup | `rmdir .../<name>`（必须没进程才能删）|

### 5. cgroup 树 vs cgroup namespace

- **cgroup 树**：目录嵌套形成的层级结构，用来表达"组的组"配额（K8s QoS 就靠这个）
- **cgroup namespace**：让容器里 `cat /proc/self/cgroup` 看到 `/` 而不是宿主路径。**不是为了防偷看**，而是让容器内工具（Java/Go runtime）能正确读到自己的 cgroup 文件

### 6. overlay 和 pivot_root 的协作

```
父进程: overlay mount → merged 目录（多层镜像合一）
                         ↓
父进程: ApplyBindMounts → volume 挂到 merged/<target>
                         ↓
父进程: fork(CLONE_NEWNS) → 子进程继承挂载树
                         ↓
子进程: pivot_root(merged) → merged 变成 /，volume 自动出现
```

**两个 bind mount 的不同用途**：

| 类型 | 谁做 | 目的 |
|------|------|------|
| self-bind（merged 绑自己）| 子进程 | 让 merged 100% 是合法挂载点，满足 pivot_root 前置条件 |
| volume-bind（宿主路径绑到 merged/x）| 父进程 | 子进程 pivot_root 后看不到宿主路径，必须父进程提前挂 |

---

## 四、initProcess 的"灵魂替换"

```go
syscall.Exec(bin, payload.Cmd, envSlice)
```

`syscall.Exec` = `execve(2)` 系统调用。**它不是 fork+exec，而是把当前进程的内存映像直接替换掉**：

- PID 不变
- namespace 不变
- cgroup 归属不变
- 但里面跑的代码完全变了（从 mydocker init 变成 /bin/sh 之类）

从这一刻起，这个进程就是用户的程序，Go runtime 完全消失。**容器就"诞生"了**。

---

## 五、CgroupParent / CgroupDriver 的双重身份

| 入口 | CgroupParent | CgroupDriver |
|------|--------------|--------------|
| CLI（mydocker run）| 不赋值，零值 `""` → 落到 `/sys/fs/cgroup/<name>` | 不赋值，自动探测 |
| CRI（K8s）| Kubelet 通过 gRPC 传，形如 `kubepods/burstable/pod<uid>` | mydocker-cri 启动参数 `--cgroup-driver`，必须和 Kubelet 一致 |

**为什么 CRI 必须由 Kubelet 决定？** Kubelet 用 cgroup 树做节点级资源核算和 QoS 强制，CRI 实现必须配合。driver 不一致是 K8s 集群最高频的坑之一。

---

## 六、术语对照速查

| 术语 | 通俗解释 |
|------|---------|
| clone flags | fork 时让内核顺便创建哪些新 namespace 的开关位 |
| pivot_root | 把进程的根目录换成另一个目录（chroot 的安全升级版） |
| setns | 让当前进程"跳进"另一个已存在的 namespace |
| netns | network namespace 简写（独立网卡/路由/iptables） |
| CNI | 容器网络接口标准，给容器配 IP 和网线 |
| cgroup | 资源限制机制，本质是 /sys/fs/cgroup 下的文件系统 |
| FD | File Descriptor，进程里打开资源的整数编号（0=stdin, 1=stdout, 2=stderr）|
| pipe | 内核里的缓冲区，进程间通信 |
| bind mount | 把一个目录"复制挂载"到另一个位置（同一份数据两个入口）|
| overlay | 联合文件系统，把多层只读镜像 + 一层可写层合并成一个目录 |
| detach | 后台模式，父进程启动完就放手 |
| TTY | 终端设备，前台模式下容器 stdio 接到用户终端 |
| payload | 载荷，要传递的数据本身 |
| QoS | Quality of Service，K8s 的资源服务等级（Guaranteed/Burstable/BestEffort）|

---

## 七、一句话回顾全流程

> **父进程用 clone 开一个套着新 namespace 的子进程让它先卡住 → 父进程在外面给它配好 cgroup 和网络 → 通过管道发指令 → 子进程做 pivot_root 换根 → 然后 exec 成用户命令。容器就跑起来了。**

整个过程的精髓：**让用户进程一启动时，所有的隔离（ns）、限制（cgroup）、视野（rootfs）、网络都已就绪**。这就是 Docker、runc、containerd、mini-docker 共同遵循的设计哲学。
