package catalog

// MemStore 是纯内存 Store,供单元测试使用(不落盘、无 fsync)。
type MemStore struct {
	events   []Event
	failNext bool
}

// NewMemStore 创建内存 Store。
func NewMemStore() *MemStore { return &MemStore{} }

// Append 模拟提交。failNext 打开时下一次写入返回错误且不保留事件,
// 用于验证"写失败不产生重复版本/重复边"。
func (m *MemStore) Append(ev Event) error {
	if m.failNext {
		m.failNext = false
		return errf(KindCorrupt, "模拟的提交失败")
	}
	m.events = append(m.events, ev)
	return nil
}

// Load 按提交顺序重放。
func (m *MemStore) Load(apply func(Event) error) error {
	for _, ev := range m.events {
		if err := apply(ev); err != nil {
			return err
		}
	}
	return nil
}

// Compact 用快照重置事件流(展开成登记事件,保持接口语义一致)。
func (m *MemStore) Compact(snap Snapshot) error {
	m.events = nil
	return applySnapshot(&snap, func(ev Event) error {
		m.events = append(m.events, ev)
		return nil
	})
}

// FailNextAppend 安排下一次 Append 失败。
func (m *MemStore) FailNextAppend() { m.failNext = true }

// EventCount 返回已提交事件数(测试断言用)。
func (m *MemStore) EventCount() int { return len(m.events) }
