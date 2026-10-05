package catalog

import "sort"

// ErrorKind 用稳定的机器可读编码区分不同失败原因，
// HTTP 层据此映射状态码，测试据此断言失败类别。
type ErrorKind string

const (
	ErrValidation         ErrorKind = "validation_error"          // 请求本身不合法
	ErrNotFound           ErrorKind = "not_found"                 // 资产/字段/版本不存在
	ErrAmbiguousKey       ErrorKind = "ambiguous_key"             // 业务键有歧义（别名指向多个资产）
	ErrDanglingReference  ErrorKind = "dangling_reference"        // 血缘指向不存在的资产或字段
	ErrConflict           ErrorKind = "conflict"                  // 同一版本内容不一致 / 已删除资产被修改
	ErrIncompatibleSchema ErrorKind = "incompatible_schema"       // 破坏性 schema 演进
	ErrCorruptStore       ErrorKind = "corrupt_store"             // 落盘数据损坏
	ErrUnsupportedVersion ErrorKind = "unsupported_store_version" // 存储信封版本不认识
)

// CatalogError 携带可区分的错误类别与上下文细节。
type CatalogError struct {
	Kind   ErrorKind
	Reason string
	// Details 用于歧义/悬空/不兼容等场景逐项列出问题。
	Details []string
	Changes []IncompatibleChange
}

func (e *CatalogError) Error() string {
	msg := string(e.Kind) + ": " + e.Reason
	if len(e.Details) > 0 {
		d := append([]string(nil), e.Details...)
		sort.Strings(d)
		msg += " [" + joinStrings(d, "; ") + "]"
	}
	return msg
}

// ValidationError 是本地参数校验错误的轻量实现。
type ValidationError struct{ Reason string }

func (e *ValidationError) Error() string { return string(ErrValidation) + ": " + e.Reason }

func joinStrings(ss []string, sep string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

func asCatalogError(err error) (*CatalogError, bool) {
	if ce, ok := err.(*CatalogError); ok {
		return ce, true
	}
	if ve, ok := err.(*ValidationError); ok {
		return &CatalogError{Kind: ErrValidation, Reason: ve.Reason}, true
	}
	return nil, false
}
