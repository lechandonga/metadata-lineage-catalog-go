package catalog

import (
	"sort"
	"time"
)

// Column 描述 schema 中的一列。
//
// Type 使用平台自有的可比较类型名（见 types.go 中的类型层级），
// Nullable=false 表示 NOT NULL。
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Doc      string `json:"doc,omitempty"`
}

// Asset 是按稳定业务键定位的数据资产。
type Asset struct {
	ID         string    `json:"id"`
	Key        string    `json:"key"`
	Name       string    `json:"name,omitempty"`
	Kind       string    `json:"kind,omitempty"` // 例如 table / topic / dashboard
	CreateTime time.Time `json:"create_time"`
	UpdateTime time.Time `json:"update_time"`
	Deleted    bool      `json:"deleted"`
	DeleteTime time.Time `json:"delete_time,omitempty"`
}

// SchemaVersion 是资产某一版本 schema 的不可变快照。
type SchemaVersion struct {
	AssetID     string    `json:"asset_id"`
	Version     int       `json:"version"`
	Columns     []Column  `json:"columns"`
	ContentHash string    `json:"content_hash"`
	CreateTime  time.Time `json:"create_time"`
}

// Edge 是字段级血缘依赖边：src 字段产出 dst 字段。
type Edge struct {
	SrcAssetKey string    `json:"src_asset_key"`
	SrcColumn   string    `json:"src_column"`
	DstAssetKey string    `json:"dst_asset_key"`
	DstColumn   string    `json:"dst_column"`
	CreateTime  time.Time `json:"create_time"`
}

// IncompatibleChange 描述一处破坏性 schema 变化。
type IncompatibleChange struct {
	Kind   string `json:"kind"`   // dropped_column / narrowed_type / renamed_column / nullability_tightened
	Column string `json:"column"` // 新版本中的列名（rename 时为新名）
	Old    string `json:"old,omitempty"`
	New    string `json:"new,omitempty"`
	Reason string `json:"reason"`
}

// VersionDiff 描述任意两个版本之间的差异。
type VersionDiff struct {
	FromVersion int            `json:"from_version"`
	ToVersion   int            `json:"to_version"`
	Added       []Column       `json:"added"`
	Removed     []Column       `json:"removed"`
	Changed     []ColumnChange `json:"changed"`
}

// ColumnChange 描述同名列在两个版本间的变化。
type ColumnChange struct {
	Name        string `json:"name"`
	OldType     string `json:"old_type"`
	NewType     string `json:"new_type"`
	OldNullable bool   `json:"old_nullable"`
	NewNullable bool   `json:"new_nullable"`
}

// ImpactNode 是影响面结果中的一个字段节点。
type ImpactNode struct {
	AssetKey string `json:"asset_key"`
	Column   string `json:"column"`
}

// ImpactResult 是字段下游影响面查询结果：按字段去重、顺序稳定，
// 并给出从源字段到每个受影响字段的一条最短路径（用于解释传播链路）。
type ImpactResult struct {
	Source ImpactNode    `json:"source"`
	Impact []ImpactEntry `json:"impact"`
}

// ImpactEntry 为一个受影响字段及一条具体传播路径。
type ImpactEntry struct {
	Node ImpactNode   `json:"node"`
	Path []ImpactNode `json:"path"`
}

// snapshot 是 catalog 的完整状态。catalog 的可变状态始终只通过
// 替换 *snapshot 指针来更新，因此读操作拿到的永远是完整快照。
type snapshot struct {
	SchemaVersion int                         `json:"schema_version"` // 存储信封版本，当前为 1
	Assets        map[string]*Asset           `json:"assets"`         // id -> asset
	ByKey         map[string]string           `json:"by_key"`         // key -> id
	Aliases       map[string][]string         `json:"aliases"`        // 别名（曾用 key）-> id 列表
	Versions      map[string][]*SchemaVersion `json:"versions"`       // assetID -> 版本（升序）
	Edges         []Edge                      `json:"edges"`          // 不可变有序切片，整体替换
	edgeStore     *edgeStore                  `json:"-"`              // 与 Edges 同构的有序判重结构
	NextAssetSeq  int                         `json:"next_asset_seq"`
}

// edgeStore 是不可变的边集合：Edges 始终按指纹字典序排序，
// 判重用二分查找 O(log E)，增删产生新切片（O(E) 复制，单次约
// 数 MB / 万级边，可接受）；整体随 snapshot 指针一次性发布，
// 读者拿到旧快照后无锁遍历也不会看到写一半的状态。
type edgeStore struct {
	edges []Edge // 已按 edgeFingerprint 升序排序
}

func newEdgeStore(es []Edge) *edgeStore {
	out := append([]Edge(nil), es...)
	sort.Slice(out, func(i, j int) bool { return edgeFingerprint(out[i]) < edgeFingerprint(out[j]) })
	return &edgeStore{edges: out}
}

func (x *edgeStore) has(fp string) bool {
	i := sort.Search(len(x.edges), func(i int) bool { return edgeFingerprint(x.edges[i]) >= fp })
	return i < len(x.edges) && edgeFingerprint(x.edges[i]) == fp
}

func (x *edgeStore) add(e Edge) *edgeStore {
	fp := edgeFingerprint(e)
	i := sort.Search(len(x.edges), func(i int) bool { return edgeFingerprint(x.edges[i]) >= fp })
	ns := &edgeStore{edges: make([]Edge, 0, len(x.edges)+1)}
	ns.edges = append(ns.edges, x.edges[:i]...)
	ns.edges = append(ns.edges, e)
	ns.edges = append(ns.edges, x.edges[i:]...)
	return ns
}

func (x *edgeStore) remove(fp string) (*edgeStore, bool) {
	i := sort.Search(len(x.edges), func(i int) bool { return edgeFingerprint(x.edges[i]) >= fp })
	if i >= len(x.edges) || edgeFingerprint(x.edges[i]) != fp {
		return x, false
	}
	ns := &edgeStore{edges: make([]Edge, 0, len(x.edges)-1)}
	ns.edges = append(ns.edges, x.edges[:i]...)
	ns.edges = append(ns.edges, x.edges[i+1:]...)
	return ns, true
}
