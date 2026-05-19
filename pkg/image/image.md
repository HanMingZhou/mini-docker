Image 包知识地图
mini-docker 的 pkg/image 干三件事：

管理本地镜像（什么镜像、有几层、放在哪）
从远程 registry 拉镜像（Pull）
给容器组装 rootfs（OverlayFS 把多层合成一个根目录）
一、先建立"镜像到底是什么"的心智模型
镜像 ≠ 一个文件 = 一堆"层（layer）"的有序集合
每一层就是一个 tar 包，里面装着"相对上一层我增删改了什么文件"。

举个例子，一个 nginx 镜像可能有 6 层：

layer 0: alpine 基础系统    （/bin /etc /lib /usr ...）
layer 1: 装 openssl         （新增 /usr/lib/libssl.so 等）
layer 2: 装 nginx           （新增 /usr/sbin/nginx 等）
layer 3: 拷配置             （新增 /etc/nginx/nginx.conf）
layer 4: 删一个不要的文件    （记录"删除 /tmp/xxx"，用 whiteout 标记）
layer 5: 设置默认命令        （元数据，不在 tar 里）
关键术语对照表
术语	解释
Image（镜像）	一组有序 layer + 一个 config（默认命令、env 等）+ 一个 manifest（清单）
Layer（层）	一个 tar.gz 包，记录相对下层的文件增删改
DiffID	层解压后内容的 sha256，跨 registry 稳定（mini-docker 用这个做 key）
Manifest（清单）	描述镜像的 JSON：有哪些层、哪个 config、digest 是多少
Config（配置）	镜像的元数据 JSON：默认 Cmd/Entrypoint/Env/WorkingDir
Reference	用户写的名字，如 nginx:alpine、index.docker.io/library/nginx:latest
Registry	存镜像的服务器，比如 Docker Hub、阿里云 ACR
OverlayFS	Linux 内核提供的"分层文件系统"，把多个目录合成一个视图
whiteout	在上层标记"下层的某个文件被删除"的特殊文件
二、文件布局：磁盘上长什么样
这是理解整个包的关键。打开 image.go 文件顶部的注释 + Pull 的实现，能拼出这张图：

<root>/                                       ← Store.root (一般是 /var/lib/mydocker)
├── images/                                   ← 镜像目录（按名字索引）
│   ├── nginx:alpine/
│   │   └── manifest.json                     ← 镜像元数据
│   └── busybox:latest/
│       └── manifest.json
│
├── layers/                                   ← 共享层池（按 digest 索引）
│   ├── sha256_abc123.../                     ← 解压后的 layer 内容
│   │   ├── bin/  etc/  usr/  ...
│   │   └── .done                             ← 完成标记
│   ├── sha256_def456.../
│   │   ├── usr/sbin/nginx
│   │   └── .done
│   └── ...
│
└── containers/                               ← 每个容器自己的目录
    └── abc123/
        ├── upper/                            ← 读写层（容器写入的新文件）
        ├── work/                             ← OverlayFS 内部工作目录
        └── merged/                           ← 容器启动时挂载，作为 /
设计要点：层是内容寻址且全局共享的
层的目录名 = 内容的 sha256（DiffID）
不同镜像可以引用同一层（比如 100 个镜像都基于 alpine），磁盘上只存一份
manifest.json 里用 LayerRefs: ["layers/sha256_xxx", ...] 引用
这就是 Docker 节省磁盘的核心机制。

三、核心类型（image.go）
image.go:29-49
type Manifest struct {
    Name      string    `json:"name"`
    Reference string    `json:"reference,omitempty"`
    Digest    string    `json:"digest,omitempty"`
    Layers    []string  `json:"layers"`
    LayerRefs []string  `json:"layer_refs,omitempty"`
    ...
    Config ImageConfig `json:"config,omitempty"`
}
Name：用户友好的短名，如 nginx:alpine，作为本地索引 key。
Reference：完整 registry 引用，如 index.docker.io/library/nginx:alpine。
Digest：镜像 config 的 sha256，唯一标识镜像版本。
Layers vs LayerRefs：
Layers：旧式，层存在镜像目录内（如 images/nginx/layers/0），用于 Import 的本地导入
LayerRefs：新式，层存在共享池（layers/sha256_xxx），用于 Pull 拉下来的镜像
Config：镜像默认的 Cmd / Entrypoint / Env / WorkingDir，跑容器时如果用户不指定就用这些。
image.go:52-66
type Store struct {
    root string
}
 
func New(root string) (*Store, error) {
Store 就是镜像管理器，所有操作的入口。

四、三个核心流程
流程 A：本地导入（Import）
image.go:264-297
func (s *Store) Import(name, tarPath string) error {
    ...
    imgDir := s.ImageDir(name)
    layerDir := filepath.Join(imgDir, "layers", "0")
    ...
    if err := extractTar(tarPath, layerDir); err != nil {
        ...
    }
    m := &Manifest{
        Name:      name,
        Layers:    []string{filepath.Join("layers", "0")},
        CreatedAt: time.Now().UTC(),
    }
只支持单层。等价于 docker import：你给它一个解压好的 rootfs tar 包，它解压到 images/<name>/layers/0/ 然后写 manifest。简单粗暴，适合自己打包测试镜像。

流程 B：远程拉取（Pull）
这是 image 包最有意思的部分，在 puller.go。借助第三方库 go-containerregistry 处理 OCI 协议复杂性。

puller.go:34-90
func (s *Store) Pull(ref string, opts PullOptions) (*Manifest, error) {
    ...
    tag, err := name.ParseReference(ref, name.WeakValidation)
    ...
    img, err := remote.Image(tag, remoteOpts...)
    ...
    layers, err := img.Layers()
逐步分解：

name.ParseReference：把 "nginx" 正规化成 "index.docker.io/library/nginx:latest"。Docker 的命名约定有不少坑（默认 registry、默认 namespace library、默认 tag latest），这个库帮你处理。
authn.DefaultKeychain：读 ~/.docker/config.json 拿凭证。公共镜像不需要登录。
remote.Image(tag, ...)：通过 HTTPS 调 registry 的 OCI API 拿到镜像 manifest 和 config。这一步只是拿元数据，没下载层数据。
img.Layers()：返回每一层的"句柄"（懒加载）。
遍历每层：
puller.go:97-140
for i, layer := range layers {
    digest, err := layer.DiffID()
    ...
    layerKey := strings.ReplaceAll(digest.String(), ":", "_")
    layerDir := filepath.Join(s.LayersDir(), layerKey)
    ...
    if _, err := os.Stat(filepath.Join(layerDir, ".done")); err == nil {
        ...
        continue   // 已下载过，跳过（这就是层共享）
    }
    ...
    rc, err := layer.Uncompressed()    // 流式解压 tar.gz
    ...
    if err := extractTarStream(rc, layerDir); err != nil {
拿层的 DiffID（解压后内容的 sha256）作为本地 key
检查 .done 标记：如果别的镜像已经下载过这层，跳过——这就是层去重
否则流式下载并解压到 layers/sha256_xxx/
完成后写 .done 标记
保存 manifest：包含 LayerRefs（指向共享层）和镜像默认的 Config（Cmd / Env 等）。
流程 C：组装 rootfs（PrepareRootfs）—— OverlayFS 上场
overlay_linux.go:11-58
func prepareRootfs(s *Store, imageName, containerDir string) (string, error) {
    ...
    layers, err := s.LayerPaths(imageName)
    ...
    data := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s",
        formatLowerDirs(layers), paths.Upper, paths.Work)
 
    if err := syscall.Mount("overlay", paths.Merged, "overlay", 0, data); err != nil {
OverlayFS 的核心概念（最容易卡的点）：

读文件：
  1. 先看 upperdir 有没有 → 有就用
  2. 没有就从 lowerdir 从上到下找 → 找到就用
 
写文件（修改已有文件）：
  1. 把文件从 lower 复制到 upper（copy-up）
  2. 在 upper 上修改
 
删文件：
  1. 在 upper 创建一个 character device 0/0（whiteout 标记）
  2. lower 的文件不会真删，但被遮蔽，看不见了
 
workdir：
  内核用来做 copy-up 等原子操作的临时空间，必须和 upperdir 在同一个文件系统。
为什么需要 upper 和 work？

lowerdir（镜像层）必须是只读的——很多容器共享，不能改。
upperdir 是容器专属的可写层，容器死了删掉它就清空所有改动（如果不挂 volume）。
这就是为什么 docker rm 那么快、docker run --rm 数据不留——所有改动只在 upper 里。
formatLowerDirs 的顺序：

image.go:362-368
func formatLowerDirs(layers []string) string {
    rev := make([]string, len(layers))
    for i, l := range layers {
        rev[len(layers)-1-i] = l
    }
    return strings.Join(rev, ":")
}
Manifest.Layers 是从下到上存的（layer 0 是基础层），但 OverlayFS 的 lowerdir=A:B:C 要求上层在前。所以这里要反转。比如：

manifest: [base, openssl, nginx]（base 是 layer 0）
mount: lowerdir=nginx:openssl:base,upperdir=...,workdir=...
这是个非常常见的"踩坑点"，看反了整个文件系统就翻过来了。

五、whiteout：删除是怎么"分层表达"的
layer.go:46-62
base := filepath.Base(name)
if strings.HasPrefix(base, ".wh.") {
    dir := filepath.Dir(name)
    ...
    if err := os.WriteFile(target, nil, 0644); err != nil {
        return err
    }
    continue
}
OCI 镜像规范里，删除一个文件靠在 tar 里放一个特殊文件 .wh.<name>：

原本：
  layer1/usr/bin/foo    （某文件）
 
layer2 想删除 foo，它的 tar 里有：
  ./usr/bin/.wh.foo     （0 字节，名字以 .wh. 开头）
OverlayFS 真正"看得懂"的 whiteout 不是这样的文件，而是字符设备 mknod(0,0) 或者目录上的 trusted.overlay.opaque xattr。需要 CAP_MKNOD 权限。

mini-docker 简化为"放一个空标记文件"——能教学，但多层删除语义不完全正确（OverlayFS 不会真把下层文件遮蔽）。注释里也讲了：教学级别，单层场景够用。

六、Resolve 和别名匹配
image.go:137-172
func (s *Store) Resolve(ref string) (*Manifest, error) {
允许用户用各种方式引用同一个镜像：

用户输入	匹配规则
nginx:alpine	exact match on Name
index.docker.io/library/nginx:alpine	exact match on Reference
sha256:abc...	exact match on Digest
nginx（无 tag）	自动加 :latest 再 match
nginx:alpine 但本地只有 index.docker.io/library/nginx:alpine	取最后一段比较
这模仿 Docker CLI 的体验：你怎么写都能找到。

七、把整个 image 包串成一个生命周期
用户：mydocker pull nginx:alpine
   │
   ▼
Store.Pull("nginx:alpine", ...)
   │
   ├─ ParseReference → index.docker.io/library/nginx:alpine
   ├─ remote.Image → HTTP 拿 manifest + config
   ├─ for each layer:
   │     ├─ DiffID → "sha256_xxx"
   │     ├─ 已有 .done 标记？跳过（共享）
   │     └─ 否则下载 + extractTarStream → layers/sha256_xxx/
   └─ 写 images/nginx:alpine/manifest.json（含 LayerRefs 和 Config）
 
────────────────────────────────────────────────────────────
 
用户：mydocker run nginx:alpine
   │
   ▼
Store.Resolve("nginx:alpine") → Manifest
   │
   ▼
Store.PrepareRootfs("nginx:alpine", "/var/lib/mydocker/containers/abc/")
   │
   ├─ LayerPaths → ["/.../layers/sha256_base", "/.../layers/sha256_nginx"]
   ├─ mkdir upper/ work/ merged/
   └─ mount -t overlay overlay merged \
        -o lowerdir=sha256_nginx:sha256_base,upperdir=upper,workdir=work
        ↓
   merged/ 现在是完整的 nginx 文件系统
   │
   ▼ （后续）
container.start → fork init → pivot_root(merged) → exec 用户命令
 
────────────────────────────────────────────────────────────
 
容器退出/删除：
   ├─ CleanupRootfs(containerDir) → umount merged + rm upper/work/merged
   └─ images/ 和 layers/ 完全不动（其他容器还要用）
 
────────────────────────────────────────────────────────────
 
用户：mydocker rmi nginx:alpine
   ├─ rm -rf images/nginx:alpine/
   └─ gcOrphanLayers() 扫描 layers/，删除没人引用的
八、知识点速查
你需要掌握的概念清单
镜像 = 层 + manifest + config
层是 tar 包，按 DiffID（sha256）内容寻址
层全局共享：不同镜像引用相同层只存一份
OverlayFS 三件套：lowerdir（只读层，多个）+ upperdir（可写层，单个）+ workdir（内核工作目录）
lowerdir 顺序：上层在前，下层在后（和 manifest.Layers 反过来）
whiteout：用 .wh.<name> 在 tar 里表达"删除下层文件"
copy-up：在 overlay 里修改 lower 的文件时，先复制一份到 upper 再改
DiffID vs Digest：DiffID 是解压后内容的 hash（用于本地去重）；Digest 是 config 的 hash（用于镜像版本标识）
Reference vs Name：完整 registry 引用 vs 本地短名
推荐动手验证
在 Linux 上：

bash
# 1. 看一个真正的镜像有几层
docker pull nginx:alpine
docker history nginx:alpine
 
# 2. 手动玩 overlayfs
mkdir -p lower1 lower2 upper work merged
echo "in lower1" > lower1/a.txt
echo "in lower2 (overrides)" > lower2/a.txt
echo "only lower2" > lower2/b.txt
sudo mount -t overlay overlay \
  -o lowerdir=lower2:lower1,upperdir=upper,workdir=work merged
 
ls merged          # a.txt b.txt
cat merged/a.txt   # "in lower2 (overrides)"  ← 上层赢
 
# 在 merged 里改文件，看 upper 发生了什么
echo "modified" > merged/a.txt
ls upper           # a.txt 出现了（copy-up）
cat lower1/a.txt   # 还是 "in lower1"（lower 不动）
cat lower2/a.txt   # 还是 "in lower2 ..."（lower 不动）
 
sudo umount merged
想深入读的代码顺序
image.go 的 Manifest、Store、Import、Resolve（最简单）
overlay_linux.go 整个文件（理解 overlay 怎么挂）
layer.go 的 extractTarStream（tar 怎么解压、whiteout 怎么处理）
puller.go（最复杂，但思路就是"循环每层 → 去重 → 解压"）
如果哪一块还想再展开（比如 OCI manifest 格式、go-containerregistry 库内部、whiteout 的正确实现细节），告诉我具体点，我针对性讲。

Feedback submit