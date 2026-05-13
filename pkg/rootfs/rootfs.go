// Package rootfs 负责把容器进程的根目录切换到自备 rootfs，
// 挂载容器视角下必要的特殊文件系统（/proc、/sys、/dev），以及额外的 bind mount。
package rootfs

// Mount 描述一个 bind mount（用户 -v 指定的 volume 或内部挂载）。
type Mount struct {
	Source   string // 宿主机路径（绝对路径）
	Target   string // 容器内路径（绝对路径，相对新根）
	ReadOnly bool
}

// Setup 在 **容器 init 进程内** 调用。
// 顺序：
//  1. pivot_root 到 newRoot
//  2. 挂 /proc /sys /dev
//  3. 依次处理 extraMounts（bind mount 用户 volume）
//
// 在非 Linux 平台上是 no-op，仅为语法检查服务。
func Setup(newRoot string, extraMounts []Mount) error {
	return setup(newRoot, extraMounts)
}
