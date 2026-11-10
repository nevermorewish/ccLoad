# 项目测试适配记录（2026-11-10）

测试遵循本 fork 的原生协议透传策略，不重新启用协议转换，不批量跳过失败用例。

- 用四协议 × auto/local × 兼容修复开关 × stream/nonstream 的 96 个拒绝场景替代上游转换成功/探测集成场景。
- 另验证 12 个桥接渠道拒绝场景（同时验证 Admin）、3 个 WebSocket 外协议拒绝场景、2 个 Responses 子资源拒绝场景。
- 保留并适配同协议流聚合、工具调用、缓存计费、模型列表、动态思考、会话隔离、错误日志和取消测试；内部 protocol 转换器单元测试仍运行。
- 修正 Windows 权限位、可执行路径、SSE 换行、零耗时和观察结果发布时序；shell 转义测试在 Git for Windows sh 实际执行。
- 清理弃用测试夹具及 lint 告警；生产代码仅有注释、格式、资源清理和等价分支整理。

验证结果：

| 检查 | 结果 |
| --- | --- |
| golangci-lint run ./... | 0 issues |
| go test -tags sonic ./... | 工作树与 D:/huanxing/ccLoad 均通过 |
| node --test web/assets/js/*.test.js www/build.test.mjs | 373/373 通过 |
| npm run web:test | 17/17 通过 |
| npm run web:typecheck | 通过 |
| 五组时序/稳定性测试 -count=10 | 通过 |
| git diff --check | 通过 |
| race | 未执行成功：CGO_ENABLED=0，环境缺少 C 编译器 |

改动已逐文件核验同步回原仓库，未提交、推送或发布。详细测试替换清单与运行日志保存在 `C:/Users/Administrator/AppData/Local/Temp/ccload-test-adaptation-15mfc22k/evidence`。
