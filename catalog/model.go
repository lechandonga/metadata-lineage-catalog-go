// Package catalog 实现本地元数据目录的全部领域逻辑:
// 数据资产与 schema 版本登记、schema 兼容性判定、版本差异、
// 字段级血缘登记/撤销、下游影响面计算,以及基于 WAL + 快照的持久化。
package catalog

import "time"

// Column 描述 schema 中的一列。
type Column struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description,omitempty"`
}

// Version 是一个资产不可变的 schema 版本快照。
type Version struct {
	Version     string    `json:"version"`
	Columns     []Column  `json:"columns"`
	Fingerprint string    `json:"fingerprint"`
	CreatedAt   time.Time `json:"created_at"`
	// Deleted 为 true 时表示该版本处于被删除资产的历史中,数据仍可追溯。
	Deleted bool `json:"deleted,omitempty"`
	// DeletedAt 记录资产被删除的时间,用于历史展示。
	DeletedAt time.Time `json:"deleted_at,omitempty"`
}

// Asset 是数据资产实体。资产以 (namespace,name) 作为稳定业务键。
type Asset struct {
	Namespace string              `json:"namespace"`
	Name      string              `json:"name"`
	Latest    string              `json:"latest"`
	Versions  map[string]*Version `json:"versions"`
	CreatedAt time.Time           `json:"created_at"`
	UpdatedAt time.Time           `json:"updated_at"`
	Deleted   bool                `json:"deleted"`
	DeletedAt time.Time           `json:"deleted_at,omitempty"`
}

// Key 返回资产的全限定业务键 namespace/name。
func (a *Asset) Key() string { return a.Namespace + "/" + a.Name }

// FieldRef 精确定位一个字段。
type FieldRef struct {
	Namespace string `json:"namespace"`
	Asset     string `json:"asset"`
	Column    string `json:"column"`
}

func (f FieldRef) assetKey() string { return f.Namespace + "/" + f.Asset }
func (f FieldRef) String() string   { return f.Namespace + "/" + f.Asset + "." + f.Column }

// Edge 是一条字段级血缘依赖边:Source 字段的数据流向 Target 字段。
type Edge struct {
	Source    FieldRef  `json:"source"`
	Target    FieldRef  `json:"target"`
	CreatedAt time.Time `json:"created_at"`
}

// Incompatibility 描述一处 schema 破坏性变化。
type Incompatibility struct {
	Kind   string `json:"kind"` // removed_column / renamed_column / type_narrowed
	Column string `json:"column"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Reason string `json:"reason"`
}

// SchemaDiff 表示任意两个版本之间的差异。
type SchemaDiff struct {
	FromVersion string         `json:"from_version"`
	ToVersion   string         `json:"to_version"`
	Added       []Column       `json:"added"`
	Removed     []Column       `json:"removed"`
	Changed     []ColumnChange `json:"changed"`
	Compatible  bool           `json:"compatible"`
}

// ColumnChange 描述一列的类型变化。
type ColumnChange struct {
	Column string `json:"column"`
	From   string `json:"from"`
	To     string `json:"to"`
}

// ImpactResult 是下游影响面查询结果。
type ImpactResult struct {
	Start FieldRef `json:"start"`
	// Downstreams 为受影响字段(不含起点),按全限定名升序,与边登记顺序无关。
	Downstreams []FieldRef   `json:"downstreams"`
	Paths       []ImpactPath `json:"paths"`
}

// ImpactPath 解释一条影响是如何从起点传到某个下游字段的。
type ImpactPath struct {
	Destination FieldRef   `json:"destination"`
	Path        []FieldRef `json:"path"` // 含起点与终点
}
