package catalog

import (
	"fmt"
	"sync"
	"testing"
)

// TestConcurrentRegisterIdempotent 多个 goroutine 并发上报同一资产同一版本,
// 最终只能有一个版本、一条登记事实;竞态检测下应稳定通过。
func TestConcurrentRegisterIdempotent(t *testing.T) {
	c, store := newTestCatalog(t)
	const writers = 16
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func() {
			defer wg.Done()
			_, err := c.RegisterAsset("dw", "users", "v1", colsV1)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("并发同内容上报应全部成功(幂等),出现错误: %v", err)
		}
	}

	a, err := c.GetAsset(AssetRef{Namespace: "dw", Asset: "users"})
	if err != nil {
		t.Fatalf("读取资产失败: %v", err)
	}
	if len(a.Versions) != 1 {
		t.Fatalf("并发重复上报后应只有 1 个版本,实际 %d", len(a.Versions))
	}
	if store.EventCount() != 1 {
		t.Fatalf("WAL 应只有 1 条登记事件,实际 %d", store.EventCount())
	}
}

// TestConcurrentSnapshotConsistency 读写并发下,查询读到的资产版本图必须始终完整:
// 每一轮登记的都是"在前一版基础上加一列"的兼容版本,任何快照里最新版本
// 的列数必须等于其版本序号(看不到写一半的状态)。
func TestConcurrentSnapshotConsistency(t *testing.T) {
	c, _ := newTestCatalog(t)

	// 初始 v1: 一列。
	colsFor := func(n int) []Column {
		cols := make([]Column, n)
		for i := 0; i < n; i++ {
			cols[i] = Column{Name: fmt.Sprintf("c%d", i), Type: "int64"}
		}
		return cols
	}
	mustRegister(t, c, "dw", "snap", "v1", colsFor(1))

	const rounds = 40
	var wg sync.WaitGroup

	// 单写者串行登记 v2..vN(加列)。
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 2; i <= rounds+1; i++ {
			if _, err := c.RegisterAsset("dw", "snap", fmt.Sprintf("v%d", i), colsFor(i)); err != nil {
				t.Errorf("登记 v%d 失败: %v", i, err)
				return
			}
		}
	}()

	// 多读者:任意时刻读到的快照必须自洽。
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				a, err := c.GetAsset(AssetRef{Namespace: "dw", Asset: "snap"})
				if err != nil {
					t.Errorf("并发读取失败: %v", err)
					return
				}
				latest := a.Versions[a.Latest]
				if latest == nil {
					t.Errorf("快照缺少 latest=%s 指向的版本", a.Latest)
					return
				}
				// vK 应有 K 列;版本序号与列数不符意味着读到撕裂状态。
				var k int
				if _, err := fmt.Sscanf(a.Latest, "v%d", &k); err != nil {
					t.Errorf("版本号解析失败 %q: %v", a.Latest, err)
					return
				}
				if len(latest.Columns) != k {
					t.Errorf("快照不一致: latest=%s 但列数=%d(期望 %d)", a.Latest, len(latest.Columns), k)
					return
				}
				// 每个历史版本也必须完整:列数 == 版本序号。
				for verName, v := range a.Versions {
					var vn int
					if _, err := fmt.Sscanf(verName, "v%d", &vn); err != nil {
						t.Errorf("版本号异常 %q", verName)
						return
					}
					if len(v.Columns) != vn {
						t.Errorf("历史版本 %s 列数=%d,期望 %d,读到不完整快照", verName, len(v.Columns), vn)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentEdgesAndImpact 并发登记互不冲突的边并同时查询影响面,
// 不允许重复边、不允许查询挂死、最终边数确定。
func TestConcurrentEdgesAndImpact(t *testing.T) {
	c, _ := newTestCatalog(t)
	root := buildChainAssets(t, c, "root", 1)[0]
	leaves := buildChainAssets(t, c, "leaf", 20)

	var writersWg sync.WaitGroup
	for _, leaf := range leaves {
		writersWg.Add(1)
		go func(dst FieldRef) {
			defer writersWg.Done()
			if err := c.AddEdge(root, dst); err != nil {
				t.Errorf("并发登记边失败: %v", err)
			}
		}(leaf)
	}
	// 与写入并发的影响面查询:过程中结果数单调不减即可,重点是不挂死、不重复。
	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		prev := -1
		for {
			select {
			case <-stop:
				return
			default:
			}
			res, err := c.Impact(root)
			if err != nil {
				t.Errorf("并发影响面查询失败: %v", err)
				return
			}
			seen := map[string]bool{}
			for _, d := range res.Downstreams {
				if seen[d.String()] {
					t.Errorf("影响面出现重复字段 %s", d)
					return
				}
				seen[d.String()] = true
			}
			if len(res.Downstreams) < prev {
				t.Errorf("无撤销操作时影响面不应收缩: %d -> %d", prev, len(res.Downstreams))
				return
			}
			prev = len(res.Downstreams)
		}
	}()
	writersWg.Wait() // 等所有写者完成后,再通知 reader 退出
	close(stop)
	readerWg.Wait()

	if len(c.ListEdges()) != len(leaves) {
		t.Fatalf("并发登记后应有 %d 条边,实际 %d", len(leaves), len(c.ListEdges()))
	}
	res, _ := c.Impact(root)
	if len(res.Downstreams) != len(leaves) {
		t.Fatalf("影响面应为 %d,实际 %d", len(leaves), len(res.Downstreams))
	}
}

// TestWriteFailureNoPartialState 持久化提交失败时内存状态不得改变。
func TestWriteFailureNoPartialState(t *testing.T) {
	c, store := newTestCatalog(t)
	mustRegister(t, c, "dw", "users", "v1", colsV1)
	store.FailNextAppend()
	_, err := c.RegisterAsset("dw", "users", "v2", append(cloneCols(colsV1), Column{Name: "x", Type: "int64"}))
	if err == nil {
		t.Fatal("模拟提交失败时登记应返回错误")
	}
	a, _ := c.GetAsset(AssetRef{Namespace: "dw", Asset: "users"})
	if _, ok := a.Versions["v2"]; ok {
		t.Fatal("提交失败后不得在内存留下 v2(不能写一半)")
	}
	if a.Latest != "v1" {
		t.Fatalf("提交失败后 latest 应保持 v1,实际 %s", a.Latest)
	}
	// 重试应成功且不产生重复。
	mustRegister(t, c, "dw", "users", "v2", append(cloneCols(colsV1), Column{Name: "x", Type: "int64"}))
	if len(c.ListAssets(false)) != 1 {
		t.Fatal("重试后仍应只有一个资产")
	}
}
