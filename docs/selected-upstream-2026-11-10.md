# 选择性同步上游记录（2026-11-10）

基线：nevermorewish/ccLoad main b7a4990e；参考 caidaoli/ccLoad master d3378d67。

## 合入范围

- 37479185：Anthropic OAuth 动态模型目录、分页及认证刷新，Fable 5.2 价格/fast 支持、新 limits 额度格式、保留 Fable 独立窗口。
- 3fbab316（仅计费部分）：OpenAI 30m/12h 缓存写入解析、成本、管理价格、日志持久化、三库迁移与前端展示；未合入该提交的提示词、图像格式和协议核心改动。
- d3378d67：Anthropic 响应头超额 utilization 归一为 100%，保留无效值检查。
- 4ac3c703、d8eb3372、ab5fec60、c200c91a、fb2c330c、9f365da0、18e66b9b：流终态/截断处理、请求错误分类、拒绝跨源重定向、OAuth 临时故障退避、成功响应正文规则探测。
- da87bd41（最小依赖）：刷新 grant 指纹及持久化回执存储，用于 Anthropic 导入轮换失败恢复；没有整体导入 Codex 刷新流程或 CLIProxyAPI 核心。

## 本地兼容

保留全局原生协议透传、既有确定性请求拒绝、自动冷却总开关、React 后台及用户原有未提交文件。适配 React 全局/渠道价格编辑器和日志 TTL。删除本次新增且只能在跨协议转换开启时运行的上游集成测试，保留原有测试；本分支原生透传回归通过。

修复旧库重启迁移将 OpenAI TTL 写入重复归入 5m 的问题，并增加重复迁移验证。既有用户静态资产保留，新构建入口引用新增的哈希资源。

## 验证

- Go sonic 编译与完整程序构建成功。
- Anthropic 模型发现、导入刷新回执、Fable 窗口、超额响应头、TTL JSON/SSE/计费、成功响应探测、OAuth 退避、跨源重定向和原生透传定向测试通过。
- util、cooldown、storage/schema、storage/sql、storage/sqlite 包测试通过。
- SQLite TTL 迁移/单条批量日志/重启重复迁移、刷新回执及混合存储回执测试通过。
- React TypeScript 检查、17 项测试及生产构建通过。
- 本次涉及旧后台 47 项测试通过；全套旧后台 367 项中 366 项通过，shell 转义用例在原仓库同样失败。
- 执行了全量 go test -tags sonic ./internal/...；原仓库基线有 220 个顶层失败，主要是原有跨协议转换测试与本分支策略冲突及 Windows 相关用例。本次运行有 221 个顶层失败，额外出现的零 TTL 缓存和 xAI 配额时序失败均已在原仓库复现；xAI 单独重跑通过。不能宣称全量测试通过。
- 本次变更的 lint 问题已修复；增量检查剩余两项来自用户原有未跟踪的 internal/model/log_error_category.go。全量 lint 另有既存 Kimi/video/Windows syscall 问题。
- race 未运行成功：当前 Go 环境 CGO_ENABLED=0，且无可用 gcc。MySQL/PostgreSQL 迁移代码与测试已加入，但没有外部数据库集成环境，未验证真实实例。

没有提交、推送、发布或操作线上服务。
