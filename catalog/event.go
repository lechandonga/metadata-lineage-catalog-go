package catalog

import "time"

// 事件类型。WAL 只追加这些不可变记录,重放顺序即提交顺序。
const (
	EvAssetRegistered = "asset_registered"
	EvAssetDeleted    = "asset_deleted"
	EvEdgeAdded       = "edge_added"
	EvEdgeRemoved     = "edge_removed"
)

// Event 是 WAL 中的一条提交记录。字段按事件类型复用。
type Event struct {
	Type      string    `json:"type"`
	Namespace string    `json:"namespace,omitempty"`
	Asset     string    `json:"asset,omitempty"`
	Version   string    `json:"version,omitempty"`
	Columns   []Column  `json:"columns,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	DeletedAt time.Time `json:"deleted_at,omitempty"`
	Source    *FieldRef `json:"source,omitempty"`
	Target    *FieldRef `json:"target,omitempty"`
}

// Snapshot 是持久化的完整状态快照。
type Snapshot struct {
	Assets map[string]*Asset `json:"assets"`
	Edges  []*Edge           `json:"edges"`
}

// Store 是持久化抽象:每次状态变更先以 fsync 追加 Event,
// 启动时通过 Load 重放;Compact 用完整快照截断历史。
type Store interface {
	Append(ev Event) error
	Load(apply func(Event) error) error
	Compact(snap Snapshot) error
}
