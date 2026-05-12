package sandbox

// SaveForTest 把一个 Sandbox 原样写到 Manager 的根目录。
// 仅供其他包的测试使用；避免跨包访问 Manager.save（未导出）。
// 注意：它绕过了所有业务校验，不要在生产代码里使用。
func SaveForTest(m *Manager, sb *Sandbox) error {
	return m.save(sb)
}
