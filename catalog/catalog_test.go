package catalog

import (
	"testing"
	"time"
)

var baseTime = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

func newTestCatalog(t *testing.T) (*Catalog, *MemStore) {
	t.Helper()
	now := baseTime
	store := NewMemStore()
	c, err := NewForTest(store, func() time.Time { return now })
	if err != nil {
		t.Fatalf("创建目录失败: %v", err)
	}
	return c, store
}

func mustRegister(t *testing.T, c *Catalog, ns, name, v string, cols []Column) *RegisterResult {
	t.Helper()
	res, err := c.RegisterAsset(ns, name, v, cols)
	if err != nil {
		t.Fatalf("登记资产 %s/%s@%s 失败: %v", ns, name, v, err)
	}
	return res
}

var colsV1 = []Column{{Name: "id", Type: "int64"}, {Name: "name", Type: "string"}}

// TestRegisterIdempotencyAndConflict 同一业务键的版本幂等与冲突拒绝。
func TestRegisterIdempotencyAndConflict(t *testing.T) {
	c, store := newTestCatalog(t)

	r1 := mustRegister(t, c, "dw", "users", "v1", colsV1)
	if r1.Outcome != "created" {
		t.Fatalf("首次登记 outcome=%s,预期 created", r1.Outcome)
	}

	// 相同内容重复上报必须幂等,且不产生新事件。
	eventsBefore := store.EventCount()
	r2 := mustRegister(t, c, "dw", "users", "v1", colsV1)
	if r2.Outcome != "idempotent" {
		t.Fatalf("重复上报 outcome=%s,预期 idempotent", r2.Outcome)
	}
	if store.EventCount() != eventsBefore {
		t.Fatal("幂等上报不应追加 WAL 事件")
	}

	// 同版本不同内容必须明确拒绝(KindConflict),历史不可变。
	conflict := []Column{{Name: "id", Type: "string"}, {Name: "name", Type: "string"}}
	_, err := c.RegisterAsset("dw", "users", "v1", conflict)
	ce, ok := AsError(err)
	if !ok || ce.Kind != KindConflict {
		t.Fatalf("同版本不同内容应返回 conflict,实际 %v", err)
	}
	t.Logf("[判定依据] 拒绝冲突版本: %s", ce.Message)

	got := mustGetVersion(t, c, AssetRef{Namespace: "dw", Asset: "users"}, "v1")
	if got.Columns[0].Type != "int64" {
		t.Fatal("冲突上报不得覆盖历史版本")
	}
}

// TestRegisterRejectsBreakingChange 破坏性演进必须被拒绝并标注原因。
func TestRegisterRejectsBreakingChange(t *testing.T) {
	c, _ := newTestCatalog(t)
	mustRegister(t, c, "dw", "users", "v1", colsV1)

	// 兼容:加列。
	added := append(cloneCols(colsV1), Column{Name: "age", Type: "int32"})
	r := mustRegister(t, c, "dw", "users", "v2", added)
	if r.Outcome != "created" {
		t.Fatalf("加列应放行,outcome=%s", r.Outcome)
	}

	// 破坏:删列。
	_, err := c.RegisterAsset("dw", "users", "v3", colsV1)
	ce, ok := AsError(err)
	if !ok || ce.Kind != KindIncompatibleSchema {
		t.Fatalf("删列应返回 incompatible_schema,实际 %v", err)
	}
	incs, _ := ce.Details.([]Incompatibility)
	t.Logf("[判定依据] 拒绝 v3,不兼容项:")
	for _, inc := range incs {
		t.Logf("  - %s %s: %s", inc.Kind, inc.Column, inc.Reason)
	}
	if len(incs) != 1 || incs[0].Kind != "removed_column" || incs[0].Column != "age" {
		t.Fatalf("应标出 age 列被删,实际 %+v", incs)
	}

	// 被拒绝的版本不得落库、不可查询。
	if _, gerr := c.GetVersion(AssetRef{Namespace: "dw", Asset: "users"}, "v3"); gerr == nil {
		t.Fatal("被拒绝的 v3 不应存在")
	}
}

func mustGetVersion(t *testing.T, c *Catalog, ref AssetRef, v string) *Version {
	t.Helper()
	got, err := c.GetVersion(ref, v)
	if err != nil {
		t.Fatalf("查询版本 %s 失败: %v", v, err)
	}
	return got
}

// TestVersionDiff 任意两个版本之间的增删改差异。
func TestVersionDiff(t *testing.T) {
	c, _ := newTestCatalog(t)
	mustRegister(t, c, "dw", "users", "v1", []Column{
		{Name: "id", Type: "int32"},
		{Name: "name", Type: "string"},
	})
	mustRegister(t, c, "dw", "users", "v2", []Column{
		{Name: "id", Type: "int64"}, // 拓宽
		{Name: "name", Type: "string"},
		{Name: "email", Type: "string"},
	})

	d, err := c.DiffVersions(AssetRef{Namespace: "dw", Asset: "users"}, "v1", "v2")
	if err != nil {
		t.Fatalf("版本差异查询失败: %v", err)
	}
	if len(d.Added) != 1 || d.Added[0].Name != "email" {
		t.Fatalf("新增列应为 email,实际 %+v", d.Added)
	}
	if len(d.Changed) != 1 || d.Changed[0].Column != "id" || d.Changed[0].From != "int32" || d.Changed[0].To != "int64" {
		t.Fatalf("变化列应为 id int32->int64,实际 %+v", d.Changed)
	}
	if len(d.Removed) != 0 {
		t.Fatalf("不应有删除列,实际 %+v", d.Removed)
	}
	if !d.Compatible {
		t.Fatal("加列+拓宽应为兼容差异")
	}
	t.Logf("[判定依据] v1->v2: added=%v changed=%v removed=%v compatible=%v",
		d.Added, d.Changed, d.Removed, d.Compatible)

	// 反向 diff:删列 + 收窄,应为破坏性。
	rev, err := c.DiffVersions(AssetRef{Namespace: "dw", Asset: "users"}, "v2", "v1")
	if err != nil {
		t.Fatalf("反向差异查询失败: %v", err)
	}
	if rev.Compatible {
		t.Fatal("反向(删列+收窄)必须判为不兼容")
	}
}

// TestAmbiguousKeyAndDanglingReference 短名歧义和悬空引用要可区分。
func TestAmbiguousKeyAndDanglingReference(t *testing.T) {
	c, _ := newTestCatalog(t)
	mustRegister(t, c, "dw", "users", "v1", colsV1)
	mustRegister(t, c, "ods", "users", "v1", colsV1)
	mustRegister(t, c, "dw", "orders", "v1", []Column{{Name: "id", Type: "int64"}, {Name: "uid", Type: "int64"}})

	// 短名 users 命中两个 namespace:歧义。
	_, err := c.GetAsset(AssetRef{Namespace: "", Asset: "users"})
	ce, ok := AsError(err)
	if !ok || ce.Kind != KindAmbiguous {
		t.Fatalf("短名重名应返回 ambiguous_key,实际 %v", err)
	}
	t.Logf("[判定依据] 歧义错误: %s", ce.Message)

	// 加上 namespace 后精确定位正常。
	if _, err := c.GetAsset(AssetRef{Namespace: "dw", Asset: "users"}); err != nil {
		t.Fatalf("精确定位不应失败: %v", err)
	}

	// 血缘指向不存在的资产:dangling。
	err = c.AddEdge(
		FieldRef{Namespace: "dw", Asset: "orders", Column: "uid"},
		FieldRef{Namespace: "dw", Asset: "ghost", Column: "id"},
	)
	ce, ok = AsError(err)
	if !ok || ce.Kind != KindDangling {
		t.Fatalf("不存在资产应返回 dangling_reference,实际 %v", err)
	}

	// 资产存在但字段不存在:同样是 dangling,但消息指明字段。
	err = c.AddEdge(
		FieldRef{Namespace: "dw", Asset: "orders", Column: "uid"},
		FieldRef{Namespace: "dw", Asset: "users", Column: "nope"},
	)
	ce, ok = AsError(err)
	if !ok || ce.Kind != KindDangling {
		t.Fatalf("不存在字段应返回 dangling_reference,实际 %v", err)
	}
	t.Logf("[判定依据] 悬空字段错误: %s", ce.Message)
}

// TestDeleteKeepsHistory 删除资产后历史版本与历史血缘仍可追溯。
func TestDeleteKeepsHistory(t *testing.T) {
	c, _ := newTestCatalog(t)
	mustRegister(t, c, "dw", "users", "v1", colsV1)
	mustRegister(t, c, "dw", "orders", "v1", []Column{{Name: "id", Type: "int64"}, {Name: "uid", Type: "int64"}})
	if err := c.AddEdge(
		FieldRef{Namespace: "dw", Asset: "users", Column: "id"},
		FieldRef{Namespace: "dw", Asset: "orders", Column: "uid"},
	); err != nil {
		t.Fatalf("登记边失败: %v", err)
	}

	if err := c.DeleteAsset("dw", "users"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	// 删除幂等。
	if err := c.DeleteAsset("dw", "users"); err != nil {
		t.Fatalf("重复删除应幂等,实际 %v", err)
	}

	// 墓碑资产默认列表不可见,显式 include_deleted 可见。
	for _, a := range c.ListAssets(false) {
		if a.Key() == "dw/users" {
			t.Fatal("已删除资产不应出现在默认列表")
		}
	}
	var found bool
	for _, a := range c.ListAssets(true) {
		if a.Key() == "dw/users" {
			found = true
		}
	}
	if !found {
		t.Fatal("include_deleted 时应能看到墓碑资产")
	}

	// 历史版本仍可读,且带墓碑标记。
	v := mustGetVersion(t, c, AssetRef{Namespace: "dw", Asset: "users"}, "v1")
	if !v.Deleted {
		t.Fatal("历史版本应标记 deleted")
	}
	// 历史血缘仍保留。
	if len(c.ListEdges()) != 1 {
		t.Fatal("删除资产不应清理历史血缘边")
	}
	// 不允许向已删除资产登记新版本。
	_, err := c.RegisterAsset("dw", "users", "v2", colsV1)
	if ce, ok := AsError(err); !ok || ce.Kind != KindDeleted {
		t.Fatalf("向已删除资产登记版本应返回 asset_deleted,实际 %v", err)
	}
	// 不允许登记指向已删除资产的新边。
	err = c.AddEdge(
		FieldRef{Namespace: "dw", Asset: "orders", Column: "id"},
		FieldRef{Namespace: "dw", Asset: "users", Column: "id"},
	)
	if ce, ok := AsError(err); !ok || ce.Kind != KindDeleted {
		t.Fatalf("指向已删除资产的新边应返回 asset_deleted,实际 %v", err)
	}
}

// TestValidationErrors 基本入参校验。
func TestValidationErrors(t *testing.T) {
	c, _ := newTestCatalog(t)
	if _, err := c.RegisterAsset("", "t", "v1", colsV1); err == nil || mustKind(t, err) != KindValidation {
		t.Fatalf("空 namespace 应为 validation,实际 %v", err)
	}
	if _, err := c.RegisterAsset("dw", "t", "v1", []Column{{Name: "a", Type: "int64"}, {Name: "a", Type: "string"}}); err == nil || mustKind(t, err) != KindValidation {
		t.Fatalf("重复列名应为 validation,实际 %v", err)
	}
	if _, err := c.RegisterAsset("dw", "t", "v1", nil); err == nil || mustKind(t, err) != KindValidation {
		t.Fatalf("空 schema 应为 validation,实际 %v", err)
	}
}

func mustKind(t *testing.T, err error) ErrorKind {
	t.Helper()
	ce, ok := AsError(err)
	if !ok {
		t.Fatalf("期望 *catalog.Error,实际 %T: %v", err, err)
	}
	return ce.Kind
}
