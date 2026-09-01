# cline2api 代码审计与流程梳理报告

- 审计日期：2026-09-01 · 审计范围：根目录全部 Go 源码（~25 文件 ≈ 6500 行）+ 内嵌前端 `admin_html.go` + CI/部署文件
- 方法：4 个并行维度审计（并发与生命周期 / 请求路径正确性 / 管理面与安全 / 数据格式与边界）+ **每条发现逐条人工复核**（Read/Grep 复验后才收录）；静态兜底 `go vet ./...`（零告警）、`go test -race ./...`（通过，但测试套未以高并发压测，不能据此断言无竞态）
- 标注：无标记 = 已亲自复核；「假设」= 依赖上游行为的推断；「代理实证」= 审计代理以临时程序实测确认

---

## 1. 流程梳理

### 1.1 入口与生命周期

```
main.go (!desktop)                    desktop_main.go (desktop)
  -login/-add-account → 设备授权加号     startProxy(协程) → 轮询 /health → Wails WebView → /admin/
  -capture        → MITM 录制
  -list           → 列出账号
  -start          → build+launch+开浏览器
  默认 → startProxy(host, port) → select{} 永久阻塞（restartListener 可接管监听）
```

`startProxy`（proxy.go:225）初始化顺序：

1. 预热所有账号 token（`ensureAccountToken`）
2. `startModelSync`（一次性拉取 Cline 官方模型表，异步）
3. zen 启用时 `startZenModelsRefresher`（**仅启动时已启用才注册**，10 分钟周期 + 启动后 ~2s 首次）
4. `startCompactCleanup`（周期清理 >24h 会话压缩状态）
5. `freePort`（**Windows PowerShell 强杀占用端口的进程**，proxy.go:2007-2019）
6. 构建 `http.ServeMux`（全部路由）→ `startCooldownRecovery`（30s 周期）
7. `ListenAndServe`（`http.Server` 无任何超时配置）

管理端改监听地址 → `handleAdminUpdateConfig` 起协程调 `restartListener`（proxy.go:129-160）：**先 Shutdown 旧 server（2s）再 bind 新地址**，bind 失败则零监听（见 P1-9）。

### 1.2 请求调用链（三条协议 × routeModel 三分支）

```
/v1/chat/completions | /v1/messages | /v1/responses        （+ 无版本别名）
  └ corsHandler → apiKeyHandler（无 keys 配置 = 开放中继）→ 协议转换 → routeModel(zen.go:172)
       ├─ "reject"  zen 可解析但非免费 → 400
       ├─ "zen"     maybeCompact(compact.go) → callZenAPI(zen.go:503)
       │              uTLS Chrome-120 + H2 客户端、出口代理池、身份轮换、并发信号量 zenSem(8)
       └─ "cline"   model="free" → callFreeClineAPI（freeModelChain × 账号遍历）
                    其余 → pickAccountForModel(策略+模型冷却) → callClineAPIWithAccount
                           buildUpstreamBody → clineHeaders(+管理端覆盖头) → api.cline.bot
                           401 → 刷新 token → 重试一次
  响应：unwrapDataEnvelope → normalizeOpenAIResponse → parseTokenUsage → recordTokenUsage
        → finalizeRequestLog；三条独立 SSE 转换路径（见 1.5）
```

### 1.3 后台任务

| 启动器 | 周期 | 职责 |
|---|---|---|
| startCooldownRecovery | 30s | 探活冷却账号（真实 "hi" 请求），成功即复活 |
| startZenModelsRefresher | 10min | 全量替换 zen 模型表（仅启动时 zen 已启用才启动） |
| startCompactCleanup | 周期 | 清理 >24h 压缩状态 |
| startModelSync | 一次性 | 拉取 Cline 模型表，仅替换 Source=remote 条目 |
| pollWorkosToken | 登录期间 | 设备码轮询（阻塞式，无超时 client） |

### 1.4 数据文件读写矩阵

| 文件 | 写方 | 原子性 | 损坏后果 |
|---|---|---|---|
| .cline-accounts.json | savePool（~25 处调用点，**无锁 Marshal + 直接覆盖写**） | ✗ 直接 WriteFile | 静默空池 → 下次保存覆盖销毁 |
| .cline-config.json | saveProxyConfigLocked | ✗ | 静默回退默认 |
| .cline-zen.json | setZenConfig | ✗ | 静默重置（key→public、代理池清空） |
| .cline-request-logs.json | saveRequestLogsLocked | ✓ tmp+rename | （唯一正确实现） |
| .cline-credentials.json | auth.go | ✗ | legacy，无调用方 |

全部 0600，gitignored。搜索顺序：exe 目录 → cwd → ~/.cline2api/（仅查找已存在文件；新文件一律创建在 exe 目录旁）。

### 1.5 模块间数据流转与依赖

- **pool/types** 为全局状态核心：`pool *AccountPool` + `poolMu`；其余模块（proxy/zen/admin/models_sync）都经 `loadPool()/savePool()` 读写并各自持有配置互斥量（proxyConfigMu、zenConfigMu、zenStateMu、zenTransportMu、zenProxyCooldownsMu、requestLogsMu、compactStatesMu、oauthSessionsMu、adminSessionsMu、modelSyncMu）。锁顺序经核无环（并发审计「已验证无恙」清单）。
- **配置流向**：admin API →（无锁变更，P0-2）→ proxyConfig/zenConfig → 每请求读取（clineHeaders、routeModel、callZenAPI、maybeCompact）。
- **SSE 三路径**：`handleStreamResponse`（OpenAI 透传，两上游共用）、`handleAnthropicStream`（OpenAI→Anthropic 事件机 + toolAccumulator）、`chatStreamToResponses`（OpenAI→Responses）。
- **模型注册表**：remote（Cline 同步，仅替换 remote 条目）/ zen（同步+seed）/ custom（管理端）/ builtin 兜底；`routeModel` 中池内同 ID 优先于 zen。

---

## 2. 发现清单（跨代理去重后 49 项）

### P0 — 致命（4 项）

> **修复状态（2026-09-01）**：四项 P0 已全部修复于分支 `fix/p0-audit-2026-09`，各含回归测试（p0_fixes_test.go），`go test -race ./...` 通过。以下正文保留审计时原貌。

**P0-1 ✅ [已修复] `savePool` 无锁 Marshal 全局池 + 非原子覆盖写 → 进程死亡 + 账号文件被清空**
- 位置：pool.go:89-94；调用点 ~25 处（池内 5 处 / 池外 20 处：proxy.go:737,752,758,763,781,789、proxy.go:1045（解锁后调用）、pool.go:101,136,146、admin.go:147,733,907,939,1022,1044,1148,1205、models_sync.go:189、zen.go:742）
- 问题：`json.MarshalIndent(pool)` 不持 poolMu；另一协程在 poolMu 内写 `acc.ModelStats`/`ModelCooldowns`（proxy.go:1023-1043、proxy.go:1090）→ `fatal error: concurrent map iteration and map write`（**不可 recover，整进程死亡**，net/http 的 recover 也无法拦截）。两个并发无锁 savePool 还会对同一文件并发 WriteFile → JSON 撕裂 → `loadPool`（pool.go:71-74）解析失败静默返回空池 → 下一次 savePool 把空池写盘 → **全部账号/密钥/模型永久丢失**。
- 触发：普通并发流量即可（无需恶意输入）。
- 修复：savePool 自取 poolMu（或锁内深拷贝快照后再 Marshal）；改 tmp+rename（参照 request_logs.go:97-101）；loadPool 失败时保留坏文件为 .bak 并拒绝在其上保存。

**P0-2 ✅ [已修复] 共享配置指针被无锁变更，与每请求 map 遍历并发 → fatal**
- 位置：admin.go:983-1004（`getProxyConfig()` 返回共享指针后直接写 `cfg.Strategy`、`cfg.Headers[k]=v`，未持 proxyConfigMu）；admin.go:1345-1426（zenConfig 同模式，约 20 处字段直写）
- 交叉并发点：clineHeaders 每请求 `range cfg.Headers`（proxy.go:620-623）；callZenAPI/routeModel/maybeCompact 读 zenConfig 字段
- 场景：管理员保存配置的同时任意在途请求正在构建上游头 → 并发 map 写+遍历 → 整个代理进程死亡。附带：Host 校验失败时 strategy/headers 已被改（部分生效 + 400 响应）。
- 修复：copy-on-write —— 构造新配置对象、持锁经 setProxyConfig/setZenConfig 原子替换；getter 返回不可变快照。

**P0-3 ✅ [已修复] Anthropic 流式 tool_use 参数被清空丢弃（流式工具调用完全失效）**
- 位置：proxy.go:1863-1865（首个非空 args 片段即触发 emit）+ emitToolBlock proxy.go:1733-1754（无 `input_json_delta` 事件；JSON 解析失败 → `input:{}`）
- 场景：上游分片流式发送 `function.arguments`（OpenAI 流式标准行为）→ 第一个分片 `"{"pa"` 就满足 `id/name/args 非空` → 立即发出 `content_block_start(input:{})` + `content_block_stop`，后续参数片段全部丢弃（`acc.emitted=true`）。`/v1/messages` 流式客户端（Claude Code 等）拿到参数为 `{}` 的 tool_use —— 模型的真实工具调用丢失，客户端执行错误的工具。Anthropic 规范本要求参数经 `input_json_delta` 传递，本实现从不发送。
- 修复：块完成后才发射（finish_reason 到达或新 index/id 出现）；累积参数以 `input_json_delta` 事件发送；start 块 `input` 恒为 `{}`。

**P0-4 ✅ [已修复] 默认部署 = 管理面零鉴权暴露公网（含明文 token 导出）+ /v1 开放中继**
- 位置：admin.go:101-104（`AdminPasswordHash == ""` 直接放行）；admin.go:665-684（export 明文返回所有 refreshToken）；proxy.go:277-281（`len(p.Keys)==0` → /v1/* 无鉴权）；Dockerfile `CMD ["-host","0.0.0.0",...]` + compose 公网端口映射
- 场景：全新 `docker compose up` 后，任何互联网访客可导出全部账号 token、改配置、删账号、借池烧配额；警告只打印在容器 stdout（proxy.go:512-517）无人能见。
- 修复：非回环监听且未设密码时拒绝服务（fail-closed）；compose 默认 `127.0.0.1:3457:3457`；首启打印一次性设置令牌。部署手册已强制"首启即设密码"，但代码层应 fail-closed。

### P1 — 高危（13 项）

> **修复状态（2026-09-01）**：13 项 P1 已全部修复于分支 `fix/p1-audit-2026-09`（P1-7 由 P0-1 闭环：坏文件隔离 + 原子写；P1-2 的 zen 传输为自定义 http2.Transport，标准超时字段不适用，挂起防护由 P1-4 的 context 传播 + Retry-After 钳制落实）。回归测试见 `p1_fixes_test.go`，`go test -race ./...` 通过。以下正文保留审计时原貌。

**P1-1 ✅ [已修复] 「刷新全部」持 poolMu 跨 N 次无超时网络调用 → 全局永久停摆**
admin.go:713-720：`poolMu.Lock()` 后循环 `refreshAccountToken`（→ http.go:25-27 无 Timeout 的 client）。auth 端点半开连接一次 → poolMu 永久持有，所有 loadPool/pickAccount/admin 请求全部阻塞，且无超时无看门狗。修复：锁内快照、锁外刷新；client 加超时。

**P1-2 ✅ [已修复] zen 客户端无超时 + 信号量跨重试/退避持有 + Retry-After 无上限 → zen 自死锁**
zen_proxy.go:32（无 Timeout）、zen.go:518-519（`sem <-` 一次获取，defer 循环结束才释放）、zen.go:576-585（`wait = max(delay, parseRetryAfter)`，zen.go:380-396 接受任意秒数/HTTP 日期，无钳制）。8 个请求命中黑洞连接或大 Retry-After（retries 最大 10）→ `zenSem` 永久占满，全部 zen 流量与 `maybeCompact→generateSummary`（compact.go:236）无限阻塞。修复：client/context 超时、钳制 wait（如 ≤30s）、睡眠期释放信号量。

**P1-3 ✅ [已修复] 全链路零超时（slowloris + 永久挂起）**
`http.Server` 无 Read/Write/Idle/HeaderTimeout（proxy.go:139,495-498）；httpClient 无 Timeout、Transport 无 TLSHandshake/ResponseHeader 超时（http.go:17-27）；zen 客户端同。上游收下 TCP+TLS 后不回包 → handler goroutine 永久卡死；慢速 dribble 头部 → FD 耗尽。修复：`ReadHeaderTimeout:10s, IdleTimeout:120s`（SSE 路径 WriteTimeout 留 0 用每请求计时）；transport 三超时；刷新/鉴权调用专用 30s client。

**P1-4 ✅ [已修复] 客户端断连不取消上游 + SSE 写错误全忽略**
三条 SSE 路径的 `w.Write`/`Flush` 错误全部丢弃（proxy.go:1140-1192、1760-1906、responses.go:303-338）；上游请求不带 `r.Context()`（全仓库 `.Context()` 仅见于测试与 restartListener）。客户端 Ctrl-C 后协程继续消费完整上游生成（分钟级、烧 token、占 zen 信号量）；客户端停滞读取 + 无 WriteTimeout → 永久泄漏。修复：`http.NewRequestWithContext(r.Context(), …)` + 写失败即中止。

**P1-5 ✅ [已修复] 热路径账号字段无锁写**
proxy.go:735-737、750-758、762-764、779-782、787-789（`acc.Status/CooldownUntil/LastUsed/UsageCount` 无 poolMu）；admin.go:765；与 poolMu 内的遍历（pool.go:178-180/228-230）并发 → 数据竞争（race-detector UB，理论撕裂）。修复：小粒度锁内 helper（markAccountCooldown 等）。

**P1-6 ✅ [已修复] `{"choices":[]}` → 未检查断言 panic**
proxy.go:1472 `getNested(openAI,"choices",0).(map[string]any)`；1465 只判 `choices == nil`，空数组穿透（normalizeOpenAIResponse proxy.go:1951-1953 原样放行非 map 元素）。上游 200 + 空 choices（过载后端现实可见）→ panic，net/http 逐连接 recover 兜底但客户端只见连接断开、无 JSON 错误、请求日志未落。两条非流式分支（proxy.go:1616、1684）同受影响。修复：comma-ok 断言 + 空回退。

**P1-7 ✅ [已修复·经 P0-1 闭环] 池文件损坏 → 静默空池 → 下次保存永久销毁**
pool.go:71-74：Unmarshal 失败 → 静默空池（无备份/无告警/无隔离）。断电/崩溃（P0-1 的非原子写放大此概率）→ 重启即"零账号"，且任意一次 savePool 用空池覆盖坏文件。修复：坏文件改名 .bak、尝试修复、拒绝覆盖解析失败的文件、管理端告警。

**P1-8 ✅ [已修复] parseCooldownUntil 把分钟当小时 + Duration 溢出**
proxy.go:805-821：`"Try again in 45m"` → 组1=45(小时) → **冷却 45 小时**（「代理实证」；"0h 0m" 正确回退 1h）；`999999999999h` → Duration 溢出回绕，可能得到负值（冷却瞬间失效）或任意大值。且冷却持久化到 ModelCooldowns，重启不消失。修复：h/m 分组显式化 + 时长钳制（如 [1m, 24h]）。

**P1-9 ✅ [已修复] restartListener 先关旧监听再绑新地址 → 绑定失败 = 全代理下线**
proxy.go:137-159：先换 `currentServer` → Shutdown 旧（2s）→ `ListenAndServe`；端口被占（重启路径不调 freePort）→ 零监听。两次快速改配置互相 Shutdown 对方的新 server → 双失败。`listenHost/listenPort` 无锁写（134-135）。修复：先 `net.Listen` 成功再切流；地址变量纳入 serverMu。

**P1-10 ✅ [已修复] delete-all 连带清空 API Keys / 模型表 / 默认模型 / 管理密码**
admin.go:730-733：重置为仅 `Accounts/Keys` 两字段的空池 —— Models、DefaultModel、AdminPasswordHash/Salt、ListenHost 全部清零。i18n 文案只说"删除账号"。所有客户端立刻 401，模型列表依赖 10 分钟 zen 刷新回填（Cline remote 模型不自动恢复）。修复：仅重置 Accounts/CurrentIdx，保留其余字段。

**P1-11 ✅ [已修复] freeModelChain 任何非 429 错误中断整链**
proxy.go:694-697：仅 429 与账号不可用继续走链；链首模型（硬编码常量，proxy.go:26-28）被上游下线/改名 → 400/404 → 整链放弃、客户端拿原始 5xx。三处硬编码模型 ID（proxy.go:26-28、39-46、zen.go:47-56）可漂移失联。修复：4xx（除 401/403 账号级）应推进下一链模型；链由 `getAllModels()` cost=free 动态派生。

**P1-12 ✅ [已修复] max_tokens / usage 数值溢出与静默改写**
proxy.go:569-573：`int(1e19)` → **-9223372036854775808** 直传上游；负数透传（Anthropic 路径 proxy.go:1566 仅修 ==0）；字符串静默变 128000；responses.go:28-30 同模式。proxy.go:934-935 usage 转换：`value >= 0` 防不住 `int64(1e30)` 溢出 → 负 token 数永久入库污染统计。修复：转换前范围钳制（max_tokens ≥1，usage ≤1e7）。

**P1-13 ✅ [已修复] Anthropic→OpenAI：并行 tool_result 只保留最后一个 + tool_choice 原样透传 + stop_sequences 丢弃**
proxy.go:1396/1425-1444：单 `toolResult *map[string]any` 每块覆盖 —— 用户消息含两个 tool_result（并行工具调用标准形态）时上游只见一个 → 400 或上下文错乱；该分支的 text 块同样被丢。proxy.go:1373-1375 `tool_choice` Anthropic 形状直接透传给 OpenAI 上游（`{"type":"any"}` ≠ `"required"`）；`stop_sequences`（proxy.go:1282）解析后从未映射为 `stop`；`temperature:0` 与未设置不可区分（proxy.go:1360-1365）被丢。修复：收集 `[]toolResult` 逐个追加；映射 tool_choice 形状；`stop_sequences→stop`；显式零值透传。

### P2 — 中危（16 项，精简）

**P2-1 ✅ 内嵌前端 XSS：`esc()` 不转义引号 + 三处属性/JS 串 sink**
admin_html.go:1191（textContent+innerHTML 序列化只转 `&<>`）；sink：2116（`title="' + esc(l.error)"`，error 含上游 4xx/5xx 原始响应体，proxy.go:784 → request_logs.go:191）、1805（`onclick="deleteModel(\'' + esc(m.id) + '\')"`）、2036（`option value="..."`）。模型 ID 全程无字符白名单。管理面板可导出全部 token，存储 XSS = 完全接管。「假设」子项：上游错误体是否回显含 `"` 的客户端可控串（决定远程可触发性）。修复：esc 补 `"`/`'` 转义；日志错误单元格用 textContent；onclick 改事件委托。

**P2-2 ✅ 未设密码时管理 API 无任何 CSRF 防线**
admin.go 全部 30 端点无 Origin/Referer/Content-Type 校验（grep 零命中）；无密码时无 cookie 也可直接调。受害者浏览器以 no-cors POST 即可改密锁死/删号/批导。修复：无密码时限回环；有密码时校验 Origin 同源或加 CSRF token。

**P2-3 ✅ API key 由时间戳生成（可在线爆破）**
admin.go:902 `cline_%x_%x`（毫秒时间戳 + 毫秒内纳秒 <1e6）+ `==` 非常数时间比较（proxy.go:292-297）+ /v1 无限速。修复：crypto/rand 32B + subtle.ConstantTimeCompare。

**P2-4 ✅ 无 TLS、cookie 无 Secure**
admin.go:205-212（HttpOnly/Lax/Path 有，Secure 无，grep 零命中）；绑定 0.0.0.0 时密码/会话/export 全明文。修复：反代为强制前提（手册已有），代码侧 Secure 可配 + 文档声明。

**P2-5 ✅ capture 模式 0644 落盘 + token 上屏**
capture.go:166-182（step-*.json 与 full-capture.json 0644，含 Authorization 头与全部 OAuth 响应体）；capture.go:381-399 打印 access/refresh token 到控制台。修复：0600 + 脱敏 + 显式警告。

**P2-6 ✅ `expired` 是终态且瞬时网络故障即可触发**
pool.go:134-137（refreshAccountToken 任何错误——包括网络抖动——都置 expired）+ proxy.go:833-836（恢复协程 `Status != "cooldown"` 跳过 expired）→ 一次 auth 端点抖动即可让全部在用账号永久失效，只能手工 reset。修复：区分"鉴权拒绝"与"网络错误"；前者才 expired，可选低频探活 expired。

**P2-7 ✅ oauthSessions 读竞争 + 永不清理 + 轮询可永久挂起**
admin.go:482-500（查表持锁、读 state 字段不持锁，与 410-462 轮询协程的持锁写并发）；map 无 TTL（admin.go:20）；pollWorkosToken 用无超时 client。修复：锁内快照读取；>N 分钟驱逐；带 deadline context。

**P2-8 ✅ 全部请求体/上游错误体无界读取（无 MaxBytesReader）**
grep 全仓库零 MaxBytesReader；proxy.go:347/1543、responses.go:464、admin.go ~13 处 `io.ReadAll(r.Body)`；proxy.go:769 cline 错误体无上限读入再截 500（zen 侧有 readAllLimited 64KiB，proxy 侧没有）。10GB POST（无 keys 时无需鉴权）→ OOM。修复：MaxBytesReader(~32MB) + 错误体 LimitReader。

**P2-9 ✅ 上游状态码坍缩：400/401/403/429/5xx → 500；zen 恒 502；上游响应头（Retry-After 等）从不透传**
proxy.go:657-662 + 三个 handler 调用点。客户端 SDK 把上游 400 当可重试 500 反复重试注定失败的请求；429 永远到不了客户端 → 退避失效。修复：按 clineAPIError.statusCode 透传 ≥400，429 附带 Retry-After，仅传输层错误给 500。

**P2-10 ✅ Anthropic 非流式：有 tool_calls 时文本被整体清空**
proxy.go:1617-1620、1686-1689 `content = []any{}` 覆盖 openAIToAnthropic 刻意保留的文本块（1488-1493），且无视上游真实 stop_reason 强制 `tool_use`。"先说一句话再调工具" 的输出文本全丢。修复：删掉覆盖，信任转换器。

**P2-11 ✅ 流式 block index 冲突 + 残余工具乱序**
proxy.go:1728-1729（textIndex 0 起）与 1843-1846（tool 直接沿用上游 tool_calls[].index=0）→ 文本+工具流出现两个 index 0，违反 Anthropic 流契约；1889-1893 map 迭代顺序不定。修复：工具块用独立递增计数器。

**P2-12 ✅ Responses 流：上游 error 事件被忽略，失败照报成功**
responses.go:303-338 无 `obj["error"]` 检查（对照 handleAnthropicStream 1791-1796 有）；终态仍发 `response.completed(status=completed)` + 日志 completed=true（379）；Anthropic 流错误后仍补发 message_delta/message_stop 且日志记成功（1895-1906）。修复：错误终态事件 + 日志 failed。

**P2-13 ✅ Anthropic 流丢弃最后一个无换行 SSE 行**
proxy.go:1761-1763 `if err != nil { break }` 在处理 line 之前 —— 截断在记录边界的最后一个 `data:`（常含 usage）被丢；另两条路径顺序正确。修复：先处理后判 err。

**P2-14 ✅ zen/proxy 配置非原子写 + 损坏静默重置**
zen.go:295-296、admin.go:867-872 直接 WriteFile（对照 request_logs 的 tmp+rename）；崩溃写一半 → 重启静默回默认（zen key→public、代理池清空——用户失去限流规避能力而不自知）。修复：统一 tmp+rename + 坏文件 .bad 隔离 + UI 告警。

**P2-15 ✅ 数据目录不可写时静默丢全部持久化**
savePool/zen/config 写失败只 log（pool.go:91-93 等）；resolveDataPath 优先 exe 目录 —— 装在 Program Files 或只读容器层时**每次保存都失败**：UI 生成的 key、改的密码（鉴权静默回退无鉴权）、冷却/统计全部丢失。修复：启动时探测可写并回退 ~/.cline2api + UI 持久警告。

**P2-16 ✅ 管理端 Headers 可覆盖 Authorization/Content-Type，空键名直写上游**
proxy.go:620-624 先设 Authorization 再 range cfg.Headers 覆盖；admin.go:998-1003 不校验键名（`""` 键 → Go 构造畸形请求；已实证无 CRLF 注入——net/http 发送时拒绝——但坏值自我 DoS）。修复：黑名单 Authorization/Content-Type/Host + 键名白名单。

**P2-17 ✅ freePort 启动时强杀端口占用进程**
proxy.go:2007-2019：127.0.0.1 拨通即 PowerShell `Stop-Process -Force` 端口属主（可能是另一个存有未落盘状态的旧实例，或无关应用）；Linux 无 powershell 静默跳过；只探测回环。修复：拒绝启动并清晰报错，或 SO_REUSEADDR 重试。

**P2-18 ✅ 模型注册表：重复 ID + seed 模型窗口期不可见 + DefaultModel 失效不清除**
remote/zen 同步只替换各自 Source 桶、从不跨源去重（models_sync.go:162-189、zen.go:716-742）→ /v1/models 可返回重复 id（isFreeModelID 取首个匹配计费，统计可错记）；zen 同步完成前 seed 模型可路由却不在列表；同步删除模型后 p.DefaultModel 不清理（仅 custom 删除路径清）→ 默认模型请求逐次 400。修复：getAllModels 按 ID 去重；同步后校验 DefaultModel 存在性。

### P3 — 低危（14 项，一行式）

1. ✅ 每个 pick/finish 全量重写池文件（pool.go:221/251 + 每请求 789/1045）——写放大 + pick 持锁磁盘 IO（修复：脏标记 + 1s 合并）
2. ✅ round-robin CurrentIdx 在两个不同过滤列表间共享索引（pool.go:215-219 vs 245-249），冷却态变化时跳号
3. ✅ activeCount 启动快照陈旧（proxy.go:228-241；/health 与 337/428 守卫用旧值）
4. ✅ 登录防爆破仅 `time.Sleep(500ms)`（admin.go:196-199），并发连接不受限、无锁定
5. ✅ GET logout 无方法校验（admin.go:217-222）；GET export/open-external 在 Lax 下可被顶层导航触发
6. ✅ 密码比较 `==`（admin.go:161）与 API key `==` 非常数时间
7. ✅ 无任何安全头（无 CSP/nosniff/X-Frame-Options，admin.go:254-262）→ 放大 XSS、可被 iframe 点击劫持
8. ✅ 密码哈希单轮 SHA-256(salt+pwd)（admin.go:126-129）无 KDF；改密无需旧密码、空串即清除（admin.go:227-252）
9. ✅ randomHex 失败返回 ""（admin.go:167-168）→ 空会话令牌可登录（RNG 失败概率极低但属完全绕过）
10. ✅ 上游错误体回显给 /v1 客户端并入库 30 天（proxy.go:445-448、784；request_logs.go:191）
11. ✅ /health 与 /v1/health 暴露版本 + 账号数（proxy.go:256-270），无鉴权可指纹
12. ✅ 日志注入（model 串原样入日志 proxy.go:371）+ 部分路径 email 不脱敏（admin.go:333 等，truncateEmail 仅热路径使用）
13. ✅ randHex/rand.Read 错误忽略（zen.go:439-443）；limit 用 Sscanf 宽松解析（admin.go:1264-1271，"50abc"→50）；account-test 空体即全量实测每账号（admin.go:790）
14. ✅ override.md 每请求读盘 + 相对路径（与其他文件不一致）+ 无大小上限（proxy.go:1289-1302）；truncate 按字节切 UTF-8 产生 U+FFFD（http.go:59-64 等）；request log 全量重写 O(n)/请求（request_logs.go:104-113）；oauthSessions/已删模型的冷却项不清理

**「假设」级（不确证，附验证方法）**：ExpiresAt 毫秒单位假设（auth.go:257-276，若上游改秒则令牌永过期——用真实响应核实）；XSS 远程可触发性（需上游错误体回显 `"`）；Responses `output:[]` 对客户端的实际影响面。

---

## 3. 已验证无恙（负发现，同等重要）

- **resp.Body 关闭全覆盖**：401 重试先关、非 200 读后关、zen/错误路径均闭（proxy.go:742/756/769-770、zen.go:563-564 等）
- **401 重试 body 复放正确**（proxy.go:746-747，有专项测试）
- **freeModelChain 必然终止**：每种失败都把账号移出资格集
- **SSE 不用 bufio.Scanner** → 无 64KB 行截断问题（代价是 P2-8 的无上限增长）
- **SSE 响应头先于 WriteHeader**；`[DONE]` 帧正确；**无「部分流写后再 writeJSON」路径**
- **请求日志只存元数据**（无请求/响应体；Error 截 200；5000 条/30 天；tmp+rename）
- **锁顺序无环**；adminSessions/requestLogs/zen 代理冷却表的锁纪律正确；zen failover 状态机内部一致、必然自恢复
- **listAccounts 返回前剥离全部 token 字段**；i18n 47 键与调用点 1:1 完整、无注入路径
- **管理端点鉴权覆盖完整**（仅 login/logout 豁免）；会话 256-bit crypto/rand；无路径穿越；openBrowser/freePort 无命令注入；日志无 token 泄漏
- **CORS `*` 不带 credentials**（浏览器不跨域带 cookie）；有密码时 Lax+JSON POST 挡住经典 CSRF
- **Anthropic 非流式/Responses 非流式的信封解包与工具往返**有测试覆盖且正确

## 4. 测试覆盖缺口（46 个现有用例未覆盖）

`handleAnthropicStream`（P0-3 所在，0 测试）· `chatStreamToResponses`（0）· `parseCooldownUntil`（0，P1-8 秒杀）· `restartListener`（0）· 管理端鉴权/会话 TTL/密码失效（0）· 全部 admin 输入解析端点（0）· loadPool/savePool 损坏容忍与原子性（0）· recordTokenUsage 聚合（0）· max_tokens 强制转换路径（0）

## 5. 修复优先级路线（供决策，不含实现）

1. **第一批（进程稳定 + 数据安全）**：P0-1 savePool 加锁+tmp/rename+坏文件隔离、P0-2 配置 COW、P1-7/P2-14 同类、P1-1 刷新锁外移
2. **第二批（超时与取消）**：P1-2/P1-3/P1-4 一组（server/transport 超时、context 传播、Retry-After 钳制）
3. **第三批（客户端正确性）**：P0-3 流式工具调用、P2-10/11/12/13、P1-13、P2-9
4. **第四批（安全加固）**：P0-4 fail-closed、P2-1 esc()、P2-3 key 生成、P2-1/CSRF、P3 安全项
5. **第五批（数据与边界）**：P1-10/11/12/13、P2-6/15/16/18

---

*报告完 · 所有 P0/P1/P2 均经主审计逐条复核原始代码；两条「假设」已显式标注。静态检查：go vet 零告警；go test -race 通过（覆盖局限已在第 0 节声明）。*