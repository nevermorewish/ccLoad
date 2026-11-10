# 项目测试契约

本 fork 的代理入口只转发客户端原生协议；`auto`、`local` 历史配置及 `compatibilityRules` 都不能启用跨协议转换。

- 代理集成测试以 `channel_passthrough_test.go` 与 `native_protocol_contract_test.go` 为准：四种协议的请求/响应保真、原生渠道回退、错误原样返回、不兼容渠道零网络请求、Admin 与 Responses WebSocket 拒绝转换。Cursor/Zed/Antigravity 需要协议桥接的渠道在入口拒绝；独立认证及 adapter 单元测试继续运行。
- 测试计费、缓存、超时、取消、日志时，使用与上游相同的客户端协议。`nativeGenerationRequest` 提供基础请求；需要 wire 修复的夹具显式设置 `compatibilityRules`，不得修改共享测试 Server 的生产默认值。
- 上游专有的转换成功、协议自动探测及桥接集成测试已由准入矩阵取代。`internal/protocol/...` 的转换器单元测试仍属于全量测试，保留对内部库的验证。不要用 build tag、全局 skip 或重新启用生产转换来消除失败。
- Windows 不支持 POSIX mode 位，权限位断言仅在 POSIX 执行，文件内容、路径回退、下载校验及复用断言仍在所有平台执行。耗时允许为零；检查事件先后应使用同步信号或等待观察结果发布。SSE 文本夹具由 `.gitattributes` 固定 LF。
- Web 登录命令的转义测试必须执行真实 POSIX shell。Windows 可用 Git for Windows 的 `sh.exe`，不以跳过替代验证。
- 全量命令：`go test -tags sonic ./...`、`golangci-lint run ./...`、`make verify-web`。Windows 无 make 时先按 Makefile 的 `www-setup` 复制站点共享资源，再运行 `node --test web/assets/js/*.test.js www/build.test.mjs`、`npm run web:test`；React 类型检查为 `npm run web:typecheck`。race 需要 CGO 和 C 编译器，普通测试通过不代表 race 已通过。

## 测试并行化

调整测试并行度时读取；普通功能修改不要求顺带并行化测试。

`make race-fast` 运行高价值 race 子集，`make race` 运行全量；可用 `RACE_P` / `RACE_PARALLEL` 调整并行度。

- 积极 `t.Parallel()`:并行测试必须独立 Store/Server、随机监听端口、局部 mock,不共享可变 fixture;调用 `t.Setenv`/`t.Chdir`,或修改进程环境、工作目录、`http.DefaultTransport`、全局模型目录、包级 session/cache、全局 goroutine 计数的测试必须串行
- 优先并行化含 sleep、deadline、轮询、异步日志等待的高耗时测试;不为凑并行数改纯解析微测试
- `newInMemoryServer*` 在 worker 启动前将实例日志批处理周期设为 20ms，保留真实异步持久化；验证生产批处理周期的测试直接使用 `NewServer` / `NewLogService`，默认仍为 1 秒。不要修改全局周期或在 worker 启动后改写实例周期。
- 并行化前后同命令计时(`/usr/bin/time -p go test -tags sonic -count=1 ./internal/app`),收益属噪声就撤销;新增/调整并行测试后跑 `go test -race -tags sonic -count=1 -shuffle=on ./internal/app`
