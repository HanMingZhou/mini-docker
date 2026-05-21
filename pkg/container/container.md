# 容器到底是什么
## 一、容器不是虚拟机。它本质就是一个普通的 Linux 进程，但被 Linux 内核用三件事"包"了起来：
Namespace（命名空间）：让这个进程"看不到"宿主机的其他东西。比如它有自己的 PID 1，自己的网卡，自己的主机名、挂载点。
一共 7 种 namespace：pid / net / mnt / uts / ipc / user / cgroup。
Cgroup（控制组）：限制这个进程能用多少 CPU、内存。
Rootfs（根文件系统）：通过 pivot_root 把它的 / 换成另一个目录（比如解压好的 alpine 镜像），让它以为整个文件系统都不一样了。

## 二、这个文件的整体流程（双进程模型）
Docker、runc 都用同一种套路：父进程 + init 子进程。

父进程 (mydocker run)
│
│ 1. os.Pipe() 创建一根管道
│ 2. exec.Command("/proc/self/exe", "init") 启动自己
│    并设置 Cloneflags = CLONE_NEWPID|NEWNS|NEWNET|...
│ 3. cmd.Start()  ← 内核此刻 clone 出子进程，并把它放进新 namespace
│
├──→ 子进程（"init" 子命令）
│        阻塞在 read(pipe) ← 等父进程发指令
│
│ 4. 父进程：配置 cgroup（把子进程 PID 加进去）
│ 5. 父进程：配置网络（调 CNI，对子进程的 netns 做手脚）
│ 6. 父进程：通过 pipe 把 payload（rootfs/cmd/env...）发给子进程
│
└──→ 子进程被唤醒：
sethostname → rootfs.Setup(pivot_root) → chdir → exec 用户命令

因为有些事情必须父进程做（比如往 cgroup 里加 PID、调 CNI 设置网络），但又必须等子进程已经在新 namespace 里之后才能做。
所以子进程要先"卡住"，等父进程在外面把环境准备好，再放它走。这个"卡住"就是靠管道阻塞读实现的。

## 三、逐段讲解
## 1. initPipeFD = 3 和 initPayload
// 父子之间通过一对 pipe 传递 "启动指令"（JSON 编码的 initPayload）。
// FD 3 = pipe 的读端（由父进程通过 ExtraFiles 注入子进程）。
const initPipeFD = 3
FD（File Descriptor）：进程里每个打开的文件/管道/socket 都有一个整数编号。0=stdin，1=stdout，2=stderr，从 3 开始就是用户自己的。
父进程通过 cmd.ExtraFiles = []*os.File{r} 把管道读端塞给子进程，Go 规定它在子进程里就是 FD 3。
initPayload 是 JSON 结构，把"要执行什么命令、用哪个 rootfs"等参数从父传给子。
   
## 2. start() —— 父进程入口
### (a) 参数校验 + 起 cgroup 名字（34–51 行）
   不重要，跳过。

### (b) 创建管道、计算 clone flags（54–66 行）
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

### (c) 启动 init 子进程（68–72 行）
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

### (d) stdio 重定向（74–113 行）
detach 模式：日志写入文件；前台 tty 模式：连到当前终端。逻辑直白。

### (e) cmd.Start() —— 触发 fork（115 行）
这一刻，内核 clone() 出子进程，子进程立刻在新 namespace 里运行 initProcess()，第一行就是 io.ReadAll(pipe) → 阻塞等待。

### (f) 配置 cgroup（120–141 行）
```go 
if err := cg.AddProc(cmd.Process.Pid); err != nil {
```
创建 cgroup（写文件，比如 /sys/fs/cgroup/mydocker/<name>/）。
Apply 写资源限制（内存上限、CPU 配额）。
AddProc 把子进程 PID 写进 cgroup.procs 文件 —— 这一步必须在父进程做，因为子进程在新 PID namespace 里看不到自己的"真实 PID"。

### (g) 网络配置（145–156 行）
// 网络配置：此时 cmd 已启动，netns 存在；init 子进程正阻塞在 pipe 读上，
// 还没 exec 用户命令。这是调用 CNI ADD 的最佳时机。
子进程已经有了独立的 netns（在 /proc/<pid>/ns/net），但里面只有 lo（loopback），没有真正的网卡。
父进程调 CNI 插件（比如 bridge），让插件在子进程的 netns 里造一对 veth、配 IP、加路由。
之所以必须现在做：子进程还没 exec，netns 是空的、安全的。等用户命令跑起来再改网络就晚了。

### (h) 发 payload，子进程被唤醒（158–172 行）
json.NewEncoder(w).Encode(payload) 把启动参数写进管道，子进程的 ReadAll 立刻拿到数据返回，往下走。

### (i) 返回 Handle
Handle 是父进程拿在手里的"句柄"，里面有 cmd（用于 Wait）、cgroup（用于销毁）、PID、cgroup 路径、容器 IP。

## 3. wait() 和 release() 
```go 
func (h *Handle) wait() (int, error) {
err := h.cmd.Wait()
...
if derr := h.cgroup.Destroy(); derr != nil {
```
Wait() 阻塞到容器进程退出，然后关闭日志文件 + 销毁 cgroup。
release() 用在 detach 模式：父进程不 wait，直接放手，让子进程"游离"在系统里（后续 ps/logs 通过 PID 找它）

## 4. initProcess() —— 子进程入口（在新 namespace 里执行）
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

## 5. joinNamespaces / nsCloneFlag / clearCloneFlag
辅助函数：
joinNamespaces：打开 /proc/<sandbox-pid>/ns/net 这类文件，调 setns() 加入。
nsCloneFlag：字符串名 → 内核常量（"net" → CLONE_NEWNET）。
clearCloneFlag：从 flags 里把某个位清除掉（用于"不创建、要加入"的场景）。&^ 是 Go 的 AND-NOT 运算符。

## 四、几个最容易卡住的"专业词"翻译
术语	               通俗解释
clone flags	         告诉内核 fork 子进程时顺便创建哪些新 namespace 的开关位
pivot_root	         把进程的"根目录 /"切换到另一个目录，类似豪华版 chroot
setns	               让当前进程"跳进"另一个已存在的 namespace
netns	               network namespace 的简写，一套独立的网卡/路由/iptables
CNI	               容器网络接口标准，K8s 用它给容器配网
cgroup	            内核的资源限制机制，本质是 /sys/fs/cgroup 下的文件系统
stdio	               标准输入(0)/输出(1)/错误(2) 三个文件描述符
pipe	               内核里的一段缓冲区，一端写一端读，用于进程间通信
detach	            后台模式，父进程启动完就撒手，不等子进程
TTY	               终端设备，前台模式下容器的 stdio 接到你的终端，能交互
payload	            "载荷"，就是要传过去的数据本身

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

fork 之前：把 r 这个 *os.File 加到一个内部数组里。
fork 之后、exec 之前（在子进程里执行）：调 dup2(原FD, 目标FD) 系统调用，把这个 fd 强制复制到一个指定编号。
编号规则：cmd.ExtraFiles[i] 在子进程里就是 FD 3 + i。
为什么从 3 开始？因为 0/1/2 被 stdin/stdout/stderr 占了，3 是第一个可用的"自定义" FD。

用一张图说明
父进程：
os.Pipe() 返回 r, w  →  内核分配 FD 比如 r=7, w=8
cmd.ExtraFiles = []*os.File{r}

cmd.Start() 触发 fork：
┌─ fork 出子进程，FD 全部复制过来（子进程里 r 也是 7）
│
└─ Go 在子进程里立刻调 dup2(7, 3)，让 FD 3 指向同一根管道
然后 close(7)，再 execve(...)

子进程里：
os.NewFile(uintptr(3), "init-pipe")  ← 直接拿 FD 3 包成 *os.File

父进程：
cmd.ExtraFiles = []*os.File{r}

子进程：
pipe := os.NewFile(uintptr(initPipeFD), "init-pipe")
父子之间靠 initPipeFD = 3 这个约定好的常量对上号。这是一种朴素但极其稳健的"协议"，runc/Docker 也是同样做法。

## pivot_root 把 / 替换成镜像目录是什么意思？
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

步骤	      代码	            干了什么	      为什么要做
0	      Mount("", "/", "", MS_PRIVATE|MS_REC, "")	      把当前 mount namespace 里所有挂载标记为"私有"	      否则容器里的挂载会传播回宿主机，污染主机
1	      Mount(newRoot, newRoot, "", MS_BIND|MS_REC, "")	      把 newRoot 目录 bind 到自己	      pivot_root 要求 new_root 必须是一个挂载点。普通目录不行，所以自己 bind 一下让它"变成"挂载点
2	      MkdirAll(newRoot/.pivot_root)	      在新根里建个子目录	      用来临时安放老根（put_old 必须在 new_root 内部）
3	      PivotRoot(newRoot, pivotDir)	      切换！新根成为 /，老根挂到 /.pivot_root	      核心一步
4	      Chdir("/")	      把工作目录切到新的 /	      否则当前目录还指向老根的某个位置
5	      Unmount("/.pivot_root", MNT_DETACH) + Remove	卸载老根，彻底看不见宿主机	安全：容器里就算是 root 也访问不到宿主机文件
6	      mountDefaults()	挂 /proc /sys /dev	容器里这些目录是空的，得挂上才能用 ps、cat /proc/...

/proc 为什么必须重新挂？
/proc 是个伪文件系统，内容由内核根据"当前 pid namespace"动态生成。容器在新 PID namespace 里，必须在容器里重新 mount -t proc proc /proc，这样 ps 才只看到容器自己的进程（PID 1, 2, ...），看不到宿主机的几千个进程。

### 一句话总结
pivot_root = "把进程的根目录原地换成另一个目录，并把老的根从视野里抹掉"。这是容器实现"文件系统隔离"的关键一步，光靠 namespace 是不够的，还得真的换根。

## cgroup 详解
### 一、cgroup 是什么
cgroup（control group）= Linux 内核提供的"资源限制 + 统计"机制。

它跟 namespace 是两套独立的东西：
namespace 管"看到什么"（隔离视野）
cgroup 管"能用多少"（限制资源）
容器 = namespace + cgroup + rootfs 三件套。

### 二、cgroup v2 的核心模型：一棵目录树
打开 /sys/fs/cgroup，它本身就是个挂载点（类型是 cgroup2fs）。这个目录就是整棵 cgroup 树的根。

/sys/fs/cgroup/                 ← 根 cgroup（包含系统所有进程）
├── cgroup.procs                ← 哪些 PID 属于本组（写文件 = 加入）
├── cgroup.controllers          ← 本层可用的控制器：cpu memory io ...
├── cgroup.subtree_control      ← 给"子目录"启用哪些控制器（关键！）
├── memory.max                  ← 内存上限（写一个数字就是字节数）
├── cpu.max                     ← CPU 配额："quota period"
│
├── mydocker/                   ← 自己 mkdir 出来的子 cgroup（=parent）
│   ├── cgroup.subtree_control
│   └── abc123/                 ← 又一个子目录（=容器自己的叶子 cgroup）
│       ├── cgroup.procs        ← 把容器进程 PID 写这里
│       ├── memory.max          ← 写 "104857600" = 100MB 上限
│       └── cpu.max             ← 写 "50000 100000" = 给 50% CPU
关键点：cgroup 就是文件系统。 创建 cgroup = mkdir，限制资源 = 往文件里 echo 一个数字，加入进程 = 把 PID echo 到 cgroup.procs。没有什么神奇的 API

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
// 逐级从 cgroupV2Root 到 dir，一层层 enable
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


### 八、overlay 和 pivot_root 的关系：两件事，缺一不可
#### 一、用一个比喻先建立直觉
把容器想象成一个"假装住进新房子"的过程：

步骤	       类比	                                 对应技术
造房子	    把一堆建材（镜像层）拼出一栋完整的房子	   overlayfs（产出 merged 目录）
搬进去住	    让你的"户口"指向这栋新房子，旧家从此看不见	pivot_root
merged      只是宿主机文件系统里的一个普通目录而已（路径形如 /var/lib/mydocker/containers/abc/merged）。如果不做 pivot_root，容器进程的 / 还是宿主机的 /——它依然能 ls /etc、cat /etc/shadow，看到宿主机的所有东西

pivot_root 才是把这个目录挂上"根"的位置，让容器进程从此"看不见"宿主机

#### 二、两个文件分别在做什么
overlay_linux.go 的工作（在父进程里）
目标：拼出一份"看起来像 alpine 镜像"的目录树。
执行前：
/var/lib/mydocker/containers/abc/
├── upper/    （空）
├── work/     （空）
└── merged/   （空）

/var/lib/mydocker/images/alpine/
├── layer1/content/   （只读，里面有 /bin /etc /usr ...）
├── layer2/content/
└── layer3/content/

mount("overlay", merged, "overlay", 0, "lowerdir=l3:l2:l1,upperdir=upper,workdir=work")

执行后：
merged/  ← 现在 ls 它，看到的就是 alpine 完整的文件系统
├── bin/  etc/  usr/  var/  ...
重点：merged 仍然是宿主机文件系统树的一个分支。它的绝对路径是 /var/lib/mydocker/containers/abc/merged。容器进程如果只走到这一步，它的 / 还是宿主机的根。


pivot_root 之前（容器进程视角）：
/
├── bin/      ← 宿主机的 /bin
├── etc/      ← 宿主机的 /etc（能看到宿主机的所有秘密！）
├── var/lib/mydocker/containers/abc/merged/   ← 我们的 alpine 在这儿
│   ├── bin/  etc/  ...
└── ...

pivot_root 之后：
/
├── bin/      ← 现在是 alpine 的 /bin
├── etc/      ← 现在是 alpine 的 /etc
└── ...
（宿主机的根从视野里消失，访问不到了）
#### 三、为什么必须分两步？能不能合一步？
不能。 因为 namespace 和挂载操作有约束：

overlay mount 必须在父进程做（在 start() 流程里早于 fork init 子进程的某个时机），因为它涉及读取镜像元数据、创建目录等准备工作。
pivot_root 必须在子进程做，且子进程必须已经在新的 mount namespace 里——否则它的 pivot_root 会把宿主机的根也换掉，灾难性后果。
所以流程必然是：
父进程：
1. overlay mount → 产出 merged 目录
2. clone(CLONE_NEWNS|...)  fork 出子进程，子进程进入新 mount namespace
3. 通过 pipe 把 merged 路径告诉子进程

子进程（在新 mount namespace 里）：
4. pivot_root(merged, .pivot_root)  ← 用 merged 当新根
5. exec 用户命令
   四、串起整个项目的全景图
   ┌─────────────────────────────────────────────────────────────┐
   │ 父进程（mydocker run）                                       │
   │                                                              │
   │  ① image 包：prepareRootfs()                                │
   │     └─ syscall.Mount("overlay", merged, "overlay", ...)     │
   │        把多层镜像合成一个 merged 目录                          │
   │                                                              │
   │  ② container 包：start()                                    │
   │     ├─ os.Pipe()                                            │
   │     ├─ exec.Command("/proc/self/exe", "init")               │
   │     │     SysProcAttr.Cloneflags = NEWNS|NEWPID|NEWNET|...  │
   │     ├─ cmd.Start()  ← 内核 clone 出子进程                    │
   │     ├─ cgroup.Apply / AddProc(子进程PID)                     │
   │     ├─ CNI 配网络                                            │
   │     └─ 通过 pipe 发 payload{Rootfs: merged, Cmd: ...}       │
   └─────────────────────────────────────────────────────────────┘
   │ pipe
   ▼
   ┌─────────────────────────────────────────────────────────────┐
   │ 子进程（mydocker init，在新 namespace 里）                    │
   │                                                              │
   │  ③ container 包：initProcess()                              │
   │     ├─ 读 pipe 拿 payload                                    │
   │     └─ rootfs.Setup(merged)  ← 这里发生 pivot_root          │
   │                                                              │
   │  ④ rootfs 包：setup()                                       │
   │     ├─ Mount("/", MS_PRIVATE|MS_REC)  防挂载泄漏             │
   │     ├─ Mount(merged, merged, MS_BIND) 让 merged 变成挂载点   │
   │     ├─ PivotRoot(merged, merged/.pivot_root) ← 换根！        │
   │     ├─ Chdir("/")                                            │
   │     ├─ Unmount("/.pivot_root")  抹掉宿主机老根                │
   │     └─ mountDefaults() 挂 /proc /sys /dev                   │
   │                                                              │
   │  ⑤ syscall.Exec(用户命令)                                   │
   │     此刻进程"摇身一变"成了 sh / nginx / 你跑的任何东西        │
   └─────────────────────────────────────────────────────────────┘
一句话：
overlay 准备的是素材（一个长得像镜像的目录）
pivot_root 做的是生效（把这个目录真正挂到容器进程的"根"位置）


#### 小结
overlay 的 merged = 把镜像各层合并出的"看起来像 alpine 根目录的素材目录"，但它仍然位于宿主机文件系统树里。
pivot_root = 把这个素材目录真正挂到容器进程的 /，并抹掉宿主机老根，让容器进程从此与宿主机文件系统视野隔离。
两步顺序：父进程先做 overlay → fork 出带新 mount namespace 的子进程 → 子进程做 pivot_root。少任何一步容器都无法正确隔离。


#### payload.Rootfs 的没赋值
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
run.go:
  mergedRoot, _ = image.prepareRootfs(...)        ← overlay 合成 merged 目录
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
  PivotRoot(newRoot, ...)        ← newRoot 就是 merged
所以之前讲的"overlay 准备素材，pivot_root 把素材挂为根"在代码里就是通过这个 Rootfs 字段串起来的。

#### extraMounts 传 nil
看 setup 函数最后一段注释（：
我们改成"在父进程里 bind mount 到 merged/"，这里 extraMounts 就无事可做。
也就是说：volume（-v 参数）实际上是在父进程里、overlay 完成之后、fork 之前，由 rootfs.ApplyBindMounts(merged, mounts) 直接 bind 到 merged/<target> 路径下。这样等子进程 pivot_root 后，volume 自然就出现在容器视野里了。
子进程这一步就什么都不用干，所以传 nil 占位，函数里直接 _ = extraMounts 忽略。这是为了保持 API 对称（万一以后想换回"子进程内挂载"模式），不算冗余设计。

#### setup 函数详细讲解
```go
func setup(newRoot string, extraMounts []Mount) error 
```
我把它拆成 7 个步骤，每一步讲做了什么 + 为什么必须这么做。

##### Step 0：把整棵挂载树标为"私有"
// 0. 把整棵树改成 private，否则 pivot_root 里的 mount 会泄漏到宿主机
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

##### Step 1：把 newRoot bind mount 到自己
// 1. bind mount newRoot 到自身，满足 pivot_root 的要求
```go
if err := syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND|syscall.MS_REC, ""); err != nil 
```
pivot_root 系统调用的硬性要求
man page 里写得很清楚：new_root 必须是一个挂载点（mount point），不能是普通目录。

但 merged 此刻只是 overlay 挂载里的一个目标目录，对父进程来说它是挂载点，可经过 MS_PRIVATE|MS_REC 之后，到底它是不是被识别成"独立挂载点"会有歧义。最稳妥的办法是：
bind mount 它到自己——mount --bind /a/b /a/b。
这是一个特殊技巧：把目录 bind 到自己路径上，等于"强制让这个路径成为一个挂载点"，不改变内容。这样 pivot_root 的前置条件就满足了。

##### Step 2：在 newRoot 里建一个 .pivot_root 子目录
```go
pivotDir := filepath.Join(newRoot, ".pivot_root")
if err := os.MkdirAll(pivotDir, 0700); err != nil 
```
pivot_root(new_root, put_old) 调用要求 put_old 路径必须在 new_root 内部（这样老根才能被"塞"进新根的某个子目录里）。
我们建 merged/.pivot_root 作为容纳老根的临时位置。0700 权限只让 root 访问，反正一会儿就删了。

##### Step 3：执行 pivot_root
```go 
if err := syscall.PivotRoot(newRoot, pivotDir); err != nil 
```
这是核心一步。系统调用语义：
调用前：
  /                ← 宿主机老根
  └── merged/      ← 即将成为新根的目录
      └── .pivot_root/
 
调用后：
  /                ← 现在是原来的 merged
  └── .pivot_root/ ← 老根被塞到这里
      ├── bin/   etc/   home/   ...   （宿主机的 / 内容全在这）
注意从这一刻起，进程视野里的 / 就变了。它再 ls /etc 看到的就是 alpine 的 /etc，而不是宿主机的。

##### Step 4：Chdir("/")
```go
if err := syscall.Chdir("/"); err != nil 
```
pivot_root 不会自动改变进程的 cwd（current working directory）。如果调用前 cwd 是 /some/path，那它现在指向的还是老根上的某个 inode，并没指向新根。这会导致后续相对路径全错。

所以显式 chdir("/") 把 cwd 也搬到新根。

##### Step 5：卸载并删除老根
```go
const oldRoot = "/.pivot_root"
if err := syscall.Unmount(oldRoot, syscall.MNT_DETACH); err != nil 
```
/.pivot_root 现在挂的是宿主机老根。如果不卸载，容器 root 用户可以 cd /.pivot_root && ls，宿主机文件系统全暴露——这是容器逃逸级别的安全问题。
MNT_DETACH（lazy unmount）：即使有进程还在使用这个挂载点（理论上不可能，但保险），也允许"延迟卸载"——立刻从命名空间移除，等没人用时再真清理。
然后 os.Remove(oldRoot) 删掉那个空目录，连入口都不留。
执行完后，容器进程完全访问不到宿主机文件系统了。

##### Step 6：挂载 /proc /sys /dev
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
挂载	类型	作用
/proc	proc（伪文件系统）	内核动态生成的进程信息。容器在新 PID namespace 里，必须重挂 /proc，这样 ps 才只看到容器自己的进程
/sys	sysfs	内核对象树（设备、模块等）。挂只读，容器不该乱改
/dev	tmpfs（内存文件系统）	设备节点目录。先挂个空 tmpfs，常用设备（/dev/null、/dev/zero）一般再 mknod 或 bind 进来（你这个项目目前简化处理）

flag 含义：
MS_NOSUID：忽略 setuid/setgid 位（防止容器里通过 /proc 之类提权）
MS_NOEXEC：这个文件系统上的文件不可执行
MS_NODEV：忽略其中的设备节点
MS_RDONLY：只读挂载
MS_STRICTATIME：每次访问都更新 atime（POSIX 行为）
这些都是安全加固，业界通用做法。

##### Step 7：忽略 extraMounts（已经在父进程里挂好了）
```go
_ = extraMounts
return nil
```
如前面解释：volume 是父进程在 overlay 之后、fork 之前用 ApplyBindMounts(merged, mounts) 挂到 merged/<target> 的。pivot_root 后这些挂载就自然出现在新根里。子进程这里啥都不用做。

一张表总结整个 setup
步骤	一句话	关键 syscall
0	让挂载操作不泄漏到宿主机	mount("", "/", "", MS_PRIVATE|MS_REC, "")
1	让 newRoot 成为合法挂载点	mount(newRoot, newRoot, "", MS_BIND|MS_REC, "")
2	准备老根的安放位置	mkdir(newRoot/.pivot_root)
3	真正切根	pivot_root(newRoot, .pivot_root)
4	切 cwd 到新根	chdir("/")
5	抹掉老根，防逃逸	umount("/.pivot_root", MNT_DETACH) + rmdir
6	挂 /proc /sys /dev	mount(...) × 3
7	volume 已在父进程挂好，跳过	—
完成这 7 步，子进程的"文件系统视野"就是一个干净、隔离、配齐了 /proc 的容器世界，下一步 syscall.Exec 就可以让用户命令在里面跑了。

##### 两个"bind mount"的细节深挖
这两个点都涉及到 bind mount，但用途完全不同。我先把 bind mount 这个概念讲透，然后两个场景就好理解了。

######  一、先把 bind mount 讲清楚
普通 mount vs bind mount
普通 mount：把一个设备/文件系统挂到目录上。
```bash
mount -t ext4 /dev/sda1 /mnt        # 把 sda1 这块盘挂到 /mnt
mount -t proc proc /proc            # 把 proc 伪文件系统挂到 /proc
bind mount：把一个已存在的目录/文件"复制"挂到另一个位置。不复制数据，只是让两个路径指向同一份内容。
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
######  二、场景 A：mount --bind /merged /merged（把目录 bind 到自己）
为什么有这种"自己 bind 到自己"的写法？
pivot_root 系统调用有个硬性要求：
new_root 必须是一个 mount point（挂载点），而不仅仅是一个普通目录。

我们的 merged 目录是怎么来的？看：
/var/lib/mydocker/containers/abc/
├── upper/         ← 普通目录
├── work/          ← 普通目录
└── merged/        ← 父进程在这里挂了 overlay，所以这是个挂载点 ✓
理论上 merged 已经是挂载点了（父进程做了 mount -t overlay ... merged），那为什么子进程还要再 bind 一次？

真正的原因：mount namespace 边界 + 传播类型
回顾一下流程：
父进程：
  1. 在父 mount namespace 里 mount overlay → merged 成为挂载点
  2. fork 子进程（CLONE_NEWNS）
        ↓
      子进程拥有自己的 mount namespace（一开始是父的拷贝）
        ↓
      子进程做 mount("", "/", MS_PRIVATE|MS_REC, ...)
        ↓
      整棵挂载树被改成 private
经过 MS_PRIVATE|MS_REC 处理后，merged 这个挂载点在某些内核版本/某些场景下，可能因为传播类型变更或祖先挂载点共享关系等原因，不能直接作为 pivot_root 的 new_root。pivot_root 内部会做一系列检查（其中一条是 new_root 和 put_old 不能在同一个挂载上），有时会失败。

解决办法：bind 自己一次
```go
syscall.Mount(newRoot, newRoot, "", syscall.MS_BIND|syscall.MS_REC, "")
```
这一行的效果是：在当前 mount namespace 里，强制在 newRoot 路径上叠加一个全新的、独立的挂载点。

打个比方：
bind 之前：
  merged 是一个"挂载点"，但它的"出身"是父 namespace 复制来的，
  和上层挂载点之间有共享关系。
 
bind 之后（mount --bind merged merged）：
  merged 上叠加了一层 bind 挂载，
  这一层是"全新的、独立的"挂载，没有任何复杂关系。
  pivot_root 一看："好，这就是个干净的挂载点"，通过。
这是 runc/Docker 都在用的标准技巧，被叫做"self-bind"。你可以把它理解为："我不管你之前是什么状态，我现在用 bind mount 在你身上盖一层全新的挂载戳，从此你 100% 是个合法挂载点"

######  三、场景 B：父进程提前把 volume bind 到 merged/<target>
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
  /var/lib/mydocker/containers/abc/merged/    ← overlay 合并出的 alpine 根
  ├── bin/  etc/  usr/  ...
  └── app/                                    ← 镜像里可能没有这个目录
 
  /home/alice/code/                            ← 宿主机上的真实目录
  ├── main.go
  └── README.md
 
执行后：
  mkdir merged/app（如果不存在）
  mount --bind /home/alice/code  merged/app
       ↓
  /var/lib/mydocker/containers/abc/merged/
  ├── bin/  etc/  usr/
  └── app/                                    ← 现在能看到 main.go README.md
      ├── main.go
      └── README.md
然后 fork、子进程 pivot_root
父进程 fork (CLONE_NEWNS)
   │
   ▼
子进程拥有自己的 mount namespace（继承自父，包含刚才的 bind mount）
   │
   ▼
子进程做 pivot_root(merged, .pivot_root)
   │
   ▼ 此刻容器视野里：
/                                ← 原来的 merged
├── bin/  etc/  usr/
└── app/                         ← bind 进来的内容自动跟着过来
    ├── main.go
    └── README.md
关键洞察：bind mount 是 mount namespace 里的一条挂载记录。pivot_root 不是"删除挂载然后重建"，它只是改变了"根"的指向，原本挂载树里的所有挂载点（包括我们刚才挂的 volume）全部都还在，只是路径前缀变了：

之前是 /var/lib/mydocker/containers/abc/merged/app
pivot_root 后是 /app
所以子进程啥都不用做，volume 自动出现在容器里。这就是为什么 extraMounts 传 nil、setup 里 _ = extraMounts 直接忽略。

######  四、对比表：两个 bind mount 干了啥
场景	代码位置	谁来做	目的
self-bind：mount --bind merged merged	setup() 第 1 步	子进程（pivot_root 之前）	把 merged 强制变成"干净的挂载点"，满足 pivot_root 前置条件
volume-bind：mount --bind 宿主机路径 merged/容器路径	ApplyBindMounts()	父进程（fork 之前）	把宿主机目录"嵌"进合并目录，pivot_root 后自动出现在容器视野
######  五、为什么必须父进程提前挂，不能子进程挂？时序图
时间轴 →
父进程：
├─ prepareRootfs:  mount overlay → merged 有内容了
├─ ApplyBindMounts: mount --bind /home/alice/code  merged/app  ★ 关键
├─ fork(CLONE_NEWNS)  ──→  子进程出生，继承挂载树（含 ★）
├─ (cgroup, CNI, pipe...)                            │
                                                     │
                                              子进程：
                                              ├─ 等 pipe
                                              ├─ rootfs.Setup(merged, nil)
                                              │   ├─ MS_PRIVATE|MS_REC
                                              │   ├─ self-bind merged
                                              │   ├─ pivot_root           ← ★ 的挂载点跟着过来了
                                              │   ├─ chdir /
                                              │   ├─ umount /.pivot_root
                                              │   └─ mount /proc /sys /dev
                                              └─ exec 用户命令
                                                 现在 ls /app 能看到 main.go
如果父进程不挂、想等子进程挂，子进程 pivot_root 后宿主机路径已经看不到了，没法再挂——所以必须父进程提前挂。

######  一句话总结
self-bind（merged bind 自己）：是一个"挂载点合法化"小技巧，让 pivot_root 的前置条件铁定满足。
volume bind 提前到父进程做：因为子进程 pivot_root 后看不到宿主机路径了，挂不了；提前在父进程挂到 merged/<target>，bind 关系会跟着 mount namespace 继承到子进程，pivot_root 后挂载点路径前缀自动从 merged/app 变成 /app，volume 就"自动出现"在容器里。
如果对 mount namespace 的"挂载继承"、MS_PRIVATE/MS_SHARED/MS_SLAVE 传播规则还想深入，告诉我，我可以画个更详细的例子



#### CgroupParent 和 CgroupDriver 的赋值来源
这两个字段在不同入口场景下赋值方式完全不同。mini-docker 有两条主要入口：CLI（mydocker run）和 CRI（K8s 通过 gRPC 调用）。

##### 一、CLI 场景（mydocker run）：两个字段都不赋值，用零值
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

#### 二、CRI 场景（K8s/Kubelet 调用）：两个字段都由 K8s 传进来
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

#### 三、一张总表
场景	CgroupParent 来源	CgroupDriver 来源
CLI (mydocker run)	不赋值，零值 ""	不赋值，零值 ""（自动探测）
CRI (K8s)	Kubelet 发过来的 LinuxPodSandboxConfig.CgroupParent，经 sandbox → store record → container.Config	mydocker-cri --cgroup-driver 启动参数（默认 systemd），经 server → store record → container.Config

#### 四、为什么这么设计？
CLI 用户只想跑个测试容器，cgroup 放哪、用什么 driver 不重要，零值 + 自动探测最省事。
K8s 必须由 Kubelet 决定：
Kubelet 把所有 Pod 组织成 kubepods/<QoS>/pod<uid> 这样的层级，CRI 实现必须照办，否则 Kubelet 没法做总体资源核算（节点级 reservation、QoS 强制）。
cgroup driver 必须和 Kubelet 一致，否则 systemd 和 cgroupfs 各写一份就乱套了（这是 K8s 排查频率最高的坑之一，你的 scripts/kubelet-integration.md 里也提到了）。
所以这两个字段在 container.Config 里存在，是为 CRI 服务的；CLI 模式下它们是"没用但要留着"的占位。






cgroup 树是什么
cgroup 在内核里就是一棵树——不是比喻，而是字面意义上的树形结构。它直接以目录树的形式暴露在 /sys/fs/cgroup/ 下，目录就是树的节点。

一、最直观：去看一眼
在你 Linux 机器上敲：

bash
$ ls /sys/fs/cgroup/
cgroup.controllers   cgroup.procs       cpu.stat         memory.stat
cgroup.max.depth     cgroup.subtree_control   ...
init.scope/          system.slice/      user.slice/
每个子目录就是一个 cgroup（节点）。子目录里又能有子目录，形成递归结构：

bash
$ tree -L 3 /sys/fs/cgroup/
/sys/fs/cgroup/                              ← 根 cgroup
├── system.slice/                            ← systemd 系统服务都挂这下面
│   ├── docker.service/
│   ├── nginx.service/
│   ├── kubelet.service/
│   └── ssh.service/
├── user.slice/                              ← 用户登录会话挂这下面
│   └── user-1000.slice/
│       └── session-3.scope/
├── kubepods.slice/                          ← K8s Pod 都挂这下面
│   ├── kubepods-besteffort.slice/
│   ├── kubepods-burstable.slice/
│   │   └── kubepods-burstable-pod_abc123.slice/   ← 一个 Pod
│   │       ├── cri-containerd-xxx.scope/          ← 容器 1
│   │       └── cri-containerd-yyy.scope/          ← 容器 2
│   └── kubepods-guaranteed.slice/
└── init.scope/                              ← systemd PID 1 自己
这就是 cgroup 树。/sys/fs/cgroup/ 是根，每往下一层目录就是树里的一个子节点。

二、每个节点里有什么？
随便进一个目录看：

bash
$ ls /sys/fs/cgroup/system.slice/nginx.service/
cgroup.controllers       cpu.max          memory.current
cgroup.events            cpu.stat         memory.max
cgroup.freeze            cpu.weight       memory.peak
cgroup.procs             io.max           memory.stat
cgroup.subtree_control   io.stat          pids.current
cgroup.threads           memory.events    pids.max
一个 cgroup 节点里有两类文件：

1. 配置文件（你写它，设置限制）

bash
$ echo $((100*1024*1024)) > memory.max     # 限内存 100MB
$ echo "50000 100000" > cpu.max            # 限 CPU 50%
2. 状态文件（你读它，看当前情况）

bash
$ cat memory.current      # 当前用了多少内存
$ cat cpu.stat            # CPU 累计用量
$ cat cgroup.procs        # 哪些 PID 在这个 cgroup 里
三、树形结构的意义：限制会向下传递
cgroup 是树不是平铺，是因为限制要继承。比如：

/sys/fs/cgroup/kubepods.slice/                memory.max = 8GB    ← K8s 总共 8GB
└── kubepods-burstable.slice/                  memory.max = 6GB   ← burstable 类 6GB
    └── kubepods-burstable-pod_abc.slice/      memory.max = 1GB   ← 这个 Pod 1GB
        └── cri-containerd-xxx.scope/          memory.max = 512M  ← 容器 512MB
子节点的限制只能比父节点更严：容器最多 512MB，是因为它在 1GB 的 Pod 节点下，又在 6GB 的 burstable 节点下。任何祖先满了，整个子树都要让步——所以 K8s 用这个层级实现 QoS 调度（Guaranteed > Burstable > BestEffort）。

进程跑起来后只能属于树上的一个节点（cgroup v2 单一层级），那个节点和它所有祖先的限制都对它生效。

四、为什么叫"树"？因为它就是一棵树
抽象地画出来：

                 根 (/sys/fs/cgroup)
                /        |          \
       system.slice  user.slice   kubepods.slice
       /     |               \         |
docker     nginx            ssh    burstable.slice
.service   .service         .service     |
                                       pod_abc.slice
                                       /         \
                               container-1     container-2
根 = /sys/fs/cgroup/
节点 = 每个子目录
叶子 = 通常是真正跑进程的那一层（service / scope）
进程 = 挂在某个叶子节点的 cgroup.procs 文件里
这棵树由谁建？主要是 systemd（系统服务、用户会话）和 容器运行时（containerd / mydocker-cri）创建相应子树。本项目对应的代码：

container_linux.go:121-145
// 创建 cgroup（写文件，比如 /sys/fs/cgroup/mydocker/<name>/）
cg, err := cgroup.NewWithConfig(cgroup.Config{
    Name:   cgName,
    Parent: cfg.CgroupParent,
    Driver: cfg.CgroupDriver,
})
这一段就是"在 cgroup 树上创建一个新节点"——本质就是 mkdir /sys/fs/cgroup/<parent>/<name>/，加上往里面写几个限制文件。Parent 就是这个新节点要挂在树的哪个父节点下。

五、连回 /proc/self/cgroup
回想上一题：

bash
$ cat /proc/self/cgroup
0::/kubepods.slice/kubepods-burstable.slice/kubepods-burstable-pod_abc.slice/cri-containerd-xxx.scope
   ↑
   这就是这棵树里"我所在节点"的路径
它是绝对路径，从树根开始一路写下来——和文件系统里 pwd 给你的路径完全一样的概念，只不过描述的是 cgroup 树而不是文件树。

cgroup namespace 把这个路径"裁掉前缀"，让容器只看到它在自己子树根之后的部分（通常是 /），实现"假装自己在树根"的效果。

六、和文件系统树类比
概念	文件系统	cgroup
树根	/	/sys/fs/cgroup/
节点	目录	子目录（每个就是一个 cgroup）
创建节点	mkdir foo	mkdir /sys/fs/cgroup/foo/（自动变成新 cgroup）
进程在哪	chdir（每个进程一个 cwd）	cgroup.procs 文件里写 PID
删节点	rmdir foo	rmdir /sys/fs/cgroup/foo/（前提是空的）
可以完全用普通的目录操作来管理 cgroup 树——这是 Linux "everything is a file" 的体现。

一句话本质
cgroup 树 = /sys/fs/cgroup/ 下的目录树，每个目录是一个 cgroup 节点，节点里的文件是限制规则和用量；进程被挂在树的某个叶子节点上，受这个节点和所有祖先节点的资源限制叠加约束。systemd 和容器运行时通过 mkdir 这棵树来组织系统资源。

cgroup 树 vs "容器隔离的 cgroup" —— 是同一个东西
这俩不是两个概念，是一个东西的两面：

cgroup 树 = 这个机制的结构形态（一棵目录树）
容器隔离的 cgroup = 这个机制的用途之一（用它给容器做资源限制）
就好比：

"户籍系统" 是一棵树（国 → 省 → 市 → 区 → 户）
"给某户人家限电" 是户籍系统的一个用途
不是两个系统，是一个系统的不同切面。

一、把它们的关系画出来
         ┌─────────────────────────────┐
         │       Linux cgroup 子系统    │
         │  （内核的资源管控引擎）        │
         └──────────────┬──────────────┘
                        │ 通过虚拟文件系统暴露
                        ▼
         ┌─────────────────────────────┐
         │  /sys/fs/cgroup/  ← cgroup 树│
         │     /                        │
         │     ├── system.slice/        │
         │     ├── user.slice/          │
         │     └── kubepods.slice/      │  ← 这就是 cgroup 树
         │         └── pod_abc/         │
         │             └── nginx/       │     每个目录 = 一个节点
         │                 ├─ memory.max│     节点里的文件 = 限制规则
         │                 ├─ cpu.max   │
         │                 └─ cgroup.procs ← 进程在这里登记
         └─────────────────────────────┘
                        │
       ┌────────────────┼─────────────────┐
       ▼                ▼                 ▼
给 systemd 服务      给容器隔离          给用户会话
限资源              限资源              限资源
（nginx.service）   （docker-xxx）      （user-1000）
                         ▲
                         │
                 这个就是"容器隔离的 cgroup"
                 ——它只是 cgroup 树上**长出来的一根树枝**
二、"容器隔离" 在树上长什么样
容器运行时（Docker / containerd / mydocker-cri）启动一个容器时，做的事情就是：

在 cgroup 树某个父节点下 mkdir 一个新子目录（= 长出一片新叶子）
往这个新目录里写限制文件（memory.max = 100MB 等）
把容器进程的 PID 写进 cgroup.procs（= 进程登记到这片叶子上）
举例：

启动一个 K8s nginx 容器之前：
 
/sys/fs/cgroup/
└── kubepods.slice/
    └── pod_abc.slice/
         (空)
 
 
启动后：
 
/sys/fs/cgroup/
└── kubepods.slice/
    └── pod_abc.slice/
        └── cri-containerd-nginx-xxx.scope/    ← 新长出来的叶子
            ├── memory.max = 104857600         ← 限 100MB
            ├── cpu.max   = "50000 100000"     ← 限 50% CPU
            └── cgroup.procs                   ← 里面写着 nginx 进程的 PID
这就是"容器隔离的 cgroup"——它不是一棵新树，只是 cgroup 树上的一个节点。

本项目里这件事就发生在：

container_linux.go:121-145
// 创建 cgroup（写文件，比如 /sys/fs/cgroup/mydocker/<name>/）
cg, err := cgroup.NewWithConfig(cgroup.Config{
    Name:   cgName,
    Parent: cfg.CgroupParent,    ← 挂在树的哪个父节点下
    Driver: cfg.CgroupDriver,
})
...
// Apply 写资源限制（内存上限、CPU 配额）
if err := cg.Apply(cfg.Resources); err != nil {
...
// AddProc 把子进程 PID 写进 cgroup.procs 文件
if err := cg.AddProc(cmd.Process.Pid); err != nil {
三步：mkdir 新节点 → 写限制文件 → 把 PID 登记进去。这就是"容器隔离的 cgroup"的全部实现。

三、为什么需要"树"而不是平铺？
如果不是树，每个容器各自一个独立的 cgroup，会出现两个问题：

问题 1：没法做"组上限"
K8s 想说"所有 BestEffort 类的 Pod 加起来不能超过 4GB"——平铺结构做不到。但有树之后：

/sys/fs/cgroup/
└── kubepods.slice/                     memory.max = 8GB
    ├── kubepods-besteffort.slice/      memory.max = 4GB   ← 给一整类设上限
    │   ├── pod_a/container/   100MB
    │   ├── pod_b/container/   200MB
    │   └── pod_c/container/   ...
    └── kubepods-burstable.slice/       memory.max = 6GB
        └── ...
besteffort 这一整个子树加起来不能超 4GB。一个 Pod 占多了，同类其他 Pod 就没空间——这是 K8s QoS 优先级的内核基础。

问题 2：没法做"父子继承"
容器在 Pod 节点下、Pod 在 QoS 节点下、QoS 在 K8s 总节点下——任何祖先满了，整片子树都受影响。这种"层层施压"只有树状结构能表达。

四、把"容器隔离的 cgroup" 重新定义清楚
回到你之前那张三件套表：

维度	容器隔离的 cgroup
它是什么	在 cgroup 树上专门给某个容器创建的一个节点（一个目录）
干什么用	节点里的限制文件给容器进程封顶资源用量
怎么实现的	容器运行时 mkdir /sys/fs/cgroup/<父>/<容器名>/ + 写文件
跟 cgroup 树啥关系	它就是 cgroup 树上的一个节点，不是另一个独立东西
所以更准确的说法是：

"容器隔离用的 cgroup" = "在 cgroup 树里，专门为这个容器开辟的一个节点"

而不是"另一个 cgroup"。整台机器上只有一棵 cgroup 树，systemd 服务、用户会话、所有容器全都挂在这棵树的不同分支上。

五、一图对照三个相关概念
经常容易混的三件事：

名称	是什么	跟容器的关系
cgroup 树	/sys/fs/cgroup/ 下的整棵目录树	容器的 cgroup 节点是这棵树上的一根树枝
容器的 cgroup	这棵树上专门给某个容器创建的节点	容器隔离三件套之一：资源隔离
cgroup namespace	一种 namespace，遮蔽 /proc/self/cgroup 显示的路径	让容器看不见自己在树上的真实位置（视野遮蔽）
第一个是结构，第二个是用法，第三个是视野遮罩。三者结合才是 K8s 容器场景下的完整图景：

cgroup 树：内核维护的全局资源管控结构
容器的 cgroup：树上分给该容器的那个节点（决定它实际能用多少资源）
cgroup namespace：让容器看不见自己在树里的位置（决定它以为自己在哪）
一句话本质
cgroup 树是 Linux 内核组织资源管控的整体结构，容器隔离的 cgroup 只是这棵树上专为某个容器开辟的一个节点——名字叫 cgroup 因为它就是这棵树的一员。它们不是两个东西，是同一棵树的"全图"和"局部"。