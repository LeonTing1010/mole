# Tasks — 停止自对抗

依赖与顺序：T1 → T2 → T3 → T5（代码链，一次构建交付）；T4 独立实测，可并行。
批准门：T1 需用户明确批准（见 [spec.md](file:///Users/leo/Projects/mole/spec.md) 的"需要的批准"）。

---

## T1 (P0) 删除 stall kick —— D2　【需批准】

整个"靠换 outbound 强制重拨"的引擎及其全部调用点。

1. `core/standby.go`
   - `keepaliveLegAction`（L226-243）：删除 `if onHy2 && fails == stallKickThreshold { return legKick }` 分支（L230-232）。
   - `legAction` 常量块（L202-212）：删除 `legKick` 常量与其注释。
2. `core/supervisor.go`
   - 删除 `requestStallKick`（L1342-1396）与 `stallKick`（L1400-1407）。
   - `keepaliveOnce` 的 switch（L986-993）：删除 `case legKick: s.requestStallKick()`。
   - `runBandwidthScheduler`：删除 `if rung, kicking := s.stallKick(); kicking { … }` 分支（L1690-1697）。
   - 常量块（L906-915）：删除 `stallKickThreshold`、`stallKickHold`（保留 `healthyFreshDialEvery`）。
   - Supervisor 结构体：删除 `kickMu`、`kickUntil`、`kickRung` 字段及其初始化。
3. `core/balance_controller.go`
   - 删除 L67-69 的 `if rung, kicking := s.stallKick(); kicking { target = BandwidthRungTag(rung) }`。
   - 删除 L163-167 的 dual 死调用；把 L155-162 的注释改写成"kick 已整体移除，原因见 spec F3"。
4. 测试：`grep -rn "stallKick\|legKick\|requestStallKick\|kickMu\|kickUntil\|kickRung" core/ cmd/`，更新或删除涉及用例（已知涉及：`core/standby_test.go`、`core/balance_controller_test.go`，以及可能 `core/leg_failover_test.go`、`core/fallback_recovery_test.go`）。

**验证**：`go build ./...` 无 "declared and not used"/undefined；`go test ./...` 全绿；`grep -rn "stallKick\|legKick" core/` 无残留（注释除外）。

---

## T2 (P1) 本地链路闸门加 RTT 佐证 —— D3

1. `utils/link.go` `updateLinkLatch`（L94-109）：进入判据改为

   ```go
   case med >= LinkBadMedianMs || (loss >= LinkBadLoss && med >= LinkOKMedianMs):
   ```

   离开判据 `loss <= LinkOKLoss && med < LinkOKMedianMs` 保持不变。
2. `utils/link.go` 顶部常量注释（L26-47）：补一句——"丢失臂若没有 RTT 臂佐证，量的是路由器对 ICMP 的降级处理，不是无线电。2026-10-14 有 9 次 `3/12 丢包 + 3ms 中位数` 的误报。"
3. 新增 `utils/link_test.go`（若不存在），覆盖：
   - `3/12 丢包 + 3ms 中位数` → **不** latch（回归 F4）；
   - `3/12 丢包 + 150ms 中位数` → latch；
   - `0 丢包 + 250ms 中位数` → latch；
   - 已 latch 后 `1/12 丢包 + 80ms 中位数` → 恢复。

**验证**：`go test ./utils/...`。

---

## T3 (P1) 门日志报告真实开闸证据 —— D5

1. `core/supervisor.go` `dialWindow`（L1249-1253）：返回 `s.state.DialFails()` 而非 `DialBad()`。
2. `core/supervisor.go` L1201-1202 文案：`"%d/%d new connections slow or failed on %s; …"` → `"%d/%d new connections failed on %s; …"`；L1184/L1186/L1188/L1191 的 "new connections failing (%d/%d)" 保持（措辞已正确）。
3. 复核 [standby.go L136-148](file:///Users/leo/Projects/mole/core/standby.go#L136-L148) 与 [brutal.go L198-204](file:///Users/leo/Projects/mole/core/brutal.go#L198-L204) 的注释与新口径一致（只改措辞，不改语义）。

**验证**：`go test ./core/...`；构造一个"4 次 timeout + 2 次慢成功"的窗口时，日志应报 `4/N`。

---

## T4 (P0) `tcp_fast_open` A/B 实测 —— D4

不改产品代码，用生成好的配置做交叉实测。

1. A 版（现状）：`~/.mole/sing-box-config.json` 中 `proxy-reality` 保持 `"tcp_fast_open": true`，`mole down && mole up`，跑 **30 分钟**。
2. B 版：把该文件的 `"tcp_fast_open"` 置为 `false`（或删掉该键），重启 sing-box/mole，跑 **30 分钟**（同 POP、同时段照度尽量接近）。
3. 每版记录：
   - `grep -cE 'dial tcp .*i/o timeout' ~/.mole/mole.log`
   - `grep -cE 'fresh dial (slow|failed)' ~/.mole/mole.log`
   - `state.json` 的 `consecutive_ok`、`dial_samples` 的中位与最大
4. 判定：B 显著更优 → 把 [config/parser.go L578](file:///Users/leo/Projects/mole/config/parser.go#L578) `o.TCPFastOpen = true` 改为 `false`（并同步 [config/types.go L111-114](file:///Users/leo/Projects/mole/config/types.go#L111-L114) 注释），重新构建交付；无显著差异 → 保留现状，在 spec.md 记录"`tcp_fast_open` 已证伪"。

**验证**：两版的三个计数与 `consecutive_ok` 有可比较的数字；结论写入 spec.md。

---

## T5 (P0) 构建与交付 —— 收口

1. `go build ./...` → `go test ./...` → `make build`
2. 核对 `/Users/leo/Projects/mole/mole` 与 `/usr/local/bin/mole` 的 sha256 一致。
3. 交付命令（由用户执行）：

   ```sh
   cd /Users/leo/Projects/mole && sudo make install && mole down && mole up
   ```

4. 30 分钟后按 [spec.md](file:///Users/leo/Projects/mole/spec.md) 的"验收标准"逐条核对，汇报计数。
