package catalog

import (
	"fmt"
	"sort"
)

// EdgeInput 是血缘登记/撤销请求，端点按 业务键+列名 精确定位。
type EdgeInput struct {
	SrcAssetKey string `json:"src_asset_key"`
	SrcColumn   string `json:"src_column"`
	DstAssetKey string `json:"dst_asset_key"`
	DstColumn   string `json:"dst_column"`
}

func (e EdgeInput) validate() error {
	if e.SrcAssetKey == "" || e.DstAssetKey == "" {
		return &ValidationError{Reason: "血缘两端的 asset_key 不能为空"}
	}
	if e.SrcColumn == "" || e.DstColumn == "" {
		return &ValidationError{Reason: "血缘必须精确到字段，column 不能为空"}
	}
	return nil
}

// PutEdge 登记一条字段级血缘边（src 字段产出 dst 字段）。
//
// 端点必须精确解析到存在且未删除的资产，字段必须存在于该资产最新
// 版本 schema 中。失败类型严格区分：
//   - ambiguous_key：业务键有歧义；
//   - not_found：资产不存在；
//   - dangling_reference：资产存在但字段不存在（悬空字段引用）。
//
// 重复登记幂等；自环拒绝；普通环允许（影响面遍历按访问集合去重，
// 不会挂死也不会重复计数）。
func (c *Catalog) PutEdge(in EdgeInput) (*Edge, bool, error) {
	if err := in.validate(); err != nil {
		return nil, false, err
	}
	if in.SrcAssetKey == in.DstAssetKey && in.SrcColumn == in.DstColumn {
		return nil, false, &ValidationError{Reason: "自环边不允许：源与目的是同一字段"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	srcID, dstID, err := c.resolveEndpointsLocked(in)
	if err != nil {
		return nil, false, err
	}
	canonical := Edge{
		SrcAssetKey: c.snap.Assets[srcID].Key,
		SrcColumn:   in.SrcColumn,
		DstAssetKey: c.snap.Assets[dstID].Key,
		DstColumn:   in.DstColumn,
	}
	fp := edgeFingerprint(canonical)
	if c.snap.edgeStore.has(fp) {
		// 重复边幂等：从有序存储中取回原记录。
		for _, e := range c.snap.Edges {
			if edgeFingerprint(e) == fp {
				ee := e
				return &ee, false, nil
			}
		}
	}
	canonical.CreateTime = c.now()
	s := cloneSnapshot(c.snap)
	s.edgeStore = s.edgeStore.add(canonical)
	s.Edges = s.edgeStore.edges
	c.snap = s
	if err := c.persistLocked(); err != nil {
		c.rollbackLoad()
		return nil, false, err
	}
	e := canonical
	return &e, true, nil
}

// DeleteEdge 撤销一条血缘边。边不存在视为已撤销，返回 deleted=false
// 且不报错：撤销是幂等的，结果确定。
func (c *Catalog) DeleteEdge(in EdgeInput) (bool, error) {
	if err := in.validate(); err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	srcID, dstID, err := c.resolveEndpointsLocked(in, allowDeleted())
	if err != nil {
		return false, err
	}
	canonical := Edge{
		SrcAssetKey: c.snap.Assets[srcID].Key,
		SrcColumn:   in.SrcColumn,
		DstAssetKey: c.snap.Assets[dstID].Key,
		DstColumn:   in.DstColumn,
	}
	fp := edgeFingerprint(canonical)
	ns, removed := c.snap.edgeStore.remove(fp)
	if !removed {
		return false, nil // 不存在 = 已经是目标状态
	}
	s := cloneSnapshot(c.snap)
	s.edgeStore = ns
	s.Edges = ns.edges
	c.snap = s
	if err := c.persistLocked(); err != nil {
		c.rollbackLoad()
		return false, err
	}
	return true, nil
}

type resolveOpt struct{ allowDeleted bool }

type resolveOption func(*resolveOpt)

func allowDeleted() resolveOption {
	return func(o *resolveOpt) { o.allowDeleted = true }
}

func (c *Catalog) resolveEndpointsLocked(in EdgeInput, opts ...resolveOption) (string, string, error) {
	o := resolveOpt{}
	for _, opt := range opts {
		opt(&o)
	}
	srcID, err := c.resolveLocked(in.SrcAssetKey)
	if err != nil {
		return "", "", err
	}
	dstID, err := c.resolveLocked(in.DstAssetKey)
	if err != nil {
		return "", "", err
	}
	if !o.allowDeleted {
		if c.snap.Assets[srcID].Deleted || c.snap.Assets[dstID].Deleted {
			return "", "", &CatalogError{Kind: ErrConflict, Reason: "血缘端点资产已删除，不能新增边（历史边仍保留）"}
		}
	}
	var missing []string
	if !columnExists(c.snap, srcID, in.SrcColumn) {
		missing = append(missing, in.SrcAssetKey+"."+in.SrcColumn)
	}
	if !columnExists(c.snap, dstID, in.DstColumn) {
		missing = append(missing, in.DstAssetKey+"."+in.DstColumn)
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return "", "", &CatalogError{
			Kind:    ErrDanglingReference,
			Reason:  "血缘指向不存在的字段",
			Details: missing,
		}
	}
	return srcID, dstID, nil
}

// columnExists 检查字段是否存在于资产最新版本 schema 中。
func columnExists(s *snapshot, assetID, col string) bool {
	versions := s.Versions[assetID]
	if len(versions) == 0 {
		return false
	}
	for _, c := range versions[len(versions)-1].Columns {
		if c.Name == col {
			return true
		}
	}
	return false
}

// ListEdges 返回全部血缘边，按 (src, dst) 规范化排序。
func (c *Catalog) ListEdges() []Edge {
	s := c.current()
	return append([]Edge(nil), s.Edges...) // 已是指纹字典序，顺序稳定
}

// Impact 查询给定字段的下游影响面：沿血缘边正向（src -> dst）
// BFS，返回除源字段外所有可达字段，顺序稳定（按 asset_key,column
// 字典序），与边的登记顺序无关；每个受影响字段附一条从源出发的
// 最短路径，解释影响如何传播。环、自环通过访问集合天然处理，
// 不会挂死或重复计数。
func (c *Catalog) Impact(assetKey, column string) (*ImpactResult, error) {
	s := c.current()
	id, err := c.resolveWith(s, assetKey)
	if err != nil {
		return nil, err
	}
	if !columnExists(s, id, column) {
		return nil, &CatalogError{
			Kind:    ErrDanglingReference,
			Reason:  "源字段不存在",
			Details: []string{assetKey + "." + column},
		}
	}
	srcKey := s.Assets[id].Key
	source := ImpactNode{AssetKey: srcKey, Column: column}

	// 邻接表：node -> 直接下游（排序保证路径构造稳定）。
	adj := map[ImpactNode][]ImpactNode{}
	for _, e := range s.Edges {
		from := ImpactNode{AssetKey: e.SrcAssetKey, Column: e.SrcColumn}
		to := ImpactNode{AssetKey: e.DstAssetKey, Column: e.DstColumn}
		adj[from] = append(adj[from], to)
	}
	for k := range adj {
		ns := adj[k]
		sort.Slice(ns, func(i, j int) bool {
			if ns[i].AssetKey != ns[j].AssetKey {
				return ns[i].AssetKey < ns[j].AssetKey
			}
			return ns[i].Column < ns[j].Column
		})
		adj[k] = ns
	}

	parent := map[ImpactNode]ImpactNode{}
	visited := map[ImpactNode]bool{source: true}
	queue := []ImpactNode{source}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if visited[next] {
				continue
			}
			visited[next] = true
			parent[next] = cur
			queue = append(queue, next)
		}
	}

	var impacted []ImpactNode
	for n := range visited {
		if n != source {
			impacted = append(impacted, n)
		}
	}
	sort.Slice(impacted, func(i, j int) bool {
		if impacted[i].AssetKey != impacted[j].AssetKey {
			return impacted[i].AssetKey < impacted[j].AssetKey
		}
		return impacted[i].Column < impacted[j].Column
	})

	entries := make([]ImpactEntry, 0, len(impacted))
	for _, n := range impacted {
		// 从 n 沿 parent 回溯到 source，再反转。
		path := []ImpactNode{n}
		for cur := n; cur != source; {
			p, ok := parent[cur]
			if !ok {
				return nil, &CatalogError{Kind: ErrCorruptStore, Reason: fmt.Sprintf("影响面遍历内部错误：%s 缺少父节点", cur.Column)}
			}
			path = append(path, p)
			cur = p
		}
		for i, j := 0, len(path)-1; i < j; i, j = i+1, j-1 {
			path[i], path[j] = path[j], path[i]
		}
		entries = append(entries, ImpactEntry{Node: n, Path: path})
	}
	return &ImpactResult{Source: source, Impact: entries}, nil
}
