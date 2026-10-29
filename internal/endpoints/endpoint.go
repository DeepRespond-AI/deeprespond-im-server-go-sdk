// Package endpoints 是每个 OpenAPI 接口的元数据（由 internal/codegen/generate.py 从 sdk/server/spec 生成）：
// 接口名、方法、路径、请求类别、批量、分页、超时等。重试、日志、指标都按它处理，各模块的方法不写重试循环。
package endpoints

import "time"

// Category 是请求的类别，决定能否自动重试（《server-go/1.功能概述文档》第 6.1 节）。
type Category uint8

const (
	Query        Category = iota // 查询
	Keyed                        // 带幂等键的写操作
	UniqueCreate                 // 以调用方给出的唯一标识创建
	Idempotent                   // 本身幂等的写操作
	Unsafe                       // 不幂等的写操作：请求发出后不自动重试
)

// Endpoint 是一个接口。
type Endpoint struct {
	Name            string        // 接口名，如 users.create
	Method          string        // HTTP 方法
	Path            string        // 路径模板（省略 /{org_name}/{app_name}），如 /users/{username}
	Category        Category      // 请求的类别
	UnsafeIf        string        // 按参数变为不幂等的条件：online_only、actions_without_version、low_priority
	IdempotencyKey  string        // 调用方没给时由 SDK 生成的幂等键字段（client_msg_id）
	EmptyBody       bool          // 请求体可以省略：参数全为零值时发送 Content-Length: 0
	Timeout         time.Duration // 每次尝试的时限（只有完成上传为 150 秒）
	RateGroup       string        // app_message_rate、request_rate 时共同等待的分组：messages、reports
	BatchField      string        // 批量接口的项列表字段
	BatchMax        int           // 批量的上限
	BatchCharged    bool          // 按项数计入额度（本地限速同样按项数计）
	DuplicateStatus int           // 表示重复提交的状态码（200），0 表示没有
	Pagination      string        // cursor、versioned、seq、export
	EndEmptyString  bool          // 没有下一页时 next_cursor 为空字符串（音视频、推送）
	VersionField    string        // 带版本号的列表：版本号字段
	CountField      string        // 带版本号的列表：数量字段
	MaxCountField   string        // 带版本号的列表：上限字段
	Postprocess     string        // 结果由手写的函数整理（text_check）
	HasResult       bool          // 有响应体（不是 204）
	Internal        bool          // SDK 内部使用（换取令牌）
}
