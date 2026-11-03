// Package aistop 是本进程中进行中的 AI 流式回复的登记：drim.Stream 开始时登记、结束时注销，
// 回调处理器收到 ai.stream_stopped 时按流 ID 找到它并停止（《AI对话/3.接口设计》第 7.1 节“停止”）。
package aistop

import "sync"

var streams sync.Map // 流 ID → 停止函数

// Register 登记一条进行中的流。
func Register(streamID string, stop func()) { streams.Store(streamID, stop) }

// Unregister 注销一条流。
func Unregister(streamID string) { streams.Delete(streamID) }

// Stop 停止本进程中的这条流；返回是否找到了。
func Stop(streamID string) bool {
	value, ok := streams.Load(streamID)
	if ok {
		value.(func())()
	}
	return ok
}
