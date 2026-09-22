package pulse

// ciGateProbeGofmt 是 CI gofmt 门禁的探针文件：语法合法、vet 干净，但格式不合法。
// 引入它的分支只用于让门禁变红，验证完即删。
func ciGateProbeGofmt( a int ) int {
	return  a +
		a
}
