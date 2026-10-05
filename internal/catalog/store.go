package catalog

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Store 是 catalog 状态的持久化抽象。Save 必须具备原子性：
// 要么完整写入新状态，要么旧状态保持可读，不允许出现半写文件。
type Store interface {
	Load() (*snapshot, error)
	Save(*snapshot) error
}

// FileStore 把状态落到单个本地 JSON 文件：
//
//	{ "schema_version": 1, "sha256": "...", "data": {...} }
//
// 写入流程：写同目录临时文件 -> fsync -> 原子 rename 替换目标文件
// -> fsync 目录。进程在任何时刻被杀，重启后看到的要么是上一份
// 完整文件，要么是新的完整文件，不会读到半成品。
//
// 读取时按类型识别损坏：JSON 语法错误、校验和不匹配、信封版本
// 不支持、结构语义不合法分别报 corrupt_store / unsupported_store_version，
// 绝不会把坏数据当正常数据返回。
type FileStore struct {
	path string
	dir  string
}

func NewFileStore(path string) *FileStore {
	return &FileStore{path: path, dir: filepath.Dir(path)}
}

type envelope struct {
	SchemaVersion int             `json:"schema_version"`
	SHA256        string          `json:"sha256"`
	Data          json.RawMessage `json:"data"`
}

const currentStoreVersion = 1

func (f *FileStore) Load() (*snapshot, error) {
	raw, err := os.ReadFile(f.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &CatalogError{Kind: ErrCorruptStore, Reason: "读取状态文件失败: " + err.Error()}
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &CatalogError{Kind: ErrCorruptStore, Reason: "状态文件为空，疑似写入中断"}
	}
	var env envelope
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&env); err != nil {
		return nil, &CatalogError{Kind: ErrCorruptStore, Reason: "状态文件不是合法 JSON，疑似半写或被截断: " + err.Error()}
	}
	if env.SchemaVersion != currentStoreVersion {
		return nil, &CatalogError{
			Kind:   ErrUnsupportedVersion,
			Reason: fmt.Sprintf("状态文件版本 %d 不受本版本支持（支持版本 %d）", env.SchemaVersion, currentStoreVersion),
		}
	}
	sum := sha256.Sum256(env.Data)
	got := hex.EncodeToString(sum[:])
	if env.SHA256 == "" || got != env.SHA256 {
		return nil, &CatalogError{Kind: ErrCorruptStore, Reason: "状态文件校验和不匹配，数据可能被部分覆盖或损坏"}
	}
	var s snapshot
	if err := json.Unmarshal(env.Data, &s); err != nil {
		return nil, &CatalogError{Kind: ErrCorruptStore, Reason: "状态文件结构无法解析: " + err.Error()}
	}
	if err := validateSnapshot(&s); err != nil {
		return nil, err
	}
	return &s, nil
}

func (f *FileStore) Save(s *snapshot) error {
	if err := validateSnapshot(s); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return &CatalogError{Kind: ErrCorruptStore, Reason: "序列化状态失败: " + err.Error()}
	}
	sum := sha256.Sum256(data)
	env, err := json.Marshal(envelope{SchemaVersion: currentStoreVersion, SHA256: hex.EncodeToString(sum[:]), Data: data})
	if err != nil {
		return &CatalogError{Kind: ErrCorruptStore, Reason: "序列化信封失败: " + err.Error()}
	}
	if err := os.MkdirAll(f.dir, 0o755); err != nil {
		return &CatalogError{Kind: ErrCorruptStore, Reason: "创建数据目录失败: " + err.Error()}
	}

	tmp, err := os.CreateTemp(f.dir, ".catalog-state-*")
	if err != nil {
		return &CatalogError{Kind: ErrCorruptStore, Reason: "创建临时文件失败: " + err.Error()}
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := io.Copy(tmp, bytes.NewReader(env)); err != nil {
		_ = tmp.Close()
		cleanup()
		return &CatalogError{Kind: ErrCorruptStore, Reason: "写临时文件失败: " + err.Error()}
	}
	if err := tmp.Sync(); err != nil { // 数据落盘
		_ = tmp.Close()
		cleanup()
		return &CatalogError{Kind: ErrCorruptStore, Reason: "fsync 数据失败: " + err.Error()}
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return &CatalogError{Kind: ErrCorruptStore, Reason: "关闭临时文件失败: " + err.Error()}
	}
	if err := os.Rename(tmpName, f.path); err != nil { // 同目录原子替换
		cleanup()
		return &CatalogError{Kind: ErrCorruptStore, Reason: "原子替换状态文件失败: " + err.Error()}
	}
	syncDir(f.dir) // 尽力而为：让 rename 落盘
	return nil
}

func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
}

// cloneSnapshot 以 copy-on-write 方式拷贝快照：map 浅拷贝，
// Edges 共享只读切片。所有变更点都以“新指针/新切片整体替换条目”
// 的方式写入，绝不原地修改旧条目，因此读方持有的旧快照始终不可变，
// 克隆成本仅为 O(资产数) 的 map 浅拷，万级边写入不会逐边复制。
func cloneSnapshot(s *snapshot) *snapshot {
	ns := &snapshot{
		SchemaVersion: s.SchemaVersion,
		NextAssetSeq:  s.NextAssetSeq,
		Assets:        make(map[string]*Asset, len(s.Assets)),
		ByKey:         make(map[string]string, len(s.ByKey)),
		Aliases:       make(map[string][]string, len(s.Aliases)),
		Versions:      make(map[string][]*SchemaVersion, len(s.Versions)),
		Edges:         s.Edges, // 不可变有序切片，直接共享
		edgeStore:     s.edgeStore,
	}
	for id, a := range s.Assets {
		ns.Assets[id] = a
	}
	for k, v := range s.ByKey {
		ns.ByKey[k] = v
	}
	for k, ids := range s.Aliases {
		ns.Aliases[k] = ids
	}
	for id, vs := range s.Versions {
		ns.Versions[id] = vs
	}
	return ns
}

func (c *Catalog) persistLocked() error {
	if c.store == nil {
		return nil
	}
	return c.store.Save(c.snap)
}

// persistLockedOrRollback 持久化；失败时把内存回滚到磁盘上的旧状态，
// 避免“内存已改、磁盘仍旧”造成后续重试产生重复版本/重复边。
func (c *Catalog) persistLockedOrRollback() error {
	err := c.persistLocked()
	if err != nil {
		c.rollbackLoad()
	}
	return err
}

func (c *Catalog) rollbackLoad() {
	if c.store == nil {
		return
	}
	s, err := c.store.Load()
	if err != nil || s == nil {
		c.snap = emptySnapshot()
		return
	}
	normalize(s)
	c.snap = s
}

// validateSnapshot 校验落盘数据的语义完整性，发现问题按
// corrupt_store 报错并逐项列出，而不是返回坏数据。
func validateSnapshot(s *snapshot) error {
	var details []string
	add := func(format string, args ...any) {
		details = append(details, fmt.Sprintf(format, args...))
	}

	if s.SchemaVersion != currentStoreVersion {
		return &CatalogError{
			Kind:   ErrUnsupportedVersion,
			Reason: fmt.Sprintf("状态文件版本 %d 不受支持（支持 %d）", s.SchemaVersion, currentStoreVersion),
		}
	}
	for id, a := range s.Assets {
		if a == nil {
			add("资产 %s 的记录为空", id)
			continue
		}
		if a.ID != id {
			add("资产索引 id=%s 与记录 id=%s 不一致", id, a.ID)
		}
		if a.Key == "" {
			add("资产 %s 缺少业务键", id)
		}
	}
	for key, id := range s.ByKey {
		a, ok := s.Assets[id]
		if !ok {
			add("业务键 %s 指向不存在的资产 id=%s", key, id)
		} else if a.Key != key {
			add("业务键索引 %s 与资产实际键 %s 不一致", key, a.Key)
		}
	}
	for alias, ids := range s.Aliases {
		for _, id := range ids {
			if _, ok := s.Assets[id]; !ok {
				add("别名 %s 指向不存在的资产 id=%s", alias, id)
			}
		}
	}
	seenVersions := map[string]map[int]bool{}
	for id, vs := range s.Versions {
		if _, ok := s.Assets[id]; !ok {
			add("版本历史属于不存在的资产 id=%s", id)
		}
		seen := map[int]bool{}
		for i, v := range vs {
			if v == nil {
				add("资产 %s 的第 %d 个版本为空", id, i)
				continue
			}
			if v.AssetID != id {
				add("版本记录 asset_id=%s 与其所属资产 %s 不一致", v.AssetID, id)
			}
			if seen[v.Version] {
				add("资产 %s 存在重复版本号 %d", id, v.Version)
			}
			seen[v.Version] = true
			if v.Version != i+1 {
				add("资产 %s 版本号不连续/乱序：位置 %d 处是 v%d", id, i+1, v.Version)
			}
			if v.ContentHash != contentHash(id, v.Version, v.Columns) {
				add("资产 %s 的 v%d 内容校验和不匹配", id, v.Version)
			}
			if err := ValidateColumns(v.Columns); err != nil {
				add("资产 %s 的 v%d 列定义不合法: %s", id, v.Version, err.Error())
			}
		}
		seenVersions[id] = seen
	}
	edgeSeen := map[string]bool{}
	prevFP := ""
	for i, e := range s.Edges {
		fp := edgeFingerprint(e)
		srcID, srcOK := s.ByKey[e.SrcAssetKey]
		dstID, dstOK := s.ByKey[e.DstAssetKey]
		if !srcOK || !dstOK {
			add("第 %d 条边引用了不存在的资产: %s -> %s", i+1, e.SrcAssetKey, e.DstAssetKey)
			continue
		}
		if fp <= prevFP {
			add("第 %d 条边重复或未排序: %s", i+1, fp)
		}
		prevFP = fp
		edgeSeen[fp] = true
		if len(s.Versions[srcID]) > 0 {
			latest := s.Versions[srcID][len(s.Versions[srcID])-1]
			if !containsColumn(latest.Columns, e.SrcColumn) {
				add("边源字段 %s.%s 不在最新 schema 中", e.SrcAssetKey, e.SrcColumn)
			}
		}
		if len(s.Versions[dstID]) > 0 {
			latest := s.Versions[dstID][len(s.Versions[dstID])-1]
			if !containsColumn(latest.Columns, e.DstColumn) {
				add("边目的字段 %s.%s 不在最新 schema 中", e.DstAssetKey, e.DstColumn)
			}
		}
	}
	if len(details) > 0 {
		return &CatalogError{Kind: ErrCorruptStore, Reason: "状态文件语义校验失败", Details: details}
	}
	return nil
}

func containsColumn(cols []Column, name string) bool {
	for _, c := range cols {
		if c.Name == name {
			return true
		}
	}
	return false
}

func edgeFingerprint(e Edge) string {
	return e.SrcAssetKey + "\x00" + e.SrcColumn + "\x00" + e.DstAssetKey + "\x00" + e.DstColumn
}
