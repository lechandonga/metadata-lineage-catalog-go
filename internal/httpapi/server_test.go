package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/lechandonga/metadata-lineage-catalog-go/internal/catalog"
)

func newTestServer(t *testing.T) (http.Handler, func()) {
	t.Helper()
	dir := t.TempDir()
	c, err := catalog.New(catalog.NewFileStore(filepath.Join(dir, "state.json")), nil)
	if err != nil {
		t.Fatal(err)
	}
	return New(c), func() {}
}

func do(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是 JSON: %s", rec.Body.String())
		}
	}
	return rec.Code, out
}

func errKind(t *testing.T, out map[string]any) string {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("响应缺少 error: %+v", out)
	}
	k, _ := e["kind"].(string)
	return k
}

// TestHTTPEndToEnd 覆盖业务：通过本地 HTTP 完成 资产登记 ->
// schema 演进（含破坏性拒绝与差异查询）-> 血缘登记/撤销 ->
// 影响面查询 的手工验证主路径，并核对错误码可区分。
func TestHTTPEndToEnd(t *testing.T) {
	h, cleanup := newTestServer(t)
	defer cleanup()

	if code, _ := do(t, h, "GET", "/healthz", nil); code != 200 {
		t.Fatalf("healthz=%d", code)
	}

	// 登记两个资产。
	for _, key := range []string{"db.orders", "db.dwd_orders"} {
		if code, out := do(t, h, "POST", "/assets", map[string]any{"key": key, "name": key}); code != 201 {
			t.Fatalf("登记资产 %s: code=%d body=%+v", key, code, out)
		}
	}

	// 初始版本。
	v1Body := map[string]any{"columns": []map[string]any{
		{"name": "id", "type": "int32", "nullable": false},
		{"name": "amt", "type": "int64", "nullable": true},
	}}
	if code, out := do(t, h, "POST", "/assets/db.orders/versions", v1Body); code != 201 {
		t.Fatalf("v1: %d %+v", code, out)
	}
	v2Body := map[string]any{"columns": []map[string]any{
		{"name": "id", "type": "int32", "nullable": false},
		{"name": "id", "type": "int64", "nullable": false},
	}}
	_ = v2Body

	// 兼容演进 v2：宽化 + 新增列。
	v2 := map[string]any{"columns": []map[string]any{
		{"name": "id", "type": "int64", "nullable": false},
		{"name": "amt", "type": "int64", "nullable": true},
		{"name": "ts", "type": "timestamp", "nullable": true},
	}}
	if code, out := do(t, h, "POST", "/assets/db.orders/versions", v2); code != 201 {
		t.Fatalf("v2 兼容演进: %d %+v", code, out)
	}
	// 重复上报幂等。
	if code, out := do(t, h, "POST", "/assets/db.orders/versions", v2); code != 200 || out["created"] != false {
		t.Fatalf("重复上报应 200 created=false: %d %+v", code, out)
	}
	// 破坏性演进：422 + incompatible_schema。
	bad := map[string]any{"columns": []map[string]any{
		{"name": "id", "type": "int32", "nullable": false}, // 收窄
		// amt、ts 删除
	}}
	code, out := do(t, h, "POST", "/assets/db.orders/versions", bad)
	if code != 422 || errKind(t, out) != "incompatible_schema" {
		t.Fatalf("破坏性变更应 422 incompatible_schema: %d %+v", code, out)
	}
	changes, _ := out["error"].(map[string]any)["incompatible_changes"].([]any)
	if len(changes) < 3 {
		t.Fatalf("应逐项列出破坏性变化, got %+v", changes)
	}

	// 版本差异。
	code, out = do(t, h, "GET", "/assets/db.orders/diff?from=1&to=2", nil)
	if code != 200 {
		t.Fatalf("diff: %d %+v", code, out)
	}
	added, _ := out["added"].([]any)
	changed, _ := out["changed"].([]any)
	if len(added) != 1 || len(changed) != 1 {
		t.Fatalf("diff 加一改一, got added=%v changed=%v", added, changed)
	}

	// 目的资产 schema。
	dst := map[string]any{"columns": []map[string]any{
		{"name": "order_id", "type": "int64", "nullable": false},
	}}
	if code, out := do(t, h, "POST", "/assets/db.dwd_orders/versions", dst); code != 201 {
		t.Fatalf("dst v1: %d %+v", code, out)
	}

	// 血缘：悬空字段 -> 422 dangling_reference。
	code, out = do(t, h, "POST", "/edges", map[string]any{
		"src_asset_key": "db.orders", "src_column": "ghost",
		"dst_asset_key": "db.dwd_orders", "dst_column": "order_id",
	})
	if code != 422 || errKind(t, out) != "dangling_reference" {
		t.Fatalf("悬空字段: %d %+v", code, out)
	}
	// 资产不存在 -> 404 not_found。
	code, out = do(t, h, "POST", "/edges", map[string]any{
		"src_asset_key": "nope", "src_column": "id",
		"dst_asset_key": "db.dwd_orders", "dst_column": "order_id",
	})
	if code != 404 || errKind(t, out) != "not_found" {
		t.Fatalf("不存在资产: %d %+v", code, out)
	}
	// 正常登记 + 幂等。
	edge := map[string]any{
		"src_asset_key": "db.orders", "src_column": "id",
		"dst_asset_key": "db.dwd_orders", "dst_column": "order_id",
	}
	if code, out := do(t, h, "POST", "/edges", edge); code != 201 || out["created"] != true {
		t.Fatalf("首次建边: %d %+v", code, out)
	}
	if code, out := do(t, h, "POST", "/edges", edge); code != 200 || out["created"] != false {
		t.Fatalf("重复建边幂等: %d %+v", code, out)
	}

	// 影响面与路径解释。
	code, out = do(t, h, "GET", "/impact?asset_key=db.orders&column=id", nil)
	if code != 200 {
		t.Fatalf("impact: %d %+v", code, out)
	}
	impacts, _ := out["impact"].([]any)
	if len(impacts) != 1 {
		t.Fatalf("影响面应含 1 个字段, got %+v", impacts)
	}
	path, _ := impacts[0].(map[string]any)["path"].([]any)
	if len(path) != 2 {
		t.Fatalf("影响路径应为 2 跳, got %+v", path)
	}

	// 撤销边：deleted=true，再撤一次 deleted=false。
	if code, out := do(t, h, "DELETE", "/edges", edge); code != 200 || out["deleted"] != true {
		t.Fatalf("首次撤销: %d %+v", code, out)
	}
	if code, out := do(t, h, "DELETE", "/edges", edge); code != 200 || out["deleted"] != false {
		t.Fatalf("重复撤销幂等: %d %+v", code, out)
	}
}
