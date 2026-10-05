package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// 类型层级：数字下标越大越宽。宽化（int32 -> int64）兼容，
// 收窄（int64 -> int32）属于破坏性变化。
//
// 不在同一条宽化链上的两个类型，默认视为破坏性类型变化
// （例如 string -> int），需要调用方显式确认。
var numericRank = map[string]int{
	"int8": 1, "int16": 2, "int32": 3, "int64": 4,
	"uint8":   1,
	"float32": 5, "float64": 6,
}

// decimal 与 bigint 比所有整数宽、比浮点语义独立，这里把
// decimal 视作可由任意数值类型宽化而来，但 decimal 不能降级为浮点。

// typeCompatible 判断 old -> new 是否为兼容（宽化）变化。
// 返回 compatible=true 表示静默兼容；false 时 reason 说明原因。
func typeCompatible(old, newCol Column) (compatible bool, reason string) {
	if old.Type == newCol.Type {
		return true, ""
	}
	o, oNum := numericRank[old.Type]
	n, nNum := numericRank[newCol.Type]
	switch {
	case old.Type == "decimal" && newCol.Type == "string":
		return false, "类型由 decimal 变为 string，语义不兼容"
	case newCol.Type == "decimal":
		// 任意数值 -> decimal 是宽化；非数值 -> decimal 不兼容。
		if oNum || old.Type == "decimal" {
			return true, ""
		}
		return false, "类型由 " + old.Type + " 收窄/转换为 decimal，语义不兼容"
	case oNum && nNum:
		// 有符号/无符号、整型/浮点之间只允许同方向宽化，
		// 跨族（uint->int、int->float）按破坏性处理以避免精度/符号问题。
		if sameTypeFamily(old.Type, newCol.Type) && n >= o {
			return true, ""
		}
		return false, "类型由 " + old.Type + " 收窄或跨族转换为 " + newCol.Type
	case old.Type == "string" && (nNum || newCol.Type == "decimal" || newCol.Type == "bool"):
		return false, "类型由 string 变为 " + newCol.Type + "，属于窄化解析，存量数据可能无法转换"
	case newCol.Type == "string" && (oNum || old.Type == "bool"):
		// 数值/bool -> string 是安全宽化（文本表达不丢值）。
		return true, ""
	}
	return false, "类型由 " + old.Type + " 变为 " + newCol.Type + "，不在受支持的兼容宽化链上"
}

func sameTypeFamily(a, b string) bool {
	return signedInt(a) && signedInt(b) ||
		a == "float32" && b == "float64" ||
		a == "uint8" && b == "uint8"
}

func signedInt(t string) bool {
	switch t {
	case "int8", "int16", "int32", "int64":
		return true
	}
	return false
}

// diffColumns 计算两个列集合之间的差异，并用简单启发式识别改名：
// 若旧版中被删的列与新版中新增的列类型完全相同，且存在唯一配对，
// 则认定为改名（破坏性：按名引用的下游会断）。
func diffColumns(oldCols, newCols []Column) (added, removed []Column, changed []ColumnChange, renames []IncompatibleChange) {
	oldMap := map[string]Column{}
	newMap := map[string]Column{}
	for _, c := range oldCols {
		oldMap[c.Name] = c
	}
	for _, c := range newCols {
		newMap[c.Name] = c
	}

	var dropped []Column
	for _, c := range oldCols {
		if _, ok := newMap[c.Name]; !ok {
			dropped = append(dropped, c)
		}
	}
	var addedCols []Column
	for _, c := range newCols {
		if _, ok := oldMap[c.Name]; !ok {
			addedCols = append(addedCols, c)
		}
	}

	// 改名启发式：按 (type, nullable) 配对，唯一匹配才算。
	renameOld := map[string]bool{}
	renameNew := map[string]bool{}
	for _, d := range dropped {
		var matches []string
		for _, a := range addedCols {
			if a.Type == d.Type && a.Nullable == d.Nullable {
				matches = append(matches, a.Name)
			}
		}
		if len(matches) == 1 {
			renameOld[d.Name] = true
			renameNew[matches[0]] = true
			renames = append(renames, IncompatibleChange{
				Kind:   "renamed_column",
				Column: matches[0],
				Old:    d.Name,
				New:    matches[0],
				Reason: "列 " + d.Name + " 疑似改名为 " + matches[0] + "（类型完全一致且配对唯一），按名引用的下游血缘会失效",
			})
		}
	}
	for _, a := range addedCols {
		if !renameNew[a.Name] {
			added = append(added, a)
		}
	}
	for _, d := range dropped {
		if !renameOld[d.Name] {
			removed = append(removed, d)
		}
	}
	for _, c := range newCols {
		o, ok := oldMap[c.Name]
		if !ok {
			continue
		}
		if o.Type != c.Type || o.Nullable != c.Nullable {
			changed = append(changed, ColumnChange{
				Name:        c.Name,
				OldType:     o.Type,
				NewType:     c.Type,
				OldNullable: o.Nullable,
				NewNullable: c.Nullable,
			})
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i].Name < added[j].Name })
	sort.Slice(removed, func(i, j int) bool { return removed[i].Name < removed[j].Name })
	sort.Slice(changed, func(i, j int) bool { return changed[i].Name < changed[j].Name })
	sort.Slice(renames, func(i, j int) bool { return renames[i].Column < renames[j].Column })
	return
}

// checkCompatibility 返回从 oldCols 演进到 newCols 的所有破坏性变化。
// 返回空切片表示完全兼容。
func checkCompatibility(oldCols, newCols []Column) []IncompatibleChange {
	added, removed, changed, renames := diffColumns(oldCols, newCols)
	var inc []IncompatibleChange
	inc = append(inc, renames...)
	for _, c := range removed {
		inc = append(inc, IncompatibleChange{
			Kind:   "dropped_column",
			Column: c.Name,
			Old:    c.Type,
			Reason: "列 " + c.Name + " 被删除，按名引用该列的下游作业会失败",
		})
	}
	for _, ch := range changed {
		oc := Column{Name: ch.Name, Type: ch.OldType, Nullable: ch.OldNullable}
		nc := Column{Name: ch.Name, Type: ch.NewType, Nullable: ch.NewNullable}
		ok, why := typeCompatible(oc, nc)
		if !ok {
			inc = append(inc, IncompatibleChange{
				Kind:   "narrowed_type",
				Column: ch.Name,
				Old:    ch.OldType,
				New:    ch.NewType,
				Reason: why,
			})
		}
		// 可空 -> 非空是收紧约束（存量 NULL 行会写入失败）。
		if ch.OldNullable && !ch.NewNullable {
			inc = append(inc, IncompatibleChange{
				Kind:   "nullability_tightened",
				Column: ch.Name,
				Old:    "nullable",
				New:    "not null",
				Reason: "列 " + ch.Name + " 由可空收紧为 NOT NULL，存量 NULL 数据无法写入",
			})
		}
	}
	_ = added
	sort.Slice(inc, func(i, j int) bool {
		if inc[i].Column != inc[j].Column {
			return inc[i].Column < inc[j].Column
		}
		return inc[i].Kind < inc[j].Kind
	})
	return inc
}

func contentHash(assetID string, version int, cols []Column) string {
	type canonical struct {
		AssetID string   `json:"a"`
		Version int      `json:"v"`
		Columns []Column `json:"c"`
	}
	b, _ := json.Marshal(canonical{AssetID: assetID, Version: version, Columns: cols})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ValidateColumns 检查列定义本身是否合法：列名非空、不重复、类型已知。
func ValidateColumns(cols []Column) error {
	seen := map[string]bool{}
	for _, c := range cols {
		if c.Name == "" {
			return &ValidationError{Reason: "存在未命名的列"}
		}
		if seen[c.Name] {
			return &ValidationError{Reason: "列名重复: " + c.Name}
		}
		seen[c.Name] = true
		if !knownType(c.Type) {
			return &ValidationError{Reason: "未知数据类型: " + c.Type}
		}
	}
	return nil
}

func knownType(t string) bool {
	if t == "string" || t == "bool" || t == "decimal" || t == "bytes" || t == "date" || t == "timestamp" {
		return true
	}
	_, ok := numericRank[t]
	return ok
}
