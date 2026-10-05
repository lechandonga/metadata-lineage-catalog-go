package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

// canonicalType 把上报的类型名归一化(去空格、转小写),
// 使 int32 与 INT32 视为同一类型。
func canonicalType(t string) string {
	return strings.ToLower(strings.TrimSpace(t))
}

// 整数族内允许的安全收宽顺序,只允许沿箭头方向变宽。
var integerWidth = map[string]int{
	"int8": 1, "int16": 2, "int32": 3, "int64": 4, "int": 4,
}

// floatWidth 为浮点族宽度(int -> float 也算数值族内的拓宽)。
var floatWidth = map[string]int{
	"float32": 5, "float64": 6,
}

// isCompatibleTypeChange 判断列类型从 from 变为 to 是否为兼容(只宽不窄)变化。
func isCompatibleTypeChange(from, to string) bool {
	from = canonicalType(from)
	to = canonicalType(to)
	if from == to {
		return true
	}
	// 数值族拓宽:int8 < int16 < int32 < int64/int < float32 < float64。
	fromW := numericWidth(from)
	toW := numericWidth(to)
	return fromW > 0 && toW > 0 && toW >= fromW
}

func numericWidth(t string) int {
	if w, ok := integerWidth[t]; ok {
		return w
	}
	if w, ok := floatWidth[t]; ok {
		return w
	}
	return 0
}

// fingerprintColumns 对列集合做与顺序无关的内容指纹,
// 用于判断"同一版本号是否上报了相同内容"。
func fingerprintColumns(cols []Column) string {
	copied := make([]Column, len(cols))
	copy(copied, cols)
	sort.Slice(copied, func(i, j int) bool {
		if copied[i].Name != copied[j].Name {
			return copied[i].Name < copied[j].Name
		}
		return copied[i].Type < copied[j].Type
	})
	raw, _ := json.Marshal(copied)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// validateColumns 校验列定义本身合法且列名不重复,返回归一化后的副本。
func validateColumns(cols []Column) ([]Column, *Error) {
	if len(cols) == 0 {
		return nil, errf(KindValidation, "schema 至少需要包含一列")
	}
	out := make([]Column, len(cols))
	seen := make(map[string]int, len(cols))
	for i, c := range cols {
		name := strings.TrimSpace(c.Name)
		typ := canonicalType(c.Type)
		if name == "" {
			return nil, errf(KindValidation, "第 %d 列缺少列名", i+1)
		}
		if typ == "" {
			return nil, errf(KindValidation, "列 %q 缺少类型", name)
		}
		if prev, ok := seen[name]; ok {
			return nil, errf(KindValidation, "列名 %q 重复(第 %d、%d 列)", name, prev+1, i+1)
		}
		seen[name] = i
		out[i] = Column{Name: name, Type: typ, Description: c.Description}
	}
	return out, nil
}

// diffColumns 计算两个有序列集合之间的差异并判定兼容性。
//
// 兼容变化:仅新增列、或同名列在数值族内拓宽。
// 破坏性变化:删列、改名(删除+新增且类型一致时识别为改名)、收窄/任意非数值族类型变更。
func diffColumns(oldCols, newCols []Column) SchemaDiff {
	oldByName := make(map[string]Column, len(oldCols))
	newByName := make(map[string]Column, len(newCols))
	for _, c := range oldCols {
		oldByName[c.Name] = c
	}
	for _, c := range newCols {
		newByName[c.Name] = c
	}

	diff := SchemaDiff{Added: []Column{}, Removed: []Column{}, Changed: []ColumnChange{}}

	for _, c := range newCols {
		if _, ok := oldByName[c.Name]; !ok {
			diff.Added = append(diff.Added, c)
		}
	}
	sortColumns(diff.Added)

	for _, c := range oldCols {
		if _, ok := newByName[c.Name]; !ok {
			diff.Removed = append(diff.Removed, c)
		}
	}
	sortColumns(diff.Removed)

	for _, c := range oldCols {
		if n, ok := newByName[c.Name]; ok && canonicalType(c.Type) != canonicalType(n.Type) {
			diff.Changed = append(diff.Changed, ColumnChange{
				Column: c.Name,
				From:   canonicalType(c.Type),
				To:     canonicalType(n.Type),
			})
		}
	}
	sort.Slice(diff.Changed, func(i, j int) bool { return diff.Changed[i].Column < diff.Changed[j].Column })

	compatible := len(diff.Removed) == 0
	for _, ch := range diff.Changed {
		if !isCompatibleTypeChange(ch.From, ch.To) {
			compatible = false
		}
	}
	diff.Compatible = compatible
	return diff
}

// detectIncompatibilities 在 diff 基础上进一步识别"改名"并给出人类可读原因。
func detectIncompatibilities(diff SchemaDiff) []Incompatibility {
	var out []Incompatibility

	// 改名启发式:一个被删列与一个新增列类型相同,判定为改名(破坏性,因为列名是字段血缘的锚点)。
	removedLeft := map[string]bool{}
	addedLeft := map[string]bool{}
	for _, c := range diff.Removed {
		removedLeft[c.Name] = true
	}
	for _, c := range diff.Added {
		addedLeft[c.Name] = true
	}
	renameTo := map[string]string{} // 旧列名 -> 新列名
	for _, r := range diff.Removed {
		for _, a := range diff.Added {
			if addedLeft[a.Name] && removedLeft[r.Name] && canonicalType(r.Type) == canonicalType(a.Type) {
				renameTo[r.Name] = a.Name
				removedLeft[r.Name] = false
				addedLeft[a.Name] = false
				break
			}
		}
	}
	for oldName, newName := range renameTo {
		out = append(out, Incompatibility{
			Kind:   "renamed_column",
			Column: oldName,
			From:   oldName,
			To:     newName,
			Reason: "列被改名为 " + newName + ",旧列名上的血缘与查询会失效,属于破坏性变化",
		})
	}

	for _, c := range diff.Removed {
		if !removedLeft[c.Name] {
			continue // 已归入改名
		}
		out = append(out, Incompatibility{
			Kind:   "removed_column",
			Column: c.Name,
			From:   c.Type,
			Reason: "列 " + c.Name + " 被删除,下游消费者将无法读到该字段",
		})
	}

	for _, ch := range diff.Changed {
		kind := "type_narrowed"
		reason := "列 " + ch.Column + " 类型从 " + ch.From + " 变为 " + ch.To + ",不属于允许的数值拓宽,可能丢失数据"
		if isCompatibleTypeChange(ch.From, ch.To) {
			continue // 拓宽是兼容的,不计入不兼容列表
		}
		out = append(out, Incompatibility{Kind: kind, Column: ch.Column, From: ch.From, To: ch.To, Reason: reason})
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].Column != out[j].Column {
			return out[i].Column < out[j].Column
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

func sortColumns(cols []Column) {
	sort.Slice(cols, func(i, j int) bool { return cols[i].Name < cols[j].Name })
}
