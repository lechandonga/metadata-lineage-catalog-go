package catalog

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// clock 允许测试注入确定时间。
type clock func() time.Time

// RegisterResult 描述一次版本登记的判定依据,便于调用方与测试日志识别。
type RegisterResult struct {
	AssetKey   string `json:"asset_key"`
	Version    string `json:"version"`
	Outcome    string `json:"outcome"` // created / idempotent / redundant
	Compatible bool   `json:"compatible"`
}

// RemoveEdgeResult 描述撤销一条边的判定结果。
type RemoveEdgeResult struct {
	Existed bool   `json:"existed"`
	Outcome string `json:"outcome"` // removed / idempotent_not_found
}

// Catalog 是线程安全的元数据目录。所有变更串行化提交,
// 查询在 RLock 下读取一致快照;持久化在互斥区内 fsync 完成后才更新内存。
type Catalog struct {
	mu    sync.RWMutex
	store Store
	now   clock

	assets map[string]*Asset
	edges  map[string]*Edge // key: edgeKey
	// outAdj:fieldKey -> 邻接下游字段集合,用于影响面与环检测。
	outAdj map[string]map[string]FieldRef
}

// New 创建目录并从 store 重放历史。重放遇到损坏数据时按类型返回错误,
// 不会把坏数据装入内存。
func New(store Store) (*Catalog, error) {
	c := &Catalog{
		store:  store,
		now:    func() time.Time { return time.Now().UTC() },
		assets: map[string]*Asset{},
		edges:  map[string]*Edge{},
		outAdj: map[string]map[string]FieldRef{},
	}
	if err := c.replay(); err != nil {
		return nil, err
	}
	return c, nil
}

// NewForTest 允许注入时钟。
func NewForTest(store Store, now clock) (*Catalog, error) {
	c, err := New(store)
	if err != nil {
		return nil, err
	}
	if now != nil {
		c.now = now
	}
	return c, nil
}

func (c *Catalog) replay() error {
	return c.store.Load(func(ev Event) error {
		return c.apply(ev)
	})
}

// RegisterAsset 登记或演进一个资产版本。
//
//   - 相同 (业务键, 版本号, 内容) 重复上报:幂等成功。
//   - 相同版本号但内容不同:返回 KindConflict,绝不覆盖。
//   - 新版本:相对最新版本做兼容性判定;破坏性变化返回 KindIncompatibleSchema 且拒绝写入。
func (c *Catalog) RegisterAsset(namespace, name, version string, cols []Column) (*RegisterResult, error) {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	version = strings.TrimSpace(version)
	if namespace == "" || name == "" || version == "" {
		return nil, errf(KindValidation, "namespace、asset、version 均不能为空")
	}
	columns, verr := validateColumns(cols)
	if verr != nil {
		return nil, verr
	}
	fp := fingerprintColumns(columns)

	c.mu.Lock()
	defer c.mu.Unlock()

	key := namespace + "/" + name
	a := c.assets[key]
	if a != nil && a.Deleted {
		return nil, errf(KindDeleted, "资产 %s 已被删除,不能登记新版本;历史版本仍可查询", key)
	}

	ev := Event{
		Type:      EvAssetRegistered,
		Namespace: namespace,
		Asset:     name,
		Version:   version,
		Columns:   columns,
		CreatedAt: c.now(),
	}

	outcome := "created"
	if a == nil {
		if err := c.store.Append(ev); err != nil {
			return nil, err
		}
		c.apply(ev)
	} else {
		if existing, ok := a.Versions[version]; ok {
			if existing.Fingerprint != fp {
				return nil, &Error{
					Kind:    KindConflict,
					Message: fmt.Sprintf("资产 %s 的版本 %s 已存在且内容不同,历史版本不可变,拒绝覆盖", key, version),
					Details: map[string]string{"existing_fingerprint": existing.Fingerprint, "incoming_fingerprint": fp},
				}
			}
			return &RegisterResult{AssetKey: key, Version: version, Outcome: "idempotent", Compatible: true}, nil
		}
		// 新版本:对照当前最新版本判定兼容性。
		latest := a.Versions[a.Latest]
		d := diffColumns(latest.Columns, columns)
		if !d.Compatible {
			incompat := detectIncompatibilities(d)
			return nil, &Error{
				Kind:    KindIncompatibleSchema,
				Message: fmt.Sprintf("资产 %s 版本 %s -> %s 存在破坏性 schema 变化,共 %d 处,已拒绝", key, latest.Version, version, len(incompat)),
				Details: incompat,
			}
		}
		if fp == latest.Fingerprint {
			outcome = "redundant" // 内容与最新版完全相同,仅版本号不同
		}
		if err := c.store.Append(ev); err != nil {
			return nil, err
		}
		c.apply(ev)
	}
	return &RegisterResult{AssetKey: key, Version: version, Outcome: outcome, Compatible: true}, nil
}

// DeleteAsset 逻辑删除资产(打墓碑)。删除幂等;历史版本与历史血缘保留可查。
func (c *Catalog) DeleteAsset(namespace, name string) error {
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	if namespace == "" || name == "" {
		return errf(KindValidation, "namespace、asset 均不能为空")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := namespace + "/" + name
	a := c.assets[key]
	if a == nil {
		return errf(KindNotFound, "资产 %s 不存在", key)
	}
	if a.Deleted {
		return nil
	}
	ev := Event{Type: EvAssetDeleted, Namespace: namespace, Asset: name, DeletedAt: c.now()}
	if err := c.store.Append(ev); err != nil {
		return err
	}
	c.apply(ev)
	return nil
}

// GetAsset 读取资产(含全部历史版本)。已删除资产仍可读取,Deleted 字段标识墓碑。
func (c *Catalog) GetAsset(ref AssetRef) (*Asset, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, a, cerr := c.resolveHistoryLocked(ref)
	if cerr != nil {
		return nil, cerr
	}
	return cloneAsset(a), nil
}

// GetVersion 读取资产的指定历史版本。
func (c *Catalog) GetVersion(ref AssetRef, version string) (*Version, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	version = strings.TrimSpace(version)
	if version == "" {
		return nil, errf(KindValidation, "version 不能为空")
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	key, a, cerr := c.resolveHistoryLocked(ref)
	if cerr != nil {
		return nil, cerr
	}
	v, ok := a.Versions[version]
	if !ok {
		return nil, errf(KindNotFound, "资产 %s 不存在版本 %s", key, version)
	}
	return cloneVersion(v), nil
}

// DiffVersions 返回资产任意两个已登记版本之间加了什么、删了什么、改了什么。
func (c *Catalog) DiffVersions(ref AssetRef, fromV, toV string) (*SchemaDiff, error) {
	if err := ref.validate(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, a, cerr := c.resolveHistoryLocked(ref)
	if cerr != nil {
		return nil, cerr
	}
	from, ok1 := a.Versions[strings.TrimSpace(fromV)]
	to, ok2 := a.Versions[strings.TrimSpace(toV)]
	if !ok1 || !ok2 {
		return nil, errf(KindNotFound, "版本不存在: from=%s(存在=%v) to=%s(存在=%v)", fromV, ok1, toV, ok2)
	}
	d := diffColumns(from.Columns, to.Columns)
	d.FromVersion = from.Version
	d.ToVersion = to.Version
	return &d, nil
}

// ListAssets 列出全部资产;includeDeleted 控制是否包含墓碑资产。结果按键排序。
func (c *Catalog) ListAssets(includeDeleted bool) []*Asset {
	c.mu.RLock()
	defer c.mu.RUnlock()
	keys := make([]string, 0, len(c.assets))
	for k, a := range c.assets {
		if includeDeleted || !a.Deleted {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := make([]*Asset, 0, len(keys))
	for _, k := range keys {
		out = append(out, cloneAsset(c.assets[k]))
	}
	return out
}

// AddEdge 登记一条字段级血缘边。
//
//   - 端点资产或字段不存在:KindDangling。
//   - 短名定位到多个 namespace 的资产:KindAmbiguous。
//   - 引用已删除资产:KindDeleted。
//   - 自环:KindConflict;会成环:KindCycle。
//   - 完全相同的边重复登记:幂等成功。
func (c *Catalog) AddEdge(source, target FieldRef) error {
	if err := source.validate(); err != nil {
		return err
	}
	if err := target.validate(); err != nil {
		return err
	}
	if source.String() == target.String() {
		return &Error{Kind: KindConflict, Message: "不允许自环: " + source.String()}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, err := c.resolveFieldLocked(source); err != nil {
		return err
	}
	if _, err := c.resolveFieldLocked(target); err != nil {
		return err
	}
	ek := edgeKey(source, target)
	if _, ok := c.edges[ek]; ok {
		return nil // 幂等
	}
	// 环检测:若 source 已在 target 的下游可达,新边会使 target->...->source->target 闭环。
	if c.reachableLocked(target, source) {
		return &Error{Kind: KindCycle, Message: fmt.Sprintf("添加边 %s -> %s 会形成有向环,已拒绝", source, target)}
	}
	ev := Event{Type: EvEdgeAdded, Source: &source, Target: &target, CreatedAt: c.now()}
	if err := c.store.Append(ev); err != nil {
		return err
	}
	c.apply(ev)
	return nil
}

// RemoveEdge 撤销一条血缘边。边不存在时返回确定的幂等结果而非错误。
func (c *Catalog) RemoveEdge(source, target FieldRef) (*RemoveEdgeResult, error) {
	if err := source.validate(); err != nil {
		return nil, err
	}
	if err := target.validate(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	ek := edgeKey(source, target)
	if _, ok := c.edges[ek]; !ok {
		return &RemoveEdgeResult{Existed: false, Outcome: "idempotent_not_found"}, nil
	}
	ev := Event{Type: EvEdgeRemoved, Source: &source, Target: &target, CreatedAt: c.now()}
	if err := c.store.Append(ev); err != nil {
		return nil, err
	}
	c.apply(ev)
	return &RemoveEdgeResult{Existed: true, Outcome: "removed"}, nil
}

// ListEdges 返回全部现存边,按 source、target 排序。
func (c *Catalog) ListEdges() []*Edge {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]*Edge, 0, len(c.edges))
	for _, e := range c.edges {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Source.String() != out[j].Source.String() {
			return out[i].Source.String() < out[j].Source.String()
		}
		return out[i].Target.String() < out[j].Target.String()
	})
	return out
}

// Impact 查询一个字段的下游影响面。结果与边的登记顺序无关:
// 邻接按字段全限定名排序遍历;同时给出到每个下游字段的一条最短、
// 字典序最小的具体路径。环与重复边不会导致挂死或重复结果。
func (c *Catalog) Impact(start FieldRef) (*ImpactResult, error) {
	if err := start.validate(); err != nil {
		return nil, err
	}
	c.mu.RLock()
	defer c.mu.RUnlock()

	if _, err := c.resolveFieldLocked(start); err != nil {
		return nil, err
	}

	sortedAdj := func(f FieldRef) []FieldRef {
		nbs := c.outAdj[fieldKey(f)]
		out := make([]FieldRef, 0, len(nbs))
		for _, r := range nbs {
			out = append(out, r)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
		return out
	}

	// BFS:parent 记录每个节点被首次发现时的前驱(最短 + 字典序最小)。
	parent := map[string]FieldRef{fieldKey(start): start}
	dist := map[string]int{fieldKey(start): 0}
	nodes := map[string]FieldRef{fieldKey(start): start}
	queue := []FieldRef{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, nb := range sortedAdj(cur) {
			nk := fieldKey(nb)
			if _, seen := dist[nk]; !seen {
				dist[nk] = dist[fieldKey(cur)] + 1
				parent[nk] = cur
				nodes[nk] = nb
				queue = append(queue, nb)
			}
		}
	}

	result := &ImpactResult{Start: start, Downstreams: []FieldRef{}, Paths: []ImpactPath{}}
	keys := make([]string, 0, len(dist)-1)
	for k := range dist {
		if k != fieldKey(start) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		dst := nodes[k]
		result.Downstreams = append(result.Downstreams, dst)
		// 沿 parent 回溯出路径。
		chain := []FieldRef{dst}
		for cur := k; parent[cur] != start; {
			p := parent[cur]
			chain = append(chain, p)
			cur = fieldKey(p)
		}
		chain = append(chain, start)
		// 反转为起点 -> 终点。
		for i, j := 0, len(chain)-1; i < j; i, j = i+1, j-1 {
			chain[i], chain[j] = chain[j], chain[i]
		}
		result.Paths = append(result.Paths, ImpactPath{Destination: dst, Path: chain})
	}
	return result, nil
}

// reachableLocked 判断从 from 出发能否沿现存边到达 to(含 from==to)。
// 使用访问集合,环不会造成死循环。调用方须持有 c.mu。
func (c *Catalog) reachableLocked(from, to FieldRef) bool {
	seen := map[string]bool{fieldKey(from): true}
	stack := []FieldRef{from}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur.String() == to.String() {
			return true
		}
		for _, nb := range c.outAdj[fieldKey(cur)] {
			nk := fieldKey(nb)
			if !seen[nk] {
				seen[nk] = true
				stack = append(stack, nb)
			}
		}
	}
	return false
}

// resolveKeyLocked 按业务键定位资产:
//   - namespace 非空:精确定位,不存在报 KindNotFound,已删除报 KindDeleted。
//   - namespace 为空:在所有未删除资产中按短名查找;
//     命中多个报 KindAmbiguous;零命中报 KindNotFound。
func (c *Catalog) resolveKeyLocked(ref AssetRef) (string, *Error) {
	if ref.Namespace != "" {
		key := ref.Namespace + "/" + ref.Asset
		a, ok := c.assets[key]
		if !ok {
			return "", errf(KindNotFound, "资产 %s 不存在", key)
		}
		if a.Deleted {
			return "", errf(KindDeleted, "资产 %s 已被删除", key)
		}
		return key, nil
	}
	var matches []string
	for key, a := range c.assets {
		if !a.Deleted && a.Name == ref.Asset {
			matches = append(matches, key)
		}
	}
	sort.Strings(matches)
	switch len(matches) {
	case 0:
		return "", errf(KindNotFound, "未找到名为 %q 的资产", ref.Asset)
	case 1:
		return matches[0], nil
	default:
		return "", &Error{
			Kind:    KindAmbiguous,
			Message: fmt.Sprintf("短名 %q 命中多个资产,请使用 namespace 限定:%v", ref.Asset, matches),
			Details: matches,
		}
	}
}

// resolveHistoryLocked 用于历史读取:全限定键可以读到墓碑资产的历史;
// 短名查找仍只在未删除资产中进行(保持与实时解析一致的歧义语义)。
func (c *Catalog) resolveHistoryLocked(ref AssetRef) (string, *Asset, *Error) {
	if ref.Namespace != "" {
		key := ref.Namespace + "/" + ref.Asset
		a, ok := c.assets[key]
		if !ok {
			return "", nil, errf(KindNotFound, "资产 %s 不存在", key)
		}
		return key, a, nil
	}
	key, cerr := c.resolveKeyLocked(ref)
	if cerr != nil {
		return "", nil, cerr
	}
	return key, c.assets[key], nil
}

// resolveFieldLocked 解析血缘端点。资产层面的歧义/缺失由 resolveKeyLocked 区分;
// 资产存在但字段不存在时报 KindDangling;引用已删除资产报 KindDeleted。
func (c *Catalog) resolveFieldLocked(f FieldRef) (*Asset, *Error) {
	ref := AssetRef{Namespace: f.Namespace, Asset: f.Asset}
	key, cerr := c.resolveKeyLocked(ref)
	if cerr != nil {
		// 血缘端点处:资产不存在属于"悬空引用",与普通查询的 not_found 区分开。
		if cerr.Kind == KindNotFound {
			return nil, &Error{
				Kind:    KindDangling,
				Message: fmt.Sprintf("血缘端点引用了不存在的资产 %s(悬空引用)", f.assetKey()),
				Details: f.String(),
			}
		}
		return nil, cerr
	}
	a := c.assets[key]
	v := a.Versions[a.Latest]
	for _, col := range v.Columns {
		if col.Name == f.Column {
			return a, nil
		}
	}
	return nil, &Error{
		Kind:    KindDangling,
		Message: fmt.Sprintf("资产 %s 的最新版本 %s 中不存在字段 %q(悬空引用)", key, a.Latest, f.Column),
		Details: f.String(),
	}
}

// apply 在内存中应用一条已提交事件。只做结构性变更,不做业务校验
// (校验发生在登记路径;重放时事件已被持久化,视为已提交事实)。
func (c *Catalog) apply(ev Event) error {
	switch ev.Type {
	case EvAssetRegistered:
		key := ev.Namespace + "/" + ev.Asset
		a := c.assets[key]
		if a == nil {
			a = &Asset{
				Namespace: ev.Namespace,
				Name:      ev.Asset,
				Versions:  map[string]*Version{},
				CreatedAt: ev.CreatedAt,
			}
			c.assets[key] = a
		}
		if _, exists := a.Versions[ev.Version]; !exists {
			cols := make([]Column, len(ev.Columns))
			copy(cols, ev.Columns)
			a.Versions[ev.Version] = &Version{
				Version:     ev.Version,
				Columns:     cols,
				Fingerprint: fingerprintColumns(ev.Columns),
				CreatedAt:   ev.CreatedAt,
			}
			a.Latest = ev.Version // 重放顺序即提交顺序,最后一个即最新
		}
		a.UpdatedAt = ev.CreatedAt
	case EvAssetDeleted:
		key := ev.Namespace + "/" + ev.Asset
		a := c.assets[key]
		if a == nil {
			return errf(KindCorrupt, "WAL 中 asset_deleted 事件引用了不存在的资产 %s", key)
		}
		a.Deleted = true
		a.DeletedAt = ev.DeletedAt
		for _, v := range a.Versions {
			v.Deleted = true
			v.DeletedAt = ev.DeletedAt
		}
	case EvEdgeAdded:
		if ev.Source == nil || ev.Target == nil {
			return errf(KindCorrupt, "WAL 中 edge_added 事件缺少端点")
		}
		ek := edgeKey(*ev.Source, *ev.Target)
		if _, ok := c.edges[ek]; !ok {
			c.edges[ek] = &Edge{Source: *ev.Source, Target: *ev.Target, CreatedAt: ev.CreatedAt}
			sk := fieldKey(*ev.Source)
			if c.outAdj[sk] == nil {
				c.outAdj[sk] = map[string]FieldRef{}
			}
			c.outAdj[sk][fieldKey(*ev.Target)] = *ev.Target
		}
	case EvEdgeRemoved:
		if ev.Source == nil || ev.Target == nil {
			return errf(KindCorrupt, "WAL 中 edge_removed 事件缺少端点")
		}
		ek := edgeKey(*ev.Source, *ev.Target)
		delete(c.edges, ek)
		if nbs := c.outAdj[fieldKey(*ev.Source)]; nbs != nil {
			delete(nbs, fieldKey(*ev.Target))
		}
	default:
		return errf(KindCorrupt, "WAL 含无法识别的事件类型 %q", ev.Type)
	}
	return nil
}

// Snapshot 返回当前状态的深拷贝,供压缩落盘使用。调用期间持写锁。
func (c *Catalog) snapshotLocked() Snapshot {
	assets := make(map[string]*Asset, len(c.assets))
	for k, a := range c.assets {
		assets[k] = cloneAsset(a)
	}
	edges := make([]*Edge, 0, len(c.edges))
	for _, e := range c.edges {
		cp := *e
		edges = append(edges, &cp)
	}
	return Snapshot{Assets: assets, Edges: edges}
}

// Compact 在写锁下用完整快照压缩持久化日志。
func (c *Catalog) Compact() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.store.Compact(c.snapshotLocked())
}

func edgeKey(s, t FieldRef) string { return s.String() + "=>" + t.String() }

func fieldKey(f FieldRef) string { return f.String() }

func cloneVersion(v *Version) *Version {
	cp := *v
	cp.Columns = make([]Column, len(v.Columns))
	copy(cp.Columns, v.Columns)
	return &cp
}

func cloneAsset(a *Asset) *Asset {
	cp := *a
	cp.Versions = make(map[string]*Version, len(a.Versions))
	for k, v := range a.Versions {
		cp.Versions[k] = cloneVersion(v)
	}
	return &cp
}
