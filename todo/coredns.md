完美抓到了真因：

plugin/forward: not an IP address or file: "/etc/resolv.conf"
CoreDNS 在容器里读不到 
resolv.conf
，于是把这个字面字符串当成了"上游 DNS 地址"——解析失败，整个 forward 插件初始化失败 → CoreDNS 直接 crash → liveness probe 永远拒绝连接 → CrashLoopBackOff。

根因：mydocker-cri 没生成 /etc/resolv.conf
K8s + 真实 CRI 实现（containerd/cri-o）会做这件事：在容器 rootfs 里生成 
resolv.conf
，内容是宿主 nameservers 或者 K8s 配置的 DNS。CoreDNS 的默认 Corefile 里有：

.:53 {
    forward . /etc/resolv.conf      ← 把所有不是集群域名的查询转发给宿主 DNS
}
如果容器里没有 
resolv.conf
，CoreDNS 就崩。

还有一个附加的二阶问题：日志里那一堆 "failed to get parse function: unsupported log format" 是因为 mydocker-cri 还没把容器 stdout 包装成 K8s 期望的 CRI log format（<timestamp> <stream> <P/F> <line>），但这是已知问题，不阻塞功能。

整个 "L4 完整性" 还差什么
让我对照 README 里写的 milestone：

- [x] L4B：kubeadm init ... ← 已完成（控制面跑起来）
- [ ] CoreDNS / 自定义 Pod（非 hostNetwork）调度 ← 我们现在卡这
- [ ] 多节点 kubeadm join
让我深一层看看，要让 CoreDNS 能跑，到底要补哪些 mydocker-cri 的能力：

Searched workspace
Find resolv.conf handling in CRI implementation.
Find /etc/hosts and hostname handling too.
零代码处理 
resolv.conf
 / /etc/hosts / /etc/hostname——这就是缺失项。

现在我可以画出清晰的剩余任务图给你：

CoreDNS / Pod 网络 完整方案
任务	做什么	改哪里	工作量
T1: 生成 /etc/resolv.conf	容器启动时往 rootfs 里写 resolv.conf	
container.go
 CreateContainer	30 行
T2: 生成 /etc/hosts	类似，写一份带 127.0.0.1 + 本 Pod IP + hostAlias 的 hosts	同上	30 行
T3: 生成 /etc/hostname	写 sandbox 的 hostname	同上	5 行
T4: CRI 日志格式包装	stdout/stderr 写到日志文件时按 <时间戳> <stream> <P/F> <line> 格式	
container_linux.go
 的 stdio 重定向	50 行（开个 pipe + goroutine 包装）
T5: Pod 网络验证	CoreDNS 起来后，确认 cluster DNS（10.96.0.10 → CoreDNS Pod IP）能解析	iptables/kube-proxy 验证	0 改动，纯验证
T6: kubeadm join 多节点	在第二个 lima VM 上 join	全部已具备能力，只是没验证	写一个 lima-k8s-join.sh
T1+T2+T3 是让 CoreDNS 跑起来的最小集合。这三个加起来 ~70 行代码 + 测试。

T4 是日志可读性，"qol"——不影响功能。 T5 是 T1-T3 完成后的副产品验证。 T6 是更后期的工作。

详细看下 T1-T3 应该怎么实现
让我看现在 CRI CreateContainer 是怎么准备 rootfs 的：

Searched workspace
Find where CRI prepares the container rootfs.
Read file(s)

container.go 75 - 111
Good. The right place to add /etc/{resolv.conf,hosts,hostname} generation is between PrepareRootfs and bind mounts — after rootfs exists but before any user-facing bind mounts (which might want to override these files via configmap).

实现计划
1. 创建一个新文件 
etcfiles.go
package cri

// writeContainerEtcFiles writes the standard /etc files into the container's
// merged rootfs. Called by CreateContainer right after PrepareRootfs.
//
//   /etc/resolv.conf  ← 关键：CoreDNS forward 插件需要它（指向真实上游 DNS）
//   /etc/hosts        ← 至少 127.0.0.1 + 自己的 IP/hostname
//   /etc/hostname     ← sandbox hostname
func writeContainerEtcFiles(mergedRoot, hostname, sandboxIP string,
    dns []string, search []string, options []string,
    extraHostAliases []*runtime.HostAlias) error
逻辑分三块：

resolv.conf：

nameserver 10.96.0.10              ← cluster DNS（kube-dns service IP，CRI Spec 里 kubelet 会传过来）
nameserver 8.8.8.8                 ← fallback 上游
search default.svc.cluster.local svc.cluster.local cluster.local
options ndots:5
但 CoreDNS 自己跑的时候不能用 cluster DNS（自己解析自己），这种情况要用宿主的 
resolv.conf
 内容。kubelet 的 PodSandboxConfig 里有 DnsConfig 字段——直接用就行。

hosts：

127.0.0.1   localhost
::1         localhost ip6-localhost ip6-loopback
10.244.0.5  pod-name      ← 自己的 IP + hostname
# user hostAliases:
1.2.3.4     foo.bar.com
hostname：直接写 sandbox 的 hostname。

2. 在 CreateContainer 里调用
// pkg/cri/container.go 的 CreateContainer，PrepareRootfs 之后：
sb := s.sandboxes.Get(req.PodSandboxId)   // 拿 sandbox 元数据
dns := req.SandboxConfig.DnsConfig
hostAliases := req.SandboxConfig.HostnameAliases  // 如果有

if err := writeContainerEtcFiles(
    mergedRoot,
    sb.Hostname,
    sb.IP,
    dns.GetServers(),
    dns.GetSearches(),
    dns.GetOptions(),
    hostAliases,
); err != nil {
    return nil, fmt.Errorf("write /etc files: %w", err)
}
3. 跑 kubeadm-e2e + 验证 CoreDNS Running
预期结果：

kube-system   coredns-xxx   1/1   Running   0   2m
并且能解析：

kubectl run -it --rm test --image=busybox -- nslookup kubernetes.default
# Server:    10.96.0.10
# Address:   10.96.0.10:53
# Name:      kubernetes.default.svc.cluster.local
# Address:   10.96.0.1
需要确认的细节
让我先把 PodSandboxConfig.DnsConfig 这个字段在咱们 CRI 里能不能拿到：

Searched workspace
Check DnsConfig usage in mydocker-cri.
Read file(s)

sandbox.go 1 - 56
RunPodSandbox 里目前没保存 hostname / DnsConfig。我得加：

sandbox.Metadata 加几个字段（Hostname / DnsServers / DnsSearches / DnsOptions / HostAliases）
RunPodSandbox 从 req.Config 里提取这几个
CreateContainer 调用一个新的 writeContainerEtcFiles(...)
整个改动量：

文件	改动	行数
sandbox.go
StartOptions 加 5 个字段	+5
sandbox_linux.go
把字段存进 Metadata	+5
sandbox.go
从 req 提取 + 传给 StartWithOptions	+12
etcfiles.go
新文件：写 resolv.conf / hosts / hostname	+80
container.go
CreateContainer 里调用	+10
单元测试	etcfiles_test.go	+60
大概 170 行。

我给你画个完整的 Roadmap
如果想把 [ ] CoreDNS / 自定义 Pod 这条勾上：

第 1 步（必做）：让 CoreDNS 跑起来
实现 T1+T2+T3——也就是在 CRI CreateContainer 时往容器 rootfs 里写：

resolv.conf
：用 sandbox config 里的 DnsServers + Searches + Options
/etc/hosts：localhost + Pod IP/hostname + HostAliases
/etc/hostname：sandbox hostname
预期效果：

kube-system  coredns-xxx     1/1  Running   ✅
这一步90% 概率就够。CoreDNS 一旦起来，整个 cluster DNS 就活了。

第 2 步（连带验证）：跑普通 Pod
kubectl run nginx --image=nginx
kubectl run -it test --image=busybox -- sh
# 在容器里：
nslookup kubernetes.default       # 应该解析到 10.96.0.1
nslookup nginx.default.svc.cluster.local
如果第 1 步成功，这个会通。如果不通，要看哪一环（kube-proxy iptables？CoreDNS forward？）。

第 3 步（可选 qol）：CRI 日志格式包装
现在 kubectl logs 看到一堆 "unsupported log format"。修起来要 50 行——加个 goroutine 把容器 stdout 包装成 CRI 格式。功能上不影响 Pod 跑，纯影响调试体验。

第 4 步（更后）：多节点 join
需要：

第二个 lima VM
在主节点 kubeadm token create --print-join-command
在 worker kubeadm join 到主节点
不动代码，只是流程验证。

我建议怎么走
要不要我先做第 1 步？大概 1-2 小时（实现 + 测试 + lima 内验证）。我按这个分两个 commit：

commit 1: feat(cri): generate /etc/{resolv.conf,hosts,hostname} in container rootfs

加 etcfiles.go + 测试
改 sandbox 元数据存 DNS/hostname
改 CreateContainer 调用
commit 2: docs: update milestones (CoreDNS resolved)

完成后 kubectl get pods -A 期望全 Running。