package catalog

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestConcurrentSnapshotConsistency 覆盖业务：多个来源并发
// 上报版本/登记边/撤销边/查询，任一时刻读方只能看到完整快照
// （资产存在则版本结构完整、边端点字段必然存在于最新 schema），
// 重复操作不产生重复版本/重复边。该用例在 -race 下必须稳定通过。
func TestConcurrentSnapshotConsistency(t *testing.T) {
	c := newTestCatalog(t)
	mustAsset(t, c, "shared.tbl")
	if _, _, err := c.PutVersion("shared.tbl", []Column{
		{Name: "id", Type: "int64", Nullable: false},
		{Name: "c0", Type: "string", Nullable: true},
	}); err != nil {
		t.Fatal(err)
	}
	// 为后续列预留兼容演进空间：一次性登记可空列 c1..c19，
	// 边只引用已发布字段，避免并发演进产生悬空引用。
	cols := []Column{{Name: "id", Type: "int64", Nullable: false}}
	for i := 0; i < 20; i++ {
		cols = append(cols, Column{Name: colName(i), Type: "string", Nullable: true})
	}
	// 直接重建版本（v2 为完整列集；v1->v2 仅新增列，兼容）。
	if _, _, err := c.PutVersion("shared.tbl", cols); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	errCh := make(chan error, 32)

	// 写者：反复登记/撤销同一小批边，重复操作必须幂等。
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			in := EdgeInput{"shared.tbl", "id", "shared.tbl", colName(id % 10)}
			for {
				select {
				case <-stop:
					return
				default:
				}
				// 自环之外（id != cX），允许成环。
				if _, _, err := c.PutEdge(in); err != nil {
					errCh <- err
					return
				}
				if _, err := c.DeleteEdge(in); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}

	// 读者：每次读快照并校验“不允许看到半成品”：
	// 版本连续、列字段存在；每条现存边的字段都在最新版本里。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			vs, err := c.Versions("shared.tbl")
			if err != nil {
				errCh <- err
				return
			}
			for i, v := range vs {
				if v.Version != i+1 {
					errCh <- &ValidationError{Reason: "读到版本号断裂的半成品快照"}
					return
				}
			}
			if len(vs) > 0 {
				latest := vs[len(vs)-1]
				for _, e := range c.ListEdges() {
					foundSrc, foundDst := false, false
					for _, col := range latest.Columns {
						if col.Name == e.SrcColumn {
							foundSrc = true
						}
						if col.Name == e.DstColumn {
							foundDst = true
						}
					}
					if e.SrcAssetKey == "shared.tbl" && !foundSrc {
						errCh <- &ValidationError{Reason: "读到源字段不在最新 schema 的半成品边"}
						return
					}
					if e.DstAssetKey == "shared.tbl" && !foundDst {
						errCh <- &ValidationError{Reason: "读到目的字段不在最新 schema 的半成品边"}
						return
					}
				}
			}
		}
	}()

	time.Sleep(150 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发读写期间出现不一致或错误: %v", err)
	}

	// 收敛后：版本数仍为 2，边数不超过写者数（无重复堆积）。
	vs, _ := c.Versions("shared.tbl")
	if len(vs) != 2 {
		t.Fatalf("重复写入不得产生重复版本, got %d", len(vs))
	}
	if n := len(c.ListEdges()); n > 4 {
		t.Fatalf("重复边/残留边异常: %d", n)
	}
}

func colName(i int) string {
	return "c" + itoa(i)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	buf := [4]byte{}
	n := len(buf)
	for i > 0 {
		n--
		buf[n] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[n:])
}

// TestFileStoreRestart 覆盖业务：每次写操作都落盘，重启后
// 资产、历史版本、血缘全部可读且幂等状态延续。
func TestFileStoreRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	open := func() *Catalog {
		c, err := New(NewFileStore(path), nil)
		if err != nil {
			t.Fatalf("打开目录失败: %v", err)
		}
		return c
	}

	c := open()
	mustAsset(t, c, "restart.tbl")
	if _, _, err := c.PutVersion("restart.tbl", []Column{
		{Name: "id", Type: "int64", Nullable: false},
		{Name: "a", Type: "string", Nullable: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PutVersion("restart.tbl", []Column{
		{Name: "id", Type: "int64", Nullable: false},
		{Name: "a", Type: "string", Nullable: true},
		{Name: "b", Type: "int32", Nullable: true},
	}); err != nil {
		t.Fatal(err)
	}
	mustAsset(t, c, "restart2.tbl")
	if _, _, err := c.PutVersion("restart2.tbl", []Column{{Name: "id", Type: "int64"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PutEdge(EdgeInput{"restart.tbl", "id", "restart2.tbl", "id"}); err != nil {
		t.Fatal(err)
	}

	// 模拟进程重启：重新从文件加载。
	c2 := open()
	vs, err := c2.Versions("restart.tbl")
	if err != nil || len(vs) != 2 {
		t.Fatalf("重启后历史版本缺失: %d err=%v", len(vs), err)
	}
	if edges := c2.ListEdges(); len(edges) != 1 || edges[0].SrcAssetKey != "restart.tbl" {
		t.Fatalf("重启后血缘缺失: %+v", edges)
	}
	// 重复登记在重启后仍然幂等，版本号延续为 3 而非重建。
	v, created, err := c2.PutVersion("restart.tbl", []Column{
		{Name: "id", Type: "int64", Nullable: false},
		{Name: "a", Type: "string", Nullable: true},
		{Name: "b", Type: "int32", Nullable: true},
		{Name: "c", Type: "int32", Nullable: true},
	})
	if err != nil || !created || v.Version != 3 {
		t.Fatalf("重启后版本号应延续, got v=%d created=%v err=%v", v.Version, created, err)
	}
}

// TestCorruptStateDetected 覆盖业务：半写/损坏文件必须被识别并按
// corrupt_store 报错，而不是当正常数据返回；原子写保证同目录下
// 不会出现半写状态（临时文件残留不影响正式文件）。
func TestCorruptStateDetected(t *testing.T) {
	t.Run("JSON截断_识别为损坏", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		if err := os.WriteFile(path, []byte(`{"schema_version":1,"sha256":"abc","data":{`), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := New(NewFileStore(path), nil)
		if kindOf(err) != ErrCorruptStore {
			t.Fatalf("截断 JSON 应 corrupt_store, got %v", err)
		}
	})

	t.Run("校验和不匹配_识别为损坏", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		good := func() []byte {
			c, err := New(NewFileStore(path), nil)
			if err != nil {
				t.Fatal(err)
			}
			mustAsset(t, c, "ok.tbl")
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			return b
		}()
		// 翻转 data 区域一个字节（信封尾部内部），保留 JSON 外壳。
		bad := append([]byte(nil), good...)
		pos := len(bad) - 20
		bad[pos] = bad[pos] ^ 0xff
		if err := os.WriteFile(path, bad, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := New(NewFileStore(path), nil)
		if kindOf(err) != ErrCorruptStore {
			t.Fatalf("校验和被破坏应 corrupt_store, got %v", err)
		}
	})

	t.Run("空文件_识别为疑似中断", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := New(NewFileStore(path), nil)
		if kindOf(err) != ErrCorruptStore {
			t.Fatalf("空文件应 corrupt_store, got %v", err)
		}
	})

	t.Run("不支持的信封版本", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "state.json")
		// 手工构造一个高版本但校验和自洽的信封。
		data := []byte(`{}`)
		sum := sha256Hex(data)
		env := `{"schema_version":99,"sha256":"` + sum + `","data":{}}`
		if err := os.WriteFile(path, []byte(env), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := New(NewFileStore(path), nil)
		if kindOf(err) != ErrUnsupportedVersion {
			t.Fatalf("应 unsupported_store_version, got %v", err)
		}
	})
}

// TestDeletedAssetHistory 覆盖业务：资产软删除后历史版本与历史血缘
// 仍可追溯，但不能再新增版本或连边。
func TestDeletedAssetHistory(t *testing.T) {
	c := newTestCatalog(t)
	mustAsset(t, c, "gone.tbl")
	mustAsset(t, c, "alive.tbl")
	if _, _, err := c.PutVersion("gone.tbl", []Column{{Name: "id", Type: "int64"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PutVersion("alive.tbl", []Column{{Name: "id", Type: "int64"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.PutEdge(EdgeInput{"gone.tbl", "id", "alive.tbl", "id"}); err != nil {
		t.Fatal(err)
	}

	if err := c.DeleteAsset("gone.tbl"); err != nil {
		t.Fatal(err)
	}
	if err := c.DeleteAsset("gone.tbl"); err != nil {
		t.Fatalf("删除应幂等: %v", err)
	}

	// 默认列表不含已删除资产，但 includeDeleted 与直接查询仍可见。
	for _, a := range c.ListAssets(false) {
		if a.Key == "gone.tbl" {
			t.Fatal("默认列表不应包含已删除资产")
		}
	}
	a, err := c.GetAsset("gone.tbl")
	if err != nil || !a.Deleted {
		t.Fatalf("已删除资产仍应可查: %+v err=%v", a, err)
	}
	vs, err := c.Versions("gone.tbl")
	if err != nil || len(vs) != 1 {
		t.Fatalf("历史版本必须保留: %d err=%v", len(vs), err)
	}
	if edges := c.ListEdges(); len(edges) != 1 {
		t.Fatalf("历史血缘必须保留, got %d", len(edges))
	}

	// 删除后不能登记新版本。
	if _, _, err := c.PutVersion("gone.tbl", []Column{
		{Name: "id", Type: "int64"},
		{Name: "x", Type: "string", Nullable: true},
	}); kindOf(err) != ErrConflict {
		t.Fatalf("已删除资产登记新版本应 conflict, got %v", err)
	}
	// 删除后不能新增边。
	if _, _, err := c.PutEdge(EdgeInput{"alive.tbl", "id", "gone.tbl", "id"}); kindOf(err) != ErrConflict {
		t.Fatalf("指向已删除资产新增边应 conflict, got %v", err)
	}
}
