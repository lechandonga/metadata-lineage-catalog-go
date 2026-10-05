package catalog

import (
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"
)

func setupLineageAssets(t *testing.T, c *Catalog, keys ...string) {
	t.Helper()
	for _, k := range keys {
		mustAsset(t, c, k)
		_, _, err := c.PutVersion(k, []Column{
			{Name: "id", Type: "int64", Nullable: false},
			{Name: "name", Type: "string", Nullable: true},
			{Name: "val", Type: "int64", Nullable: true},
		})
		if err != nil {
			t.Fatalf("PutVersion(%s): %v", k, err)
		}
	}
}

// TestLineageReferences 覆盖业务：边端点解析——精确命中、悬空字段、
// 业务键歧义，三类失败必须可区分；重复边幂等；自环拒绝。
func TestLineageReferences(t *testing.T) {
	c := newTestCatalog(t)
	setupLineageAssets(t, c, "src.tbl", "dst.tbl")

	t.Run("正常登记与重复幂等", func(t *testing.T) {
		in := EdgeInput{"src.tbl", "id", "dst.tbl", "id"}
		e, created, err := c.PutEdge(in)
		if err != nil || !created {
			t.Fatalf("首次登记应 created, got %+v err=%v", e, err)
		}
		e2, created, err := c.PutEdge(in)
		if err != nil || created {
			t.Fatalf("重复登记应幂等 created=false, got %+v err=%v", e2, err)
		}
		if len(c.ListEdges()) != 1 {
			t.Fatalf("重复边不得写入两份, got %d", len(c.ListEdges()))
		}
	})

	t.Run("资产不存在_与字段悬空_错误可区分", func(t *testing.T) {
		_, _, err := c.PutEdge(EdgeInput{"ghost.tbl", "id", "dst.tbl", "id"})
		if kindOf(err) != ErrNotFound {
			t.Fatalf("不存在资产应 not_found, got %v", err)
		}
		_, _, err = c.PutEdge(EdgeInput{"src.tbl", "missing_col", "dst.tbl", "id"})
		if kindOf(err) != ErrDanglingReference {
			t.Fatalf("不存在字段应 dangling_reference, got %v", err)
		}
	})

	t.Run("业务键歧义必须拒绝而非任选", func(t *testing.T) {
		s := c.testSnapshot()
		var ids []string
		for _, k := range []string{"src.tbl", "dst.tbl"} {
			ids = append(ids, s.ByKey[k])
		}
		c.testInjectAmbiguousAlias("legacy", ids)
		_, _, err := c.PutEdge(EdgeInput{"legacy", "id", "dst.tbl", "id"})
		if kindOf(err) != ErrAmbiguousKey {
			t.Fatalf("歧义键应 ambiguous_key, got %v", err)
		}
		_, err = c.GetAsset("legacy")
		if kindOf(err) != ErrAmbiguousKey {
			t.Fatalf("歧义键查询也应 ambiguous_key, got %v", err)
		}
	})

	t.Run("自环拒绝", func(t *testing.T) {
		_, _, err := c.PutEdge(EdgeInput{"src.tbl", "id", "src.tbl", "id"})
		if kindOf(err) != ErrValidation {
			t.Fatalf("自环应 validation_error, got %v", err)
		}
	})

	t.Run("撤销幂等_撤不存在的边结果确定", func(t *testing.T) {
		in := EdgeInput{"src.tbl", "id", "dst.tbl", "id"}
		deleted, err := c.DeleteEdge(in)
		if err != nil || !deleted {
			t.Fatalf("首次撤销应 deleted=true, got %v err=%v", deleted, err)
		}
		deleted, err = c.DeleteEdge(in)
		if err != nil || deleted {
			t.Fatalf("撤销不存在边应 deleted=false 且无错, got %v err=%v", deleted, err)
		}
	})
}

// TestLineageCyclesAndImpact 覆盖业务：环不挂死、重复边不重复计数、
// 影响面结果与登记顺序无关且按字典序稳定，并给出具体传播路径。
func TestLineageCyclesAndImpact(t *testing.T) {
	c := newTestCatalog(t)
	setupLineageAssets(t, c, "a.tbl", "b.tbl", "c.tbl", "d.tbl")

	// 环 a.id -> b.id -> c.id -> a.id，外加 a.id -> d.id 支链。
	edges := []EdgeInput{
		{"a.tbl", "id", "b.tbl", "id"},
		{"b.tbl", "id", "c.tbl", "id"},
		{"c.tbl", "id", "a.tbl", "id"},
		{"a.tbl", "id", "d.tbl", "id"},
	}
	shuffled := []EdgeInput{edges[3], edges[1], edges[0], edges[2]}
	for _, in := range shuffled {
		if _, _, err := c.PutEdge(in); err != nil {
			t.Fatalf("PutEdge %+v: %v", in, err)
		}
	}

	res, err := c.Impact("a.tbl", "id")
	if err != nil {
		t.Fatalf("含环影响面查询失败: %v", err)
	}
	got := make([]string, 0, len(res.Impact))
	for _, e := range res.Impact {
		got = append(got, e.Node.AssetKey+"."+e.Node.Column)
	}
	want := []string{"b.tbl.id", "c.tbl.id", "d.tbl.id"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("影响面应去重且按字典序稳定, got %v want %v", got, want)
	}

	var cEntry *ImpactEntry
	for i := range res.Impact {
		if res.Impact[i].Node.AssetKey == "c.tbl" {
			cEntry = &res.Impact[i]
		}
	}
	if cEntry == nil {
		t.Fatal("缺少 c.tbl 的影响条目")
	}
	pathKeys := make([]string, len(cEntry.Path))
	for i, n := range cEntry.Path {
		pathKeys[i] = n.AssetKey
	}
	wantPath := []string{"a.tbl", "b.tbl", "c.tbl"}
	if !reflect.DeepEqual(pathKeys, wantPath) {
		t.Fatalf("传播路径不符, got %v want %v", pathKeys, wantPath)
	}

	// 新登记的边立刻反映到查询。
	if _, _, err := c.PutEdge(EdgeInput{"c.tbl", "name", "d.tbl", "name"}); err != nil {
		t.Fatal(err)
	}
	r2, err := c.Impact("c.tbl", "name")
	if err != nil || len(r2.Impact) != 1 || r2.Impact[0].Node.AssetKey != "d.tbl" {
		t.Fatalf("新登记边应立刻反映到查询, got %+v err=%v", r2, err)
	}
}

// TestImpactOrderIndependent 用不同登记顺序构造同一张图，
// 影响面结果必须完全一致。
func TestImpactOrderIndependent(t *testing.T) {
	build := func(order []int) []string {
		c := newTestCatalog(t)
		setupLineageAssets(t, c, "a.tbl", "b.tbl", "c.tbl", "d.tbl", "e.tbl")
		es := []EdgeInput{
			{"a.tbl", "val", "b.tbl", "val"},
			{"a.tbl", "val", "c.tbl", "val"},
			{"b.tbl", "val", "d.tbl", "val"},
			{"c.tbl", "val", "d.tbl", "val"},
			{"d.tbl", "val", "e.tbl", "val"},
		}
		for _, i := range order {
			if _, _, err := c.PutEdge(es[i]); err != nil {
				t.Fatal(err)
			}
		}
		res, err := c.Impact("a.tbl", "val")
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(res.Impact))
		for _, e := range res.Impact {
			out = append(out, e.Node.AssetKey)
		}
		sort.Strings(out)
		return out
	}
	o1 := build([]int{0, 1, 2, 3, 4})
	o2 := build([]int{4, 3, 2, 1, 0})
	if !reflect.DeepEqual(o1, o2) {
		t.Fatalf("影响面与登记顺序有关: %v vs %v", o1, o2)
	}
	want := []string{"b.tbl", "c.tbl", "d.tbl", "e.tbl"}
	if !reflect.DeepEqual(o1, want) {
		t.Fatalf("影响面集合错误: %v", o1)
	}
}

// TestImpactPerformance 资产数千、边上万时，单次影响面秒级返回。
func TestImpactPerformance(t *testing.T) {
	if testing.Short() {
		t.Skip("short 模式跳过规模用例")
	}
	c := newTestCatalog(t)
	const nAssets = 2000
	keys := make([]string, nAssets)
	for i := 0; i < nAssets; i++ {
		k := fmt.Sprintf("p%04d.tbl", i)
		keys[i] = k
		mustAsset(t, c, k)
		_, _, err := c.PutVersion(k, []Column{{Name: "id", Type: "int64", Nullable: false}})
		if err != nil {
			t.Fatal(err)
		}
	}
	// 每个资产向后连 8 条边（约 1.6 万条，含回环边）。
	for i := 0; i < nAssets; i++ {
		for j := 1; j <= 8; j++ {
			target := (i + j) % nAssets
			if target == i {
				continue
			}
			if _, _, err := c.PutEdge(EdgeInput{keys[i], "id", keys[target], "id"}); err != nil {
				t.Fatal(err)
			}
		}
	}
	start := time.Now()
	r, err := c.Impact(keys[0], "id")
	elapsed := time.Since(start)
	if err != nil || len(r.Impact) == 0 {
		t.Fatalf("Impact 异常: %d err=%v", len(r.Impact), err)
	}
	t.Logf("规模=%d 资产/%d 边, 影响面=%d 字段, 耗时=%s", nAssets, len(c.ListEdges()), len(r.Impact), elapsed)
	if elapsed > time.Second {
		t.Fatalf("单次影响面过慢: %s，要求秒级以内", elapsed)
	}
}
