package catalog

import "sort"

func sortStrings(s []string) { sort.Strings(s) }

// sortByCreated 按版本创建时间升序排序;时间相同按版本号兜底,保证确定顺序。
func sortByCreated(versions []string, m map[string]*Version) {
	sort.Slice(versions, func(i, j int) bool {
		vi, vj := m[versions[i]], m[versions[j]]
		if !vi.CreatedAt.Equal(vj.CreatedAt) {
			return vi.CreatedAt.Before(vj.CreatedAt)
		}
		return versions[i] < versions[j]
	})
}
