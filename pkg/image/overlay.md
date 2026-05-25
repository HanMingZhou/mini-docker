# OverlayFS：upper、work、merged 和 lower 到底是什么

容器目录里只有 3 个目录：upper、work、merged。没有 lower 这个目录。

lower 不是容器自己的目录，lower 直接指向镜像目录。OverlayFS 的"挂载"操作就是把这种"指向"建立起来——它不复制任何文件，只是告诉内核："读的时候去这些路径找"。

用一个具体例子
假设 busybox 镜像有 1 层，nginx 镜像有 4 层。我们以 nginx 为例。

步骤 1：拉镜像后磁盘上的状态

```text
/var/lib/mydocker/images/
├── sha256_aaa/
│ └── content/ ← debian base 层：里面有 /bin/sh /lib/_ /etc/passwd ...
├── sha256_bbb/
│ └── content/ ← apt 装依赖：里面只有 /usr/lib/libssl.so 等新增/修改文件
├── sha256_ccc/
│ └── content/ ← nginx 安装：/usr/sbin/nginx /etc/nginx/_ 等
└── sha256_ddd/
└── content/ ← nginx 默认配置层
```

每一层是一个真实的目录，里面只装这一层"新增或修改"的文件。这些目录是只读的（约定上，不一定真的 chmod ro）。

步骤 2：用户跑 mydocker run --image nginx，准备容器目录
代码里这段：

```go
paths := ContainerRootfsPaths(containerDir)
for _, d := range []string{paths.Upper, paths.Work, paths.Merged} {
    if err := os.MkdirAll(d, 0755); err != nil { ... }
}
```

执行后：

```text
/var/lib/mydocker/containers/abc123/
├── upper/ ← 空的
├── work/ ← 空的
└── merged/ ← 空的
```

注意此时 merged/ 是空目录，里面什么都没有。

步骤 3：神奇的 mount 调用

```go
data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
    formatLowerDirs(layers), paths.Upper, paths.Work)
syscall.Mount("overlay", paths.Merged, "overlay", 0, data)
```

实际传给内核的字符串：

```text
lowerdir=/var/lib/mydocker/images/sha256_ddd/content:/var/lib/mydocker/images/sha256_ccc/content:/var/lib/mydocker/images/sha256_bbb/content:/var/lib/mydocker/images/sha256_aaa/content,
upperdir=/var/lib/mydocker/containers/abc123/upper,
workdir=/var/lib/mydocker/containers/abc123/work
```

这条 mount 调用做的不是文件复制，而是告诉内核：

"把 merged/ 这个目录注册成一个 overlayfs 挂载点。当有人来读它时，你按这个顺序去这 4 个镜像层目录 + 1 个 upper 目录里找。"

mount 调用返回成功后，磁盘上还是没多任何文件。upper/、work/、镜像目录全部原样不动。改变的是内核里的挂载表：

```bash
$ cat /proc/mounts | grep merged
overlay /var/lib/mydocker/containers/abc123/merged overlay rw,...,lowerdir=...,upperdir=...,workdir=... 0 0
```

步骤 4："看上去像合并"——但其实是查找时合并
现在你跑：

```bash
ls /var/lib/mydocker/containers/abc123/merged
```

会看到 bin/ etc/ lib/ usr/ var/ ...——就跟看到一个完整的 nginx 容器根目录一样。

但这些文件不在 merged 目录里。它们物理上还在镜像层目录里。OverlayFS 的内核驱动在你 ls 的瞬间，遍历所有 layer 目录，把每个目录里的条目合并起来，去重后返回。

文件查找的具体过程
容器跑 cat /etc/passwd：

容器进程做系统调用 open("/etc/passwd")
内核发现这是 overlayfs 挂载点，进入 overlay 驱动
按顺序检查：
upper/etc/passwd — 不存在
sha256_ddd/content/etc/passwd — 不存在
sha256_ccc/content/etc/passwd — 不存在
sha256_bbb/content/etc/passwd — 不存在
sha256_aaa/content/etc/passwd — 找到了（debian base 层里有）
内核返回这个真实文件的句柄
容器读到 debian 的 /etc/passwd 内容
整个过程没有任何文件复制。只是内核在帮你"按层穿透查找"。

同名文件如何决定优先级
如果 nginx 层（ddd）和 base 层（aaa）都有
nginx.conf
，内核用排在前面的（ddd），因为 lowerdir 是按"上层在前、下层在后"约定。

记忆窍门：lowerdir=A:B:C，A 优先级最高（最上层），C 最低（最下层）。

写操作的例子（copy-up）
这是 OverlayFS 最神奇的地方。容器跑：

echo "127.0.0.1 myhost" >> /etc/hosts
发生了什么：

内核在 overlay 里找 /etc/hosts：upper 里没有 → 在 sha256_aaa/content/etc/hosts 找到（debian 层）
检测到这是写操作（O_WRONLY/O_APPEND）
copy-up：内核把 sha256_aaa/content/etc/hosts 的全部内容复制到 upper/etc/hosts
对 upper/etc/hosts 执行写入
这之后磁盘上的状态：

```text
upper/
└── etc/
└── hosts ← 新增了！里面是修改后的内容
images/sha256_aaa/content/etc/hosts ← 完全不动，原样
```

下次再读 /etc/hosts，按查找顺序，upper/etc/hosts 优先级最高，直接返回 upper 里的。

镜像永远不被污染——这就是 copy-on-write 的字面意思。

删除操作（whiteout）
容器跑 rm /etc/hosts：

内核不能真的删 sha256_aaa/content/etc/hosts（那是只读层，会污染镜像）
在 upper/etc/hosts 创建一个 whiteout——一个特殊的字符设备文件（major=0, minor=0）
之后再读 /etc/hosts，内核看到 upper 里有 whiteout 标记，直接返回"不存在"，不再往下层找

```bash
$ ls -la upper/etc/
crw------- 1 root root 0, 0 ... hosts ← c 表示字符设备，这就是 whiteout
```

merged 目录到底是什么
它是一个虚拟挂载点，不是普通目录。

挂载之前：是空目录
挂载之后：访问它就是访问 overlayfs（内核驱动接管）
卸载之后（umount merged）：又变回空目录
所以严格说，merged 没有"merge"任何东西到自己里面。它只是一个入口，背后由内核的 overlay 驱动负责动态合并多个目录。

你可以这样验证：

```bash
# 挂载之前

$ ls merged
（空）

# 挂载

$ mount -t overlay overlay -o lowerdir=...,upperdir=...,workdir=... merged

# 挂载之后

$ ls merged
bin etc lib usr var ... ← 看上去 merged 里有这些

# 但是用 stat 看 merged 目录本身

$ stat merged
File: 'merged'
Size: 4096 ... ← 还是 4096 字节，没有变大
Inode 没变
仍然是空目录占的空间

# 卸载

$ umount merged

# 又变回空了

$ ls merged
（空）
```

merged 目录的 inode 大小始终是 4096，因为它本身真的没装东西。看到的内容是内核在实时计算给你看的。

一图总结

```text
┌─────────────────────────────────────────────────────────┐
│ merged/ （挂载点，虚拟视图，容器看到的 /） │
└────────────▲────────────────────────────────────────────┘
│ 内核 overlay 驱动按顺序查找：
│
┌─────┴──────────────────────────────────────────┐
│ upper/ ← 容器独占可写层 │
│ （目前是空，写文件后才有内容） │
└─────┬──────────────────────────────────────────┘
│ ↑找不到就往下找
▼
┌────────────────────────────────────────────────┐
│ sha256_ddd/content/ ← 镜像层 4（最上面的镜像层）│
└─────┬──────────────────────────────────────────┘
▼
┌────────────────────────────────────────────────┐
│ sha256_ccc/content/ ← 镜像层 3 │
└─────┬──────────────────────────────────────────┘
▼
┌────────────────────────────────────────────────┐
│ sha256_bbb/content/ ← 镜像层 2 │
└─────┬──────────────────────────────────────────┘
▼
┌────────────────────────────────────────────────┐
│ sha256_aaa/content/ ← 镜像层 1（base 层） │
└────────────────────────────────────────────────┘

work/ ← 内核暂存区，用户不要碰
```

核心心智模型：

lower\* = 镜像层目录（只读，多个，按优先级排序）
upper = 容器独占可写层
merged = 虚拟入口，访问它 = 触发内核合并查找
work = 内核工作目录
mount 不复制文件，只在内核里建立"查找规则"
读 = 按 upper → lower1 → lower2 → ... 的顺序找
写 = 复制到 upper 再改（copy-up）
删 = 在 upper 放 whiteout 标记
这就是 Docker 能"几秒启动一个容器"的原因——根本不复制文件系统，只是建一个 overlayfs 挂载。

## Mount 操作

```go
func Mount(source string, target string, fstype string, flags uintptr, data string) error
```

对应内核的 mount(2) 系统调用：

```c
int mount(const char *source, const char *target,
const char *filesystemtype, unsigned long mountflags,
const void *data);
```

我们调用的具体值：

```go
syscall.Mount(
"overlay", // 参数 1: source
paths.Merged, // 参数 2: target
"overlay", // 参数 3: fstype
0, // 参数 4: flags
data, // 参数 5: data
)
```

类比一个普通磁盘挂载
为了讲清楚，先看普通磁盘是怎么挂的。比如把 /dev/sdb1 挂成 /mnt/disk：

```bash
mount -t ext4 /dev/sdb1 /mnt/disk
```

对应到内核：

```c
mount("/dev/sdb1", // source: 源——这块物理设备
"/mnt/disk", // target: 目标——挂在哪个目录下
"ext4", // fstype: 用哪种文件系统驱动来读它
0, // flags: 不加特殊标志（默认读写）
NULL); // data: 没有额外参数
```

意思是："请用 ext4 驱动，把 /dev/sdb1 这块设备的内容显示在 /mnt/disk 这个目录下。"

挂载之后，ls /mnt/disk 就能看到这块磁盘里的文件。

OverlayFS 的特殊之处
OverlayFS 不是磁盘文件系统，它没有"源设备"这个概念。它的"内容"来自一堆已经存在的目录（lowerdir/upperdir）。

但 mount 系统调用的参数固定就是这 5 个，所以 overlay 必须把它的"非传统"信息塞进第 5 个参数 data 字符串里。

下面逐个看：

参数 1：source = "overlay"

```text
"overlay" // ← 这里写什么都行
```

对普通磁盘 mount 来说，source 是设备路径（/dev/sdb1），有实际意义。

对 overlayfs 来说，source 字段没有意义。内核根本不读它，因为 overlay 没有源设备——它的"源"全在 data 里说清楚了。

但 mount 调用规定 source 不能为空，所以约定俗成写文件系统名字 "overlay"。你写 "none"、"foobar"、甚至 "hello" 都能挂载成功。

只是 cat /proc/mounts 看到的第一列就是这个字符串，写 overlay 比较好认：

```text
overlay /var/lib/.../merged overlay rw,...
^^^^^^^ ← 这就是 source
```

如果你写 "hello"，那一列就显示 hello，但功能完全一样。

参数 2：target = paths.Merged

```go
paths.Merged // = /var/lib/mydocker/containers/abc/merged
```

这是挂载点——挂载之后，访问这个目录就等于访问这个文件系统。

要求：

必须是一个已存在的目录（所以代码前面 MkdirAll(paths.Merged, 0755) 先建好）
不需要是空目录，但如果不空，里面原有的内容会被"遮住"看不到（直到 unmount）
挂载之前：

```bash
$ ls /var/lib/mydocker/containers/abc/merged
（空）
```

挂载之后：

```bash
$ ls /var/lib/mydocker/containers/abc/merged
bin/ etc/ lib/ usr/ var/ ... ← overlayfs 给你看的合并视图
```

这就是"target"的意思——告诉内核 overlayfs 应该挂在哪。

参数 3：fstype = "overlay"

```text
"overlay"
```

这个必须是字符串 "overlay"，写错了内核就不知道用哪个驱动了。

Linux 内核里有几十种文件系统驱动，每种由一个名字标识：

| fstype    | 用什么驱动                |
| --------- | ------------------------- |
| "ext4"    | ext4 磁盘文件系统         |
| "tmpfs"   | 内存文件系统              |
| "proc"    | /proc 伪文件系统          |
| "sysfs"   | /sys 伪文件系统           |
| "overlay" | OverlayFS（联合文件系统） |
| "nfs"     | 网络文件系统              |

写 "overlay" 就是说："请用 OverlayFS 驱动来处理这次挂载。"

可以查看你的内核支持哪些：

```bash
cat /proc/filesystems
```

参数 4：flags = 0

```go
0 // 不加任何 mount 标志
```

flags 是一个位掩码，用来开关一些通用的挂载选项。常见的：

| 常量       | 作用                               |
| ---------- | ---------------------------------- |
| MS_RDONLY  | 只读挂载（写文件会报 EROFS）       |
| MS_NOSUID  | 忽略 setuid/setgid 位（安全）      |
| MS_NOEXEC  | 不允许执行二进制（安全）           |
| MS_NODEV   | 不允许设备文件（安全）             |
| MS_BIND    | bind mount（把目录挂到另一个目录） |
| MS_REMOUNT | 修改已挂载的选项                   |
| MS_PRIVATE | mount propagation 设为 private     |

我们传 0 就是默认：可读写、允许 setuid、允许执行。容器需要这些。

如果某天想做"只读容器"，就改成：

```go
syscall.Mount("overlay", paths.Merged, "overlay", syscall.MS_RDONLY, data)
```

参数 5：data —— overlayfs 真正的核心

```go
data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
    formatLowerDirs(layers), paths.Upper, paths.Work)
```

这是 overlayfs 专属的参数字符串，告诉它"具体怎么合并"。

普通磁盘挂载 data 通常是 NULL（Go 里传 ""），因为没什么好说的。但 overlay 没有源设备，所有信息都要靠 data 传。

实际生成的字符串大概长这样：

```text
lowerdir=/var/lib/mydocker/images/sha256_ddd/content:/var/lib/mydocker/images/sha256_ccc/content:/var/lib/mydocker/images/sha256_bbb/content:/var/lib/mydocker/images/sha256_aaa/content,upperdir=/var/lib/mydocker/containers/abc/upper,workdir=/var/lib/mydocker/containers/abc/work
```

格式：key1=value1,key2=value2,key3=value3（逗号分隔的 key-value 对）。

OverlayFS 认识的关键 key：

| key       | 值                  | 作用                         |
| --------- | ------------------- | ---------------------------- |
| lowerdir  | 用 : 分隔的多个目录 | 只读层（镜像层），最上层在前 |
| upperdir  | 一个目录            | 容器独占可写层               |
| workdir   | 一个空目录          | 内核工作目录                 |
| userxattr | 无值，开关          | 用户扩展属性（不常用）       |

所以 data 这个字符串本质上就是告诉 overlay 驱动："请用这些目录组合一个新视图给我。"

把所有参数串起来读一遍

```go
syscall.Mount(
"overlay", // 1. 这次挂载叫 overlay（标识用，不影响功能）
paths.Merged, // 2. 挂在 /var/lib/.../merged 这个目录
"overlay", // 3. 用 OverlayFS 内核驱动
0, // 4. 默认选项（可读写）
"lowerdir=A:B:C,upperdir=D,workdir=E", // 5. 具体怎么挂：A,B,C 当只读层，D 当可写层，E 当工作目录
)
```

翻译成大白话：

"嘿内核，请你用 OverlayFS 驱动，把这堆镜像层目录（A:B:C）作为只读层、把 D 作为可写层、把 E 作为工作目录，合并成一个虚拟视图，挂到 paths.Merged 这个目录下，访问 paths.Merged 时就跟访问一个完整的文件系统一样，但实际是按层去找。"

挂载成功之后，你就可以 pivot_root(paths.Merged, ...) 把容器进程的根目录切到这里，容器就启动了。

类比表格

| mount 调用要做的事               | 对应参数                                          |
| -------------------------------- | ------------------------------------------------- |
| 要挂载什么内容（源）             | source 参数；overlay 不用，所以填占位符           |
| 挂在哪里（目标目录）             | target 参数 = paths.Merged                        |
| 用什么文件系统驱动               | fstype 参数 = "overlay"                           |
| 挂载的通用选项（只读？setuid？） | flags 参数 = 0（默认）                            |
| 该文件系统的私有参数             | data 参数 = lowerdir=...,upperdir=...,workdir=... |

只要记住 "overlay 把所有真正重要的信息塞在 data 里"，前 4 个参数都是固定套路，看一眼就过。

## workdir 为什么不能碰

workdir 我之前一直说"别碰"是因为它是内核私有的实现细节，但既然你想搞清楚原理，我从根上讲。

先讲一个概念：原子操作
文件系统的某些操作必须是原子的——要么完全成功，要么完全没发生。比如 rename(a, b)：

不能出现"a 已经消失，但 b 还没出现"的中间状态
否则进程崩溃 / 断电的时候，文件就丢了
普通文件系统（ext4、xfs）原子性靠日志（journal）保证。OverlayFS 不是真的存数据的文件系统，它只是个"合并视图"。但容器里的程序仍然期望 rename、unlink 等操作是原子的。

workdir 就是 OverlayFS 用来实现原子操作的"暂存区"。

一个具体场景：rename 跨层文件
容器里跑：

```bash
mv /etc/nginx.conf /etc/nginx.conf.bak
```

这是一个 rename 系统调用：把 nginx.conf 改名为 nginx.conf.bak。

在 overlay 视角下：

nginx.conf 在镜像层（lowerdir），只读
nginx.conf.bak 不存在（要新创建）
如果直接做的话：

在 upper 创建 nginx.conf.bak（复制 lower 的 nginx.conf 内容）
在 upper 创建 nginx.conf 的 whiteout（遮住下层）
问题是这两步之间如果出问题（比如内核被中断），就会出现：

第 1 步成功，第 2 步没做 → 容器里能同时看到 nginx.conf 和 nginx.conf.bak
第 2 步成功，第 1 步没做 → 两个文件都没了
rename 必须原子。 所以 OverlayFS 的做法是：

先在 workdir 里准备好"目标状态的临时副本"（不可见区域）
一个内核原子调用把临时副本挪到 upperdir，同时清理 workdir
这个挪动是原子的（依赖底层文件系统的 rename 原子性）
workdir 的两个硬性要求
理解了用途，就能理解为什么有这两个限制：

### 要求 1：必须和 upperdir 在同一文件系统

因为 OverlayFS 在 workdir 和 upperdir 之间做的是rename操作（不是 copy）。rename 是 inode 级别的指针修改，不能跨文件系统。如果跨了，rename 会变成 copy + delete 两步——不再原子。

### 错误示例：work 和 upper 在不同 fs

```text
upperdir=/mnt/disk1/upper
workdir=/mnt/disk2/work ← 跨了文件系统，mount 直接报错 EXDEV
```

### 要求 2：必须是空目录

每次挂载 overlayfs 时，内核会在 workdir 里创建一个特殊的子目录 work/，作为它的私有暂存区。如果 workdir 不空，内核担心遇到上次没清理的脏数据，直接拒绝挂载。

```bash
$ mkdir /tmp/work
$ touch /tmp/work/foo ← 放了个文件
$ mount -t overlay -o lowerdir=...,upperdir=...,workdir=/tmp/work overlay /tmp/merged
mount: ... wrong fs type, bad option, bad superblock ...
$ dmesg | tail
overlayfs: workdir is not empty
```

实际能看到的内容
挂载 overlay 之后，看一下 workdir：

```bash
$ ls -la /var/lib/mydocker/containers/abc/work
drwxr-xr-x 3 root root 4096 Nov 15 10:00 .
drwxr-xr-x 5 root root 4096 Nov 15 10:00 ..
drwx------ 2 root root 4096 Nov 15 10:00 work ← 内核自己建的子目录
```

注意挂载之后多了个 work/ 子目录，权限 drwx------（只有 root 能进），里面平时是空的。当容器做某些复杂操作（rename、跨层删除、copy-up 大文件等）时，内核会临时往这个子目录里放东西，操作完成立刻清理。

你绝对不要手动往里面写东西，也不要删它。

```bash
$ ls /var/lib/mydocker/containers/abc/work/work
（通常是空的，瞬态会有内核临时文件）
```

一个具体的 copy-up 例子
容器里跑 echo "new line" >> /etc/passwd（追加一行，需要 copy-up 一个大文件）：

OverlayFS 内部步骤：

检测到要写 lowerdir 里的文件
在 workdir/work/ 创建临时文件 tmp_xxx
把 lowerdir 的 /etc/passwd 内容完整复制到这个临时文件
在临时文件末尾追加新行
设置临时文件的权限、xattr、时间戳等元数据，让它和 lower 的 /etc/passwd 完全一致（除了内容）
rename workdir/work/tmp_xxx → upperdir/etc/passwd（这一步原子）
清理 workdir
如果第 4 步崩溃了：

upperdir 里没有残缺的 passwd（因为还没 rename 过去）
workdir 里有个临时文件，下次挂载时内核会清理掉
容器视角下 /etc/passwd 还是 lower 里的原版
这就是 workdir 的价值：为可能失败的操作提供一个"暂存 + 原子提交"的隔离区。

类比一下
如果还不直观，类比 git 的工作流：

| Overlay 概念 | git 类比                                                             |
| ------------ | -------------------------------------------------------------------- |
| lowerdir     | 远程仓库（只读，多个 commit 叠起来）                                 |
| upperdir     | 本地工作目录（你的修改）                                             |
| workdir      | git 暂存区 / .git/objects/incoming（提交前的中间状态，提交完就清理） |
| merged       | 你看到的工作目录视图                                                 |

git 在做大对象传输时也有类似的临时区——文件先下载到
.tmp.xxx
，验证完整性后再原子 rename 到正式位置。OverlayFS 的 workdir 是同一个思路。

项目代码里 workdir 的处理

```go
paths := ContainerRootfsPaths(containerDir)
for _, d := range []string{paths.Upper, paths.Work, paths.Merged} {
    if err := os.MkdirAll(d, 0755); err != nil {
        return "", fmt.Errorf("mkdir %s: %w", d, err)
    }
}
```

我们做的事：

MkdirAll：建一个空目录（满足"空"的要求）
跟 upper 在同一个 containerDir 下（满足"同 fs"的要求）
之后不再碰：内核负责管理它的内容
容器删除时（cleanupRootfs）：

```go
for _, d := range []string{paths.Merged, paths.Work, paths.Upper} {
    if err := os.RemoveAll(d); err != nil && !os.IsNotExist(err) {
        ...
    }
}
```

直接 rm -rf，把 work 整个目录删掉。这没问题，因为已经 unmount 了，里面如果有内核的暂存文件也都是孤儿，清理掉无所谓。

一句话总结
workdir 是 OverlayFS 实现原子操作（rename / 跨层删除 / copy-up）时的内部暂存区。

用户视角：建一个空目录、和 upperdir 同 fs、然后忘掉它的存在
内核视角：所有"可能失败的多步操作"先在 workdir 里准备好，最后用一次 atomic rename 提交到 upperdir
没有 workdir：overlay 没法保证写操作的原子性，断电/崩溃可能让文件系统进入半成品状态
类比磁盘文件系统的 journal、数据库的 WAL、git 的 staging area——都是"先在不可见区域准备好，再原子提交到可见区域"这一通用模式。
