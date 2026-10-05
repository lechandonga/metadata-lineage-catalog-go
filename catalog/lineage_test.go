package catalog

import (
	"fmt"
	"testing"
	"time"
)

// buildChainAssets 注册 N 个各含 col 字段的资产,返回字段引用 a0.col ... aN-1.col。
func buildChainAssets(t *testing.T, c *Catalog, prefix string, n int) []FieldRef {
	t.Helper()
	refs := make([]FieldRef, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s_%d", prefix, i)
		mustRegister(t, c, "dw", name, "v1", []Column{{Name: "col", Type: "int64"}})
		refs[i] = FieldRef{Namespace: "dw", Asset: name, Column: "col"}
	}
	return refs
}

func addEdgeOK(t *testing.T, c *Catalog, src, dst FieldRef) {
	t.Helper()
	if err := c.AddEdge(src, dst); err != nil {
		t.Fatalf("登记边 %s -> %s 失败: %v", src, dst, err)
	}
}

// TestEdgeSelfLoopDuplicateCycle 自环、重复边幂等、环拒绝、撤销幂等。
func TestEdgeSelfLoopDuplicateCycle(t *testing.T) {
	c, _ := newTestCatalog(t)
	nodes := buildChainAssets(t, c, "t", 2)
	a, b := nodes[0], nodes[1]

	// 自环被拒绝(KindConflict)。
	err := c.AddEdge(a, a)
	ce, ok := AsError(err)
	if !ok || ce.Kind != KindConflict {
		t.Fatalf("自环应返回 conflict,实际 %v", err)
	}
	t.Logf("[判定依据] 自环拒绝: %s", ce.Message)

	// 正常边 + 重复登记幂等。
	addEdgeOK(t, c, a, b)
	addEdgeOK(t, c, a, b)
	if len(c.ListEdges()) != 1 {
		t.Fatalf("重复登记同一条边不得产生两条边,实际 %d", len(c.ListEdges()))
	}

	// 反向边会成环:a->b 已存在,再 b->a 构成环,必须拒绝。
	err = c.AddEdge(b, a)
	ce, ok = AsError(err)
	if !ok || ce.Kind != KindCycle {
		t.Fatalf("成环应返回 cycle_detected,实际 %v", err)
	}
	t.Logf("[判定依据] 环拒绝: %s", ce.Message)
	if len(c.ListEdges()) != 1 {
		t.Fatal("环拒绝不得写入边")
	}

	// 撤销幂等:第一次 removed,第二次 idempotent_not_found。
	r1, err := c.RemoveEdge(a, b)
	if err != nil || r1.Outcome != "removed" {
		t.Fatalf("首次撤销应为 removed,实际 %+v err=%v", r1, err)
	}
	r2, err := c.RemoveEdge(a, b)
	if err != nil || r2.Outcome != "idempotent_not_found" || r2.Existed {
		t.Fatalf("撤销不存在的边应确定返回 idempotent_not_found,实际 %+v err=%v", r2, err)
	}
	t.Logf("[判定依据] 撤销不存在的边: outcome=%s", r2.Outcome)
}

// TestImpactAndPaths 影响面包含全部可达下游且给出具体路径。
func TestImpactAndPaths(t *testing.T) {
	c, _ := newTestCatalog(t)
	nodes := buildChainAssets(t, c, "n", 5)
	// n0 -> n1 -> n3
	// n0 -> n2 -> n3 -> n4
	addEdgeOK(t, c, nodes[0], nodes[1])
	addEdgeOK(t, c, nodes[0], nodes[2])
	addEdgeOK(t, c, nodes[1], nodes[3])
	addEdgeOK(t, c, nodes[2], nodes[3])
	addEdgeOK(t, c, nodes[3], nodes[4])

	res, err := c.Impact(nodes[0])
	if err != nil {
		t.Fatalf("影响面查询失败: %v", err)
	}
	if len(res.Downstreams) != 4 {
		t.Fatalf("应影响 n1..n4 共 4 个字段,实际 %d: %v", len(res.Downstreams), res.Downstreams)
	}

	pathByDst := map[string][]FieldRef{}
	for _, p := range res.Paths {
		pathByDst[p.Destination.String()] = p.Path
		t.Logf("[影响路径] %s", formatPath(p.Path))
	}
	p4 := pathByDst[nodes[4].String()]
	if len(p4) != 4 || p4[0].String() != nodes[0].String() || p4[3].String() != nodes[4].String() {
		t.Fatalf("到 n4 的路径应为 n0->...->n4 长度4,实际 %v", p4)
	}
}

func formatPath(p []FieldRef) string {
	out := ""
	for i, f := range p {
		if i > 0 {
			out += " -> "
		}
		out += f.String()
	}
	return out
}

// TestImpactOrderIndependent 结果与边的登记顺序无关。
func TestImpactOrderIndependent(t *testing.T) {
	build := func(order [][2]int) []FieldRef {
		c, _ := newTestCatalog(t)
		nodes := buildChainAssets(t, c, "o", 4)
		// 图:0->1, 0->2, 1->3, 2->3(菱形)
		for _, e := range order {
			src, dst := nodes[e[0]], nodes[e[1]]
			addEdgeOK(t, c, src, dst)
		}
		res, err := c.Impact(nodes[0])
		if err != nil {
			t.Fatalf("影响面查询失败: %v", err)
		}
		return res.Downstreams
	}

	orderA := [][2]int{{0, 1}, {0, 2}, {1, 3}, {2, 3}}
	orderB := [][2]int{{2, 3}, {1, 3}, {0, 2}, {0, 1}}
	rA := build(orderA)
	rB := build(orderB)
	if len(rA) != len(rB) {
		t.Fatalf("两种登记顺序结果数量不同: %d vs %d", len(rA), len(rB))
	}
	for i := range rA {
		if rA[i].String() != rB[i].String() {
			t.Fatalf("影响面结果依赖登记顺序:\nA=%v\nB=%v", rA, rB)
		}
	}
	t.Logf("[判定依据] 两种登记顺序下结果一致且有序: %v", rA)
}

// TestImpactReflectsNewEdges 新登记/撤销的边立刻反映到查询。
func TestImpactReflectsNewEdges(t *testing.T) {
	c, _ := newTestCatalog(t)
	nodes := buildChainAssets(t, c, "live", 3)
	res, _ := c.Impact(nodes[0])
	if len(res.Downstreams) != 0 {
		t.Fatal("初始应无下游")
	}
	addEdgeOK(t, c, nodes[0], nodes[1])
	res, _ = c.Impact(nodes[0])
	if len(res.Downstreams) != 1 {
		t.Fatal("新登记的边应立刻出现在影响面")
	}
	addEdgeOK(t, c, nodes[1], nodes[2])
	res, _ = c.Impact(nodes[0])
	if len(res.Downstreams) != 2 {
		t.Fatal("传递影响应立刻可见")
	}
	if _, err := c.RemoveEdge(nodes[1], nodes[2]); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	res, _ = c.Impact(nodes[0])
	if len(res.Downstreams) != 1 {
		t.Fatalf("撤销边后影响面应收缩为 1,实际 %d", len(res.Downstreams))
	}
}

// TestImpactWithCycleSafe 已存在的环(理论上被写入拦截)不会使遍历挂死:
// 构造重复边与多边汇聚场景验证 BFS 去重。
func TestImpactWithCycleSafe(t *testing.T) {
	c, _ := newTestCatalog(t)
	nodes := buildChainAssets(t, c, "cyc", 3)
	addEdgeOK(t, c, nodes[0], nodes[1])
	addEdgeOK(t, c, nodes[0], nodes[2])
	addEdgeOK(t, c, nodes[0], nodes[1]) // 重复边
	// 0 -> {1,2},1、2 之间尝试互连成环应被拒绝:
	addEdgeOK(t, c, nodes[1], nodes[2])
	err := c.AddEdge(nodes[2], nodes[1])
	if ce, ok := AsError(err); !ok || ce.Kind != KindCycle {
		t.Fatalf("2->1 经 0 可达 1,应判环,实际 %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		res, ierr := c.Impact(nodes[0])
		if ierr != nil {
			t.Errorf("影响面查询失败: %v", ierr)
			return
		}
		if len(res.Downstreams) != 2 {
			t.Errorf("环/重复边场景下游应为 2,实际 %d", len(res.Downstreams))
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("影响面遍历疑似挂死")
	}
}
