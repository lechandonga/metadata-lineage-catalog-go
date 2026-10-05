package catalog

// 测试专用：直接在当前快照上注入“同一别名指向多个资产”的歧义状态。
// 正常业务流程中业务键唯一，不会自然产生该状态；该助手用于验证
// 解析层在面对歧义（含磁盘损坏引入的歧义）时必须拒绝并报
// ambiguous_key，而不是任意挑一个。
func (c *Catalog) testInjectAmbiguousAlias(alias string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := cloneSnapshot(c.snap)
	s.Aliases[alias] = append([]string(nil), ids...)
	c.snap = s
}

func (c *Catalog) testSnapshot() *snapshot { return c.current() }

func kindOf(err error) ErrorKind {
	if ce, ok := asCatalogError(err); ok {
		return ce.Kind
	}
	return ""
}

func changesOf(err error) []IncompatibleChange {
	if ce, ok := asCatalogError(err); ok {
		return ce.Changes
	}
	return nil
}
