package catalog

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	walFileName      = "wal.log"
	snapshotFileName = "snapshot.json"
	tmpSuffix        = ".tmp"
)

// FileStore 把状态落到本地目录:
//
//   - snapshot.json: 压缩时写出的完整状态快照(临时文件 fsync + 原子 rename)。
//   - wal.log: 只追加提交记录,每行一个 JSON Event,每条追加后 fsync。
//
// 进程在写一半时被杀:未以换行结尾的最后一条记录视为"半写",
// 重启时识别并截断(该记录从未完成提交);快照/日志中间任何无法解析的
// 记录一律返回 KindCorrupt,绝不当作正常数据装入内存。
type FileStore struct {
	dir string
	f   *os.File
}

// OpenFileStore 打开(必要时创建)数据目录,返回可追加的 Store。
func OpenFileStore(dir string) (*FileStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	path := filepath.Join(dir, walFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开 WAL 失败: %w", err)
	}
	if err := syncDir(dir); err != nil {
		f.Close()
		return nil, err
	}
	return &FileStore{dir: dir, f: f}, nil
}

// Close 关闭底层文件。
func (s *FileStore) Close() error { return s.f.Close() }

// Dir 返回数据目录。
func (s *FileStore) Dir() string { return s.dir }

// Append 将一条事件作为完整行落盘并 fsync;fsync 成功才视为提交。
func (s *FileStore) Append(ev Event) error {
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if _, err := s.f.Write(line); err != nil {
		return fmt.Errorf("写 WAL 失败: %w", err)
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("fsync WAL 失败: %w", err)
	}
	return nil
}

// Load 先读快照,再按提交顺序重放 WAL。
func (s *FileStore) Load(apply func(Event) error) error {
	snapPath := filepath.Join(s.dir, snapshotFileName)
	snapData, err := os.ReadFile(snapPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读快照失败: %w", err)
	}
	if len(bytes.TrimSpace(snapData)) > 0 {
		var snap Snapshot
		if jerr := json.Unmarshal(snapData, &snap); jerr != nil {
			return errf(KindCorrupt, "快照文件 %s 无法解析,拒绝以损坏数据启动: %v", snapPath, jerr)
		}
		if aerr := applySnapshot(&snap, apply); aerr != nil {
			return aerr
		}
	}

	walPath := filepath.Join(s.dir, walFileName)
	data, err := os.ReadFile(walPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读 WAL 失败: %w", err)
	}
	return replayWAL(walPath, data, apply)
}

// applySnapshot 把快照展开为重放事件(先注册资产各版本,再加边),
// 复用 apply 保证内存构建路径唯一。
func applySnapshot(snap *Snapshot, apply func(Event) error) error {
	keys := make([]string, 0, len(snap.Assets))
	for k := range snap.Assets {
		keys = append(keys, k)
	}
	// 快照内资产顺序无关,排序保证重放确定。
	sortStrings(keys)
	for _, k := range keys {
		a := snap.Assets[k]
		versions := make([]string, 0, len(a.Versions))
		for v := range a.Versions {
			versions = append(versions, v)
		}
		// 快照丢失提交顺序时,无法可靠知道版本先后;
		// 以版本创建时间排序(CreatedAt 来自登记时刻)。
		sortByCreated(versions, a.Versions)
		for _, v := range versions {
			ver := a.Versions[v]
			ev := Event{
				Type:      EvAssetRegistered,
				Namespace: a.Namespace,
				Asset:     a.Name,
				Version:   ver.Version,
				Columns:   ver.Columns,
				CreatedAt: ver.CreatedAt,
			}
			if err := apply(ev); err != nil {
				return err
			}
		}
		if a.Deleted {
			if err := apply(Event{Type: EvAssetDeleted, Namespace: a.Namespace, Asset: a.Name, DeletedAt: a.DeletedAt}); err != nil {
				return err
			}
		}
	}
	for _, e := range snap.Edges {
		src, dst := e.Source, e.Target
		if err := apply(Event{Type: EvEdgeAdded, Source: &src, Target: &dst, CreatedAt: e.CreatedAt}); err != nil {
			return err
		}
	}
	return nil
}

// replayWAL 逐行重放;尾部缺换行或解析失败的半截记录按半写截断,
// 中间任何坏行按损坏报错。
func replayWAL(path string, data []byte, apply func(Event) error) error {
	complete := data
	var torn []byte
	if len(data) > 0 {
		if nl := bytes.LastIndexByte(data, '\n'); nl >= 0 {
			complete = data[:nl+1]
			torn = data[nl+1:]
		} else {
			complete = nil
			torn = data
		}
	}

	scanner := bufio.NewScanner(bytes.NewReader(complete))
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			return errf(KindCorrupt, "WAL 文件 %s 第 %d 行无法解析,数据已损坏: %v", path, lineNo, err)
		}
		if err := validateEvent(ev); err != nil {
			return errf(KindCorrupt, "WAL 文件 %s 第 %d 行内容不合法: %v", path, lineNo, err)
		}
		if err := apply(ev); err != nil {
			return errf(KindCorrupt, "WAL 文件 %s 第 %d 行无法应用: %v", path, lineNo, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return errf(KindCorrupt, "读取 WAL 文件 %s 失败: %v", path, err)
	}

	// 尾部半写记录:从未完成提交,安全截断,保证重启后不留半写状态。
	if len(bytes.TrimSpace(torn)) > 0 {
		if err := os.WriteFile(path, complete, 0o644); err != nil {
			return errf(KindCorrupt, "检测到 %s 尾部半写记录但截断失败: %v", path, err)
		}
	}
	return nil
}

func validateEvent(ev Event) error {
	switch ev.Type {
	case EvAssetRegistered:
		if ev.Namespace == "" || ev.Asset == "" || ev.Version == "" {
			return fmt.Errorf("资产登记事件缺少键字段")
		}
		if len(ev.Columns) == 0 {
			return fmt.Errorf("资产登记事件缺少列")
		}
	case EvAssetDeleted:
		if ev.Namespace == "" || ev.Asset == "" {
			return fmt.Errorf("资产删除事件缺少键字段")
		}
	case EvEdgeAdded, EvEdgeRemoved:
		if ev.Source == nil || ev.Target == nil {
			return fmt.Errorf("血缘事件缺少端点")
		}
	default:
		return fmt.Errorf("未知事件类型 %q", ev.Type)
	}
	return nil
}

// Compact 原子地写入快照并清空 WAL。
func (s *FileStore) Compact(snap Snapshot) error {
	s.f.Close()

	snapPath := filepath.Join(s.dir, snapshotFileName)
	tmp := snapPath + tmpSuffix
	buf := &bytes.Buffer{}
	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(snap); err != nil {
		return err
	}
	if err := writeFileAtomic(snapPath, tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}

	// 快照就位后,用空 WAL 原子替换旧 WAL。
	walPath := filepath.Join(s.dir, walFileName)
	if err := writeFileAtomic(walPath, walPath+tmpSuffix, []byte{}, 0o644); err != nil {
		return err
	}

	f, err := os.OpenFile(walPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("压缩后重新打开 WAL 失败: %w", err)
	}
	s.f = f
	return nil
}

// writeFileAtomic 写临时文件、fsync、rename,再 fsync 目录保证元数据落盘。
func writeFileAtomic(target, tmp string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("创建临时文件 %s 失败: %w", tmp, err)
	}
	if _, err := io.Copy(f, bytes.NewReader(data)); err != nil {
		f.Close()
		return fmt.Errorf("写临时文件 %s 失败: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync 临时文件 %s 失败: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return fmt.Errorf("原子替换 %s 失败: %w", target, err)
	}
	return syncDir(filepath.Dir(target))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("打开目录 %s 失败: %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		// 部分平台/文件系统不支持目录 fsync,此时退化为忽略(本地常见 ext4 支持)。
		if os.IsPermission(err) {
			return nil
		}
		return fmt.Errorf("fsync 目录 %s 失败: %w", dir, err)
	}
	return nil
}
