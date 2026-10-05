package catalog

import (
	"fmt"
	"sort"
	"sync"
	"time"
)

// Clock 可被测试替换。
type Clock func() time.Time

// Catalog 是内存态元数据目录。所有可变状态都保存在单个不可变
// snapshot 指针背后：写操作在锁内构造新快照并整体替换，读操作
// RLock 后取出快照指针即可在锁外安全遍历，因此读到的必然是
// 某一次完整提交，而不会看到写了一半的状态。
type Catalog struct {
	mu    sync.RWMutex
	snap  *snapshot
	now   Clock
	store Store // 为 nil 时目录只在内存中工作
}

// New 打开目录：若 store 中有数据则先加载并校验，否则初始化空目录。
func New(store Store, now Clock) (*Catalog, error) {
	if now == nil {
		now = time.Now
	}
	c := &Catalog{store: store, now: now}
	if store != nil {
		s, err := store.Load()
		if err != nil {
			return nil, err
		}
		if s != nil {
			normalize(s)
			if err := validateSnapshot(s); err != nil {
				return nil, err
			}
			c.snap = s
			return c, nil
		}
	}
	c.snap = emptySnapshot()
	return c, nil
}

func emptySnapshot() *snapshot {
	s := &snapshot{
		SchemaVersion: 1,
		Assets:        map[string]*Asset{},
		ByKey:         map[string]string{},
		Aliases:       map[string][]string{},
		Versions:      map[string][]*SchemaVersion{},
	}
	s.edgeStore = newEdgeStore(s.Edges)
	return s
}

func normalize(s *snapshot) {
	if s.Assets == nil {
		s.Assets = map[string]*Asset{}
	}
	if s.ByKey == nil {
		s.ByKey = map[string]string{}
	}
	if s.Aliases == nil {
		s.Aliases = map[string][]string{}
	}
	if s.Versions == nil {
		s.Versions = map[string][]*SchemaVersion{}
	}
	s.edgeStore = newEdgeStore(s.Edges)
	s.Edges = s.edgeStore.edges
}

func (c *Catalog) current() *snapshot {
	c.mu.RLock()
	s := c.snap
	c.mu.RUnlock()
	return s
}

// RegisterAssetInput 是资产登记请求。
type RegisterAssetInput struct {
	Key  string `json:"key"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// RegisterAsset 按稳定业务键登记资产。重复上报相同内容幂等；
// 名称/类型等元数据变化属于正常更新（不产生 schema 新版本）。
func (c *Catalog) RegisterAsset(in RegisterAssetInput) (*Asset, bool, error) {
	if in.Key == "" {
		return nil, false, &ValidationError{Reason: "资产业务键 key 不能为空"}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	id, ok := c.snap.ByKey[in.Key]
	if ok {
		a := c.snap.Assets[id]
		if a.Deleted {
			return nil, false, &CatalogError{Kind: ErrConflict, Reason: "业务键 " + in.Key + " 指向的资产已删除，不允许在其之上修改"}
		}
		s := cloneSnapshot(c.snap)
		aa := *a
		aa.Name = in.Name
		aa.Kind = in.Kind
		aa.UpdateTime = c.now()
		s.Assets[id] = &aa
		c.snap = s
		if err := c.persistLocked(); err != nil {
			c.rollbackLoad()
			return nil, false, err
		}
		out := *s.Assets[id]
		return &out, false, nil
	}

	if ids, dup := c.snap.Aliases[in.Key]; dup && len(ids) > 0 {
		return nil, false, &CatalogError{
			Kind:    ErrAmbiguousKey,
			Reason:  "业务键 " + in.Key + " 是历史别名，已指向资产 " + joinStrings(ids, ","),
			Details: ids,
		}
	}

	s := cloneSnapshot(c.snap)
	s.NextAssetSeq++
	id = fmt.Sprintf("asset-%06d", s.NextAssetSeq)
	ts := c.now()
	a := &Asset{ID: id, Key: in.Key, Name: in.Name, Kind: in.Kind, CreateTime: ts, UpdateTime: ts}
	s.Assets[id] = a
	s.ByKey[in.Key] = id
	c.snap = s
	if err := c.persistLocked(); err != nil {
		c.rollbackLoad()
		return nil, false, err
	}
	out := *a
	return &out, true, nil
}

// PutVersion 登记某资产的下一版 schema。
//
// 幂等规则：
//   - 与最新版本列集合完全一致时直接返回已有版本，不新增；
//   - 相对最新版本存在破坏性变化时返回 incompatible_schema，逐项说明；
//   - 兼容演进（新增列、宽化类型、放宽为可空）正常生成新版本；
//   - 历史版本一旦生成永不修改；任何覆盖企图都被拒绝。
func (c *Catalog) PutVersion(assetKey string, cols []Column) (*SchemaVersion, bool, error) {
	if assetKey == "" {
		return nil, false, &ValidationError{Reason: "asset_key 不能为空"}
	}
	if err := ValidateColumns(cols); err != nil {
		return nil, false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	id, err := c.resolveLocked(assetKey)
	if err != nil {
		return nil, false, err
	}
	if c.snap.Assets[id].Deleted {
		return nil, false, &CatalogError{Kind: ErrConflict, Reason: "资产 " + assetKey + " 已删除，不能登记新版本"}
	}
	versions := c.snap.Versions[id]
	if len(versions) > 0 {
		latest := versions[len(versions)-1]
		if equalColumns(latest.Columns, cols) {
			return latest, false, nil // 内容完全相同：幂等
		}
		if inc := checkCompatibility(latest.Columns, cols); len(inc) > 0 {
			return nil, false, &CatalogError{
				Kind:    ErrIncompatibleSchema,
				Reason:  fmt.Sprintf("资产 %s 的新版本相对 v%d 存在 %d 处破坏性变化", assetKey, latest.Version, len(inc)),
				Changes: inc,
			}
		}
	}

	next := len(versions) + 1
	ver := &SchemaVersion{
		AssetID:     id,
		Version:     next,
		Columns:     append([]Column(nil), cols...),
		ContentHash: contentHash(id, next, cols),
		CreateTime:  c.now(),
	}
	s := cloneSnapshot(c.snap)
	s.Versions[id] = append(append([]*SchemaVersion(nil), versions...), ver)
	c.snap = s
	if err := c.persistLocked(); err != nil {
		c.rollbackLoad()
		return nil, false, err
	}
	return ver, true, nil
}

// GetAsset 按业务键查资产（含已删除资产，历史可追溯）。
func (c *Catalog) GetAsset(key string) (*Asset, error) {
	s := c.current()
	id, err := c.resolveWith(s, key)
	if err != nil {
		return nil, err
	}
	a := *s.Assets[id]
	return &a, nil
}

// ListAssets 列出资产；includeDeleted 控制是否包含已删除资产。
// 顺序按业务键排序，结果与登记顺序无关。
func (c *Catalog) ListAssets(includeDeleted bool) []*Asset {
	s := c.current()
	out := make([]*Asset, 0, len(s.Assets))
	for _, a := range s.Assets {
		if a.Deleted && !includeDeleted {
			continue
		}
		aa := *a
		out = append(out, &aa)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// Versions 返回某资产全部历史版本（版本号升序）。
func (c *Catalog) Versions(assetKey string) ([]*SchemaVersion, error) {
	s := c.current()
	id, err := c.resolveWith(s, assetKey)
	if err != nil {
		return nil, err
	}
	out := make([]*SchemaVersion, len(s.Versions[id]))
	for i, v := range s.Versions[id] {
		vv := *v
		vv.Columns = append([]Column(nil), v.Columns...)
		out[i] = &vv
	}
	return out, nil
}

// GetVersion 返回指定版本号的 schema。
func (c *Catalog) GetVersion(assetKey string, version int) (*SchemaVersion, error) {
	s := c.current()
	id, err := c.resolveWith(s, assetKey)
	if err != nil {
		return nil, err
	}
	for _, v := range s.Versions[id] {
		if v.Version == version {
			vv := *v
			vv.Columns = append([]Column(nil), v.Columns...)
			return &vv, nil
		}
	}
	return nil, &CatalogError{Kind: ErrNotFound, Reason: fmt.Sprintf("资产 %s 不存在版本 %d", assetKey, version)}
}

// DiffVersions 给出同一资产任意两个版本间 加/删/改 的列。
func (c *Catalog) DiffVersions(assetKey string, from, to int) (*VersionDiff, error) {
	s := c.current()
	id, err := c.resolveWith(s, assetKey)
	if err != nil {
		return nil, err
	}
	var of, ot *SchemaVersion
	for _, v := range s.Versions[id] {
		switch v.Version {
		case from:
			of = v
		case to:
			ot = v
		}
	}
	if of == nil || ot == nil {
		missing := from
		if ot == nil {
			missing = to
		}
		return nil, &CatalogError{Kind: ErrNotFound, Reason: fmt.Sprintf("资产 %s 不存在版本 %d", assetKey, missing)}
	}
	added, removed, changed, _ := diffColumns(of.Columns, ot.Columns)
	return &VersionDiff{FromVersion: from, ToVersion: to, Added: added, Removed: removed, Changed: changed}, nil
}

// DeleteAsset 软删除资产：删除后不能登记新版本或连边，
// 但历史版本与历史血缘全部保留、仍可查询。删除操作幂等。
func (c *Catalog) DeleteAsset(key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, err := c.resolveLocked(key)
	if err != nil {
		return err
	}
	if c.snap.Assets[id].Deleted {
		return nil
	}
	s := cloneSnapshot(c.snap)
	aa := *s.Assets[id]
	aa.Deleted = true
	aa.DeleteTime = c.now()
	s.Assets[id] = &aa
	c.snap = s
	return c.persistLockedOrRollback()
}

// resolveLocked 在写锁内把业务键解析为内部资产 ID。
func (c *Catalog) resolveLocked(key string) (string, error) {
	return c.resolveWith(c.snap, key)
}

// resolveWith 精确解析业务键：
//   - 当前键唯一命中 -> 返回 ID；
//   - 完全不存在（含已删除资产的当前键）-> not_found；
//   - 别名指向多个资产 -> ambiguous_key，绝不任意挑一个。
func (c *Catalog) resolveWith(s *snapshot, key string) (string, error) {
	if id, ok := s.ByKey[key]; ok {
		return id, nil
	}
	ids := s.Aliases[key]
	switch len(ids) {
	case 0:
		return "", &CatalogError{Kind: ErrNotFound, Reason: "找不到业务键为 " + key + " 的资产"}
	case 1:
		return ids[0], nil
	default:
		return "", &CatalogError{
			Kind:    ErrAmbiguousKey,
			Reason:  "业务键 " + key + " 匹配到多个资产: " + joinStrings(ids, ",") + "，请改用当前业务键",
			Details: append([]string(nil), ids...),
		}
	}
}

func equalColumns(a, b []Column) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
