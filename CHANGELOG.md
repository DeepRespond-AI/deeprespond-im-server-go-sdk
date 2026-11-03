# 更新记录

## v1.0.2 — 2026-11-03

- 新增 AI 对话：机器人管理（client.AI.CreateBot 等）与流式回复 client.AI.StartStream，Write* 写入正文、思考过程、图片、文件、工具进度。
- 持续写入（NDJSON）与按批写入自动切换；Pipe* 直接对接 OpenAI Chat Completions、OpenAI Responses、Anthropic、Gemini 的流。
- 回调处理器收到 ai.stream_stopped 时自动取消本进程中对应流的 Context；新增 AI 相关回调事件。
- 新增 testdata/ai 规格测试数据（与 Python、Node、Java SDK 共用）。
- SDK User-Agent 版本为 1.0.2。

## v1.0.1 — 2026-10-29

- 首次以独立 Go 模块公开发布，包名 drim，Go 1.23 或以上，仅依赖标准库。
- 同步 IM 主仓库最新 Go 服务端 SDK、Demo、示例、测试辅助和签名回调处理。
- 包含独立 RTC 频道管理与强类型回调，以及测试所需的完整签名与事件数据。
- 发布包包含自己的 testdata，解压或克隆后可独立运行单元测试。
- SDK User-Agent 版本为 1.0.1，使用 MIT 许可证。
