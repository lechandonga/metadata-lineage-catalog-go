package catalog

import (
	"testing"
	"time"
)

func newTestCatalog(t *testing.T) *Catalog {
	t.Helper()
	c, err := New(nil, func() time.Time { return time.Unix(1700000000, 0).UTC() })
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func mustAsset(t *testing.T, c *Catalog, key string) {
	t.Helper()
	if _, _, err := c.RegisterAsset(RegisterAssetInput{Key: key, Name: key}); err != nil {
		t.Fatalf("RegisterAsset(%s): %v", key, err)
	}
}

// TestSchemaCompatibility 覆盖业务：schema 兼容/破坏性演进判定。
// 判定依据：
//   - 新增列、同族整型宽化、放宽为可空 => 兼容，新版本放行；
//   - 删列、收窄类型、跨族转换、可空收紧、疑似改名 => incompatible_schema，
//     且 Changes 逐项标明 kind 与原因，不允许静默覆盖。
func TestSchemaCompatibility(t *testing.T) {
	c := newTestCatalog(t)
	mustAsset(t, c, "dw.user")

	v1, created, err := c.PutVersion("dw.user", []Column{
		{Name: "id", Type: "int32", Nullable: false},
		{Name: "age", Type: "int16", Nullable: true},
		{Name: "name", Type: "string", Nullable: true},
	})
	if err != nil || !created || v1.Version != 1 {
		t.Fatalf("v1 登记失败: %+v (created=%v)", err, created)
	}

	t.Run("兼容演进_新增列与宽化类型与放宽可空", func(t *testing.T) {
		v2, created, err := c.PutVersion("dw.user", []Column{
			{Name: "id", Type: "int64", Nullable: false}, // int32 -> int64 宽化
			{Name: "age", Type: "int32", Nullable: true}, // int16 -> int32 宽化
			{Name: "name", Type: "string", Nullable: true},
			{Name: "city", Type: "string", Nullable: true}, // 新增列
		})
		if err != nil || !created || v2.Version != 2 {
			t.Fatalf("兼容变更应放行, got %+v err=%v", v2, err)
		}
	})

	t.Run("幂等_相同内容重复上报不产生新版本", func(t *testing.T) {
		cols := []Column{
			{Name: "id", Type: "int64", Nullable: false},
			{Name: "age", Type: "int32", Nullable: true},
			{Name: "name", Type: "string", Nullable: true},
			{Name: "city", Type: "string", Nullable: true},
		}
		v, created, err := c.PutVersion("dw.user", cols)
		if err != nil || created || v.Version != 2 {
			t.Fatalf("重复上报应幂等返回 v2, got v=%d created=%v err=%v", v.Version, created, err)
		}
	})

	t.Run("破坏性_删列收窄类型收紧可空_逐项报错", func(t *testing.T) {
		_, _, err := c.PutVersion("dw.user", []Column{
			{Name: "id", Type: "int32", Nullable: false},  // int64 -> int32 收窄
			{Name: "age", Type: "int32", Nullable: false}, // 可空 -> 非空
			// name、city 被删除
		})
		if kindOf(err) != ErrIncompatibleSchema {
			t.Fatalf("期望 incompatible_schema, got %v", err)
		}
		changes := changesOf(err)
		want := map[string]bool{
			"dropped_column:name":       false,
			"dropped_column:city":       false,
			"narrowed_type:id":          false,
			"nullability_tightened:age": false,
		}
		for _, ch := range changes {
			key := string(ch.Kind) + ":" + ch.Column
			if _, ok := want[key]; !ok {
				t.Errorf("出现未预期的破坏性变化: %s (%s)", key, ch.Reason)
				continue
			}
			if ch.Reason == "" {
				t.Errorf("%s 缺少原因说明", key)
			}
			want[key] = true
		}
		for k, found := range want {
			if !found {
				t.Errorf("未检出破坏性变化 %s；实际检出: %+v", k, changes)
			}
		}
		// 被拒绝后历史不可变：最新仍为 v2。
		if vs, _ := c.Versions("dw.user"); len(vs) != 2 {
			t.Fatalf("被拒绝的写入不得产生新版本, got %d 个版本", len(vs))
		}
	})

	t.Run("破坏性_改名按启发式识别", func(t *testing.T) {
		_, _, err := c.PutVersion("dw.user", []Column{
			{Name: "id", Type: "int64", Nullable: false},
			{Name: "age", Type: "int32", Nullable: true},
			{Name: "name", Type: "string", Nullable: true},
			{Name: "hometown", Type: "string", Nullable: true}, // city 改名
		})
		if kindOf(err) != ErrIncompatibleSchema {
			t.Fatalf("期望 incompatible_schema, got %v", err)
		}
		foundRename := false
		for _, ch := range changesOf(err) {
			if ch.Kind == "renamed_column" && ch.Old == "city" && ch.New == "hometown" {
				foundRename = true
			}
		}
		if !foundRename {
			t.Fatalf("应识别 city -> hometown 的改名, got %+v", changesOf(err))
		}
	})

	t.Run("跨族转换_int到float视为破坏性", func(t *testing.T) {
		mustAsset(t, c, "dw.amount")
		if _, _, err := c.PutVersion("dw.amount", []Column{{Name: "v", Type: "int32"}}); err != nil {
			t.Fatal(err)
		}
		_, _, err := c.PutVersion("dw.amount", []Column{{Name: "v", Type: "float32"}})
		if kindOf(err) != ErrIncompatibleSchema {
			t.Fatalf("int32->float32 应判为破坏性, got %v", err)
		}
	})

	t.Run("非法列定义被拒绝", func(t *testing.T) {
		mustAsset(t, c, "dw.bad")
		_, _, err := c.PutVersion("dw.bad", []Column{
			{Name: "a", Type: "weird_type"},
		})
		if kindOf(err) != ErrValidation {
			t.Fatalf("未知类型应 validation_error, got %v", err)
		}
		_, _, err = c.PutVersion("dw.bad", []Column{
			{Name: "a", Type: "string"},
			{Name: "a", Type: "int32"},
		})
		if kindOf(err) != ErrValidation {
			t.Fatalf("重复列名应 validation_error, got %v", err)
		}
	})
}

// TestVersionDiff 覆盖业务：任意两个历史版本之间加了/删了/改了什么，
// 以及历史版本不可变、可按版本号查询。
func TestVersionDiff(t *testing.T) {
	c := newTestCatalog(t)
	mustAsset(t, c, "dw.order")
	mustPutVersion := func(cols []Column) int {
		t.Helper()
		v, _, err := c.PutVersion("dw.order", cols)
		if err != nil {
			t.Fatalf("PutVersion: %v", err)
		}
		return v.Version
	}
	mustPutVersion([]Column{
		{Name: "id", Type: "int32", Nullable: false},
		{Name: "amt", Type: "int64", Nullable: true},
	})
	mustPutVersion([]Column{
		{Name: "id", Type: "int64", Nullable: false},
		{Name: "amt", Type: "int64", Nullable: true},
		{Name: "ts", Type: "timestamp", Nullable: true},
	})

	// v1 -> v2: 加 ts, 改 id 类型。
	d, err := c.DiffVersions("dw.order", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Added) != 1 || d.Added[0].Name != "ts" {
		t.Fatalf("Added 不符: %+v", d.Added)
	}
	if len(d.Removed) != 0 {
		t.Fatalf("Removed 应为空: %+v", d.Removed)
	}
	if len(d.Changed) != 1 || d.Changed[0].Name != "id" || d.Changed[0].OldType != "int32" || d.Changed[0].NewType != "int64" {
		t.Fatalf("Changed 不符: %+v", d.Changed)
	}

	// 反向 v2 -> v1: 删 ts。
	d, err = c.DiffVersions("dw.order", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Removed) != 1 || d.Removed[0].Name != "ts" || len(d.Added) != 0 {
		t.Fatalf("反向 Diff 不符: %+v", d)
	}

	// 不存在的版本号明确 not_found。
	if _, err := c.DiffVersions("dw.order", 1, 9); kindOf(err) != ErrNotFound {
		t.Fatalf("缺失版本应 not_found, got %v", err)
	}

	// 历史版本仍可逐版查询且内容不变。
	v1, err := c.GetVersion("dw.order", 1)
	if err != nil {
		t.Fatal(err)
	}
	if v1.Columns[0].Type != "int32" {
		t.Fatalf("v1 必须保持不可变, got %s", v1.Columns[0].Type)
	}
}
