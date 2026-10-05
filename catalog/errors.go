package catalog

import (
	"errors"
	"fmt"
)

// ErrorKind 用于以编程方式区分失败原因(HTTP 层据此映射状态码)。
type ErrorKind string

const (
	// KindValidation 请求本身不合法(空键、缺列等)。
	KindValidation ErrorKind = "validation_error"
	// KindNotFound 资产或版本不存在。
	KindNotFound ErrorKind = "not_found"
	// KindDeleted 引用的资产已被删除(历史仍可查,但不允许写入新关联)。
	KindDeleted ErrorKind = "asset_deleted"
	// KindAmbiguous 只用短名定位资产时命中多个 namespace。
	KindAmbiguous ErrorKind = "ambiguous_key"
	// KindDangling 血缘端点指向不存在的资产或字段。
	KindDangling ErrorKind = "dangling_reference"
	// KindConflict 同版本内容不一致(重复上报冲突),或自环。
	KindConflict ErrorKind = "conflict"
	// KindCycle 登记该边会形成有向环。
	KindCycle ErrorKind = "cycle_detected"
	// KindIncompatibleSchema schema 发生破坏性变化。
	KindIncompatibleSchema ErrorKind = "incompatible_schema"
	// KindCorrupt 本地持久化文件损坏或含无法识别的记录。
	KindCorrupt ErrorKind = "corrupt_data"
)

// Error 是 catalog 包统一返回的可区分错误。
type Error struct {
	Kind    ErrorKind
	Message string
	// Details 承载结构化细节,例如破坏性变更列表。
	Details any
}

func (e *Error) Error() string { return string(e.Kind) + ": " + e.Message }

// AsError 从任意 error 中取出 *Error。
func AsError(err error) (*Error, bool) {
	var ce *Error
	if errors.As(err, &ce) {
		return ce, true
	}
	return nil, false
}

func errf(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}
