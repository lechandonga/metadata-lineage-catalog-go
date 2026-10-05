package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFileStoreRecovery 正常重启后状态完整可读(资产/版本/墓碑/边)。
func TestFileStoreRecovery(t *testing.T) {
	dir := t.TempDir()

	func() {
		store := openStore(t, dir)
		defer store.Close()
		c, err := New(store)
		if err != nil {
			t.Fatalf("首次打开失败: %v", err)
		}
		mustRegister(t, c, "dw", "users", "v1", colsV1)
		mustRegister(t, c, "dw", "users", "v2", append(cloneCols(colsV1), Column{Name: "age", Type: "int32"}))
		mustRegister(t, c, "dw", "orders", "v1", []Column{{Name: "id", Type: "int64"}, {Name: "uid", Type: "int64"}})
		addEdgeOK(t, c,
			FieldRef{Namespace: "dw", Asset: "users", Column: "id"},
			FieldRef{Namespace: "dw", Asset: "orders", Column: "uid"},
		)
		if err := c.DeleteAsset("dw", "orders"); err != nil {
			t.Fatalf("删除失败: %v", err)
		}
	}()

	// 重新打开,模拟进程重启。
	store := openStore(t, dir)
	defer store.Close()
	c, err := New(store)
	if err != nil {
		t.Fatalf("重启恢复失败: %v", err)
	}
	a, err := c.GetAsset(AssetRef{Namespace: "dw", Asset: "users"})
	if err != nil {
		t.Fatalf("恢复后读资产失败: %v", err)
	}
	if len(a.Versions) != 2 || a.Latest != "v2" {
		t.Fatalf("恢复后版本不完整: %+v", a.Versions)
	}
	if v := a.Versions["v1"]; len(v.Columns) != 2 {
		t.Fatal("历史版本内容恢复错误")
	}
	// 墓碑与边也恢复。
	if len(c.ListAssets(false)) != 1 {
		t.Fatal("墓碑状态未恢复")
	}
	if len(c.ListEdges()) != 1 {
		t.Fatal("历史血缘边未恢复")
	}
	if _, err := c.GetVersion(AssetRef{Namespace: "dw", Asset: "orders"}, "v1"); err != nil {
		t.Fatalf("已删除资产历史版本应仍可读: %v", err)
	}
}

func openStore(t *testing.T, dir string) *FileStore {
	t.Helper()
	store, err := OpenFileStore(dir)
	if err != nil {
		t.Fatalf("打开 FileStore 失败: %v", err)
	}
	return store
}

// TestTornWriteRecovery WAL 最后一条记录写了一半(进程被杀),
// 重启应识别并截断半写记录,此前已提交数据完好。
func TestTornWriteRecovery(t *testing.T) {
	dir := t.TempDir()

	func() {
		store := openStore(t, dir)
		defer store.Close()
		c, _ := New(store)
		mustRegister(t, c, "dw", "users", "v1", colsV1)
	}()

	walPath := filepath.Join(dir, walFileName)
	committed, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatal(err)
	}

	// 追加一条只写了一半、没有换行的 JSON(模拟 fsync 前/写入中被杀)。
	half := []byte(`{"type":"asset_registered","namespace":"dw","asset":"hal`)
	if err := os.WriteFile(walPath, append(committed, half...), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStore(t, dir)
	defer store.Close()
	c, err := New(store)
	if err != nil {
		t.Fatalf("半写 WAL 应可恢复,实际错误: %v", err)
	}
	if _, err := c.GetAsset(AssetRef{Namespace: "dw", Asset: "users"}); err != nil {
		t.Fatalf("已提交资产应完好: %v", err)
	}
	if _, err := c.GetAsset(AssetRef{Namespace: "dw", Asset: "hal"}); err == nil {
		t.Fatal("半写记录对应的资产不得被装入内存")
	}
	// 半写尾部已被截断。
	data, _ := os.ReadFile(walPath)
	if strings.Contains(string(data), "hal") {
		t.Fatal("重启后 WAL 中仍残留半写记录")
	}
}

// TestCorruptWALRejected WAL 中间出现无法解析的记录属于损坏,
// 必须按 KindCorrupt 报错,而不是静默跳过或返回脏数据。
func TestCorruptWALRejected(t *testing.T) {
	dir := t.TempDir()

	func() {
		store := openStore(t, dir)
		defer store.Close()
		c, _ := New(store)
		mustRegister(t, c, "dw", "users", "v1", colsV1)
	}()

	walPath := filepath.Join(dir, walFileName)
	good, _ := os.ReadFile(walPath)
	// 在中间插入一条完整但内容损坏的行(有换行 -> 不是半写,而是确定的损坏)。
	corruptLine := []byte("{this-is-not-json\n")
	if err := os.WriteFile(walPath, append(append(good, corruptLine...), good...), 0o644); err != nil {
		t.Fatal(err)
	}

	store := openStore(t, dir)
	defer store.Close()
	_, err := New(store)
	ce, ok := AsError(err)
	if !ok || ce.Kind != KindCorrupt {
		t.Fatalf("WAL 中间损坏必须返回 corrupt_data,实际 %v", err)
	}
	t.Logf("[判定依据] 损坏 WAL 被拒绝: %s", ce.Message)
}

// TestCorruptSnapshotRejected 快照损坏时不得用坏快照启动。
func TestCorruptSnapshotRejected(t *testing.T) {
	dir := t.TempDir()
	store := openStore(t, dir)
	c, _ := New(store)
	mustRegister(t, c, "dw", "users", "v1", colsV1)
	if err := c.Compact(); err != nil {
		t.Fatalf("压缩失败: %v", err)
	}
	store.Close()

	snapPath := filepath.Join(dir, snapshotFileName)
	if err := os.WriteFile(snapPath, []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	store2 := openStore(t, dir)
	defer store2.Close()
	_, err := New(store2)
	if ce, ok := AsError(err); !ok || ce.Kind != KindCorrupt {
		t.Fatalf("损坏快照必须返回 corrupt_data,实际 %v", err)
	}
}

// TestCompactThenRecover 压缩(快照+清空WAL)后状态完整,且可继续写入。
func TestCompactThenRecover(t *testing.T) {
	dir := t.TempDir()

	func() {
		store := openStore(t, dir)
		defer store.Close()
		c, _ := New(store)
		mustRegister(t, c, "dw", "users", "v1", colsV1)
		mustRegister(t, c, "dw", "users", "v2", append(cloneCols(colsV1), Column{Name: "age", Type: "int32"}))
		if err := c.Compact(); err != nil {
			t.Fatalf("压缩失败: %v", err)
		}
		info, _ := os.Stat(filepath.Join(dir, walFileName))
		if info.Size() != 0 {
			t.Fatalf("压缩后 WAL 应为空,实际 %d 字节", info.Size())
		}
		mustRegister(t, c, "dw", "users", "v3", append(cloneCols(colsV1), Column{Name: "age", Type: "int32"}, Column{Name: "x", Type: "int64"}))
	}()

	store := openStore(t, dir)
	defer store.Close()
	c, err := New(store)
	if err != nil {
		t.Fatalf("压缩后重启失败: %v", err)
	}
	a, _ := c.GetAsset(AssetRef{Namespace: "dw", Asset: "users"})
	if len(a.Versions) != 3 || a.Latest != "v3" {
		t.Fatalf("压缩+追加后恢复版本错误: latest=%s versions=%d", a.Latest, len(a.Versions))
	}
}

// BenchmarkImpactPerformance 资产数千、边上万时单次影响面查询应秒级以内。
func BenchmarkImpactPerformance(b *testing.B) {
	store := NewMemStore()
	c, err := New(store)
	if err != nil {
		b.Fatal(err)
	}
	const assetCount = 3000
	const edgeCount = 15000
	// 每个资产一个字段 f。
	for i := 0; i < assetCount; i++ {
		mustRegisterB(b, c, "dw", assetName(i), []Column{{Name: "f", Type: "int64"}})
	}
	// 生成一个有根、无环的大图:边 i -> j 仅当 j>i,保证不会成环。
	seed := int64(1)
	nextRand := func(n int) int {
		seed = (seed*1103515245 + 12345) & 0x7fffffff
		return int(seed) % n
	}
	added := 0
	for added < edgeCount {
		i := nextRand(assetCount - 1)
		j := i + 1 + nextRand(assetCount-i-1)
		err := c.AddEdge(
			FieldRef{Namespace: "dw", Asset: assetName(i), Column: "f"},
			FieldRef{Namespace: "dw", Asset: assetName(j), Column: "f"},
		)
		if err == nil {
			added++
		}
	}
	root := FieldRef{Namespace: "dw", Asset: assetName(0), Column: "f"}
	b.ResetTimer()
	b.ReportAllocs()
	for n := 0; n < b.N; n++ {
		res, err := c.Impact(root)
		if err != nil {
			b.Fatal(err)
		}
		if len(res.Downstreams) == 0 {
			b.Fatal("根节点应有下游")
		}
	}
}

func assetName(i int) string { return "a_" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func mustRegisterB(b *testing.B, c *Catalog, ns, name string, cols []Column) {
	b.Helper()
	if _, err := c.RegisterAsset(ns, name, "v1", cols); err != nil {
		b.Fatal(err)
	}
}
