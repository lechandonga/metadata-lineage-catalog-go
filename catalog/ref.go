package catalog

import "strings"

// AssetRef 定位一个资产。Namespace 可空,空时按短名查找(可能歧义)。
type AssetRef struct {
	Namespace string
	Asset     string
}

func (r AssetRef) validate() *Error {
	r.Asset = strings.TrimSpace(r.Asset)
	if r.Asset == "" {
		return errf(KindValidation, "asset 名称不能为空")
	}
	return nil
}

// ParseFieldRef 解析形如 namespace/asset.column 的字段全限定名。
func ParseFieldRef(s string) (FieldRef, *Error) {
	s = strings.TrimSpace(s)
	slash := strings.Index(s, "/")
	dot := strings.LastIndex(s, ".")
	if slash <= 0 || dot < slash+1 || dot == len(s)-1 {
		return FieldRef{}, errf(KindValidation, "字段引用格式必须为 namespace/asset.column,收到 %q", s)
	}
	f := FieldRef{
		Namespace: s[:slash],
		Asset:     s[slash+1 : dot],
		Column:    s[dot+1:],
	}
	if err := f.validate(); err != nil {
		return FieldRef{}, err
	}
	return f, nil
}

func (f FieldRef) validate() *Error {
	if strings.TrimSpace(f.Namespace) == "" || strings.TrimSpace(f.Asset) == "" || strings.TrimSpace(f.Column) == "" {
		return errf(KindValidation, "字段引用必须包含 namespace、asset、column,收到 %q", f.String())
	}
	return nil
}
