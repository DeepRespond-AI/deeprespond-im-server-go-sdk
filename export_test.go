package drim

// SetResumePartSize 修改续传时的分片大小（只用于测试），返回恢复的函数。
func SetResumePartSize(n int64) (restore func()) {
	old := resumePartSize
	resumePartSize = n
	return func() { resumePartSize = old }
}
