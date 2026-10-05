package catalog

import "testing"

// TestSchemaCompatibility 覆盖 schema 兼容与破坏性变更的判定规则。
// 每个用例名称即业务场景,日志输出判定依据。
func TestSchemaCompatibility(t *testing.T) {
	base := []Column{
		{Name: "id", Type: "int64"},
		{Name: "name", Type: "string"},
		{Name: "age", Type: "int32"},
	}

	cases := []struct {
		name       string
		newCols    []Column
		compatible bool
		kinds      []string // 破坏性场景下预期的不兼容类型
	}{
		{
			name:       "新增列是兼容变化",
			newCols:    append(cloneCols(base), Column{Name: "email", Type: "string"}),
			compatible: true,
		},
		{
			name:       "仅列顺序调整不算变化",
			newCols:    []Column{base[2], base[0], base[1]},
			compatible: true,
		},
		{
			name:       "int32 拓宽为 int64 是兼容变化",
			newCols:    []Column{{Name: "id", Type: "int64"}, {Name: "name", Type: "string"}, {Name: "age", Type: "int64"}},
			compatible: true,
		},
		{
			name:       "删列是破坏性变化",
			newCols:    []Column{{Name: "id", Type: "int64"}, {Name: "name", Type: "string"}},
			compatible: false,
			kinds:      []string{"removed_column"},
		},
		{
			name:       "收窄类型 int64->int32 是破坏性变化",
			newCols:    []Column{{Name: "id", Type: "int32"}, {Name: "name", Type: "string"}, {Name: "age", Type: "int32"}},
			compatible: false,
			kinds:      []string{"type_narrowed"},
		},
		{
			name:       "string 改为 int64 是破坏性变化",
			newCols:    []Column{{Name: "id", Type: "int64"}, {Name: "name", Type: "int64"}, {Name: "age", Type: "int32"}},
			compatible: false,
			kinds:      []string{"type_narrowed"},
		},
		{
			name:       "改名(同类型删+增)识别为 renamed_column 破坏性变化",
			newCols:    []Column{{Name: "id", Type: "int64"}, {Name: "full_name", Type: "string"}, {Name: "age", Type: "int32"}},
			compatible: false,
			kinds:      []string{"renamed_column"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diffColumns(base, tc.newCols)
			t.Logf("[判定依据] compatible=%v added=%d removed=%d changed=%d", d.Compatible, len(d.Added), len(d.Removed), len(d.Changed))
			if d.Compatible != tc.compatible {
				t.Fatalf("兼容性判定 = %v,预期 %v", d.Compatible, tc.compatible)
			}
			if !tc.compatible {
				incompat := detectIncompatibilities(d)
				gotKinds := map[string]bool{}
				for _, inc := range incompat {
					t.Logf("  不兼容项 kind=%s column=%s 原因=%s", inc.Kind, inc.Column, inc.Reason)
					gotKinds[inc.Kind] = true
				}
				for _, k := range tc.kinds {
					if !gotKinds[k] {
						t.Fatalf("未标出预期的不兼容类型 %s,实际 %v", k, gotKinds)
					}
				}
			}
		})
	}
}

func cloneCols(cols []Column) []Column {
	out := make([]Column, len(cols))
	copy(out, cols)
	return out
}

// TestFingerprintStable 指纹与列顺序无关,供幂等判定使用。
func TestFingerprintStable(t *testing.T) {
	a := []Column{{Name: "a", Type: "int64"}, {Name: "b", Type: "string"}}
	b := []Column{{Name: "b", Type: "string"}, {Name: "a", Type: "int64"}}
	if fingerprintColumns(a) != fingerprintColumns(b) {
		t.Fatal("列顺序不同但内容相同时指纹应一致")
	}
	c := []Column{{Name: "a", Type: "int32"}, {Name: "b", Type: "string"}}
	if fingerprintColumns(a) == fingerprintColumns(c) {
		t.Fatal("类型不同指纹不应相同")
	}
}
