# Spec — 停止自对抗：跨太平洋链路的残余抖动

日期：2026-10-14　状态：**待批准**
取证来源：`~/.mole/mole.log.20261014-134537`（07:50–13:45）、`-142025`、`-142752`、`-144446`、当前 `~/.mole/mole.log`（14:44 起）、`~/.mole/state.json`、`~/.mole/sing-box-config.json`

## 目标与约束

用户目标：让 mole 把跨境链路的不稳定"掩盖"掉，本地体验不感到波动。
用户约束（已两次明确）：**从深层次解决、方式简洁、利用协议自身能力、不把算法复杂化**；否决了"加冷却期/更严格倍率"这类补丁层。

## 根因（按影响排序，全部有日志证据）

### F1 — 已完成，保持不动
**比较门的开闸判据曾把"慢"当成"坏"。**

- 证据：`09:06:58` 四笔 **成功** 的拨测（1.2s / 2.4s / 2.3s / 1.2s）→ `09:07:11 fallback: 4/20 new connections slow or failed on proxy-reality … moving to the hy2 ladder`。同形重复于 `09:17:26`、`10:35:27`、`11:49:54`、`13:37:40`（全部 `4/20`，全部由成功但慢的样本开闸）。每次切换重置 Brutal 爬坡并换掉传输层。
- 修复已在**正在运行的二进制**中（`/usr/local/bin/mole` 与仓库 `mole` 的 sha256 同为 `7b062eacb9e96fb55aeace11be13f013db559797ee34ea9dd9592454c5b7e797`）：`freshFlowsFailing` = `State.DialFails() >= DialBadEnter`，见 [standby.go](file:///Users/leo/Projects/mole/core/standby.go#L146-L148)、[supervisor.go](file:///Users/leo/Projects/mole/core/supervisor.go#L1099)、[brutal.go](file:///Users/leo/Projects/mole/core/brutal.go#L198-L204)。

### F2 — 无需改动
**比较门的量具已经是对的。**

[probeLeg](file:///Users/leo/Projects/mole/core/legmeasure.go#L85-L103) 对每条腿取"节点本地 keeper 往返"与"经同一条腿真实外拨 cloudflare.com"两者的**较差值**；后者由 [LegEgressProbe](file:///Users/leo/Projects/mole/core/egress_probe.go#L39) 提供，由 [cmd/up.go L336](file:///Users/leo/Projects/mole/cmd/up.go#L336) 的 `verifyLegEgress` 打开。因此 A/B 与触发信号同源、同尺度。

### F3 — 待修：stall kick 是第三个引擎，今日 8 次全部无效

- 证据（`11:51:01` 的 kick 之后）：keepalive 依次失败 `×1(11:52:16) ×1(11:53:31) ×2(11:53:46) ×1(11:55:16) ×2(11:55:57) ×1(11:56:31) ×2(11:57:12)` → 再 kick → `×1(11:58:35) ×2(11:58:46)` → 再 kick → `11:59:33 fallback: released`。**四次 kick 之间 keepalive 从未恢复**。
- 每次 kick 把 `proxy`（auto 模式下承载 **100%** 流量）移到 `proxy-bw-300` 45s，带宽调度器再拉回（今日 `bandwidth profile → fallback-hy2/pinned …` 5 次）。
- 仓库自己在 dual 模式已整体禁用并写明理由——[balance_controller.go L155-L167](file:///Users/leo/Projects/mole/core/balance_controller.go#L155-L167)；auto 模式比 dual 更严重（100% vs 25% 流量）。
- 它要处理的"卡住的五次组"：hysteria 自身会话超时 + sing-box 在请求失败时重拨已经覆盖。

### F4 — 待修：本地链路闸门误报

- 今日 9 次 `LOCAL LINK BAD`，**全部**是 `lost 3/12 echoes, median 3ms`（`09:43:20`、`09:48:26`、`10:08:35`、`10:23:35`、`11:19:45`、`11:35:45`、`11:48:20`、`12:03:20`）。
- 网关 3ms 说明链路能载流；丢的是路由器对 ICMP 的降级处理（2.4GHz 繁忙信道）。[updateLinkLatch](file:///Users/leo/Projects/mole/utils/link.go#L94-L109) 目前是 `loss>=3 || med>=200` 的 **OR**，丢失臂没有任何 RTT 佐证。
- 代价：每次误报冻结全部腿/带宽决策 40–60s，暂停新拨采样（[supervisor.go L1068](file:///Users/leo/Projects/mole/core/supervisor.go#L1068)），并阻断 `tryReturnToPrimary`（[L1489](file:///Users/leo/Projects/mole/core/supervisor.go#L1489)）——即让流量多留在兜底腿上最多 60s。

### F5 — 不在 mole 逻辑内（残余）：新建连接被路径丢弃

- 14:44 运行的日志里有两条 `dial tcp 158.247.246.41:443: i/o timeout`（`14:45:40` chatgpt.com、`14:50:43` api.binance.com），而 `state.json` 报告健康：`consecutive_ok: 61`、拨测 455–702ms、网关 3ms、`last_probe_verdict: alive`。
- 即：**已建立连接正常、新建连接间歇被丢** —— VPS POP 上 `:443` 的 SYN 丢失（运营商 QoS 特征，非 mole 可控）。
- 另一条线索：REALITY 出站**无条件**开启 `tcp_fast_open`（[parser.go L578](file:///Users/leo/Projects/mole/config/parser.go#L578)），而"带数据的 SYN"正是这类"新建失败/已建立正常"模式里中间盒常见的丢弃目标。此项**需实测**，不做猜测。

**实测结论（2026-10-14，收尾）**：该 POP 的 `:443` 丢包随 QoS 窗口显著放大（同一 `icn-1791933782` POP：15:56–16:10 的 14 分钟里 65 次；关掉 TFO 后 16:13–16:43 的 30 分钟里 130 次，速率不变 → **TFO 与丢包无关**）。换到新 POP `icn-1791968917`（`158.247.217.185`）后，17:11–17:36 的 25 分钟里 `i/o timeout` 仅 **3 次**（且集中在后半段、均为 chatgpt.com），新建连接速率从 ~4.5 次/分降到 ~0.12 次/分 —— **根因确认为 POP 侧切片，唯一有效手段是重抽 POP**。

## 决策

| 编号 | 决策 | 依据 |
| --- | --- | --- |
| D1 | 保持 F1 的修复不动 | F1 |
| D2 | **删除 stall kick**（含 auto 与 dual 两条调用路径、常量、字段、调度器分支、死代码） | F3 |
| D3 | 本地链路闸门的**丢失臂需 RTT 臂佐证**：进入条件改为 `med >= LinkBadMedianMs \|\| (loss >= LinkBadLoss && med >= LinkOKMedianMs)`；离开条件不变 | F4 |
| D4 | 对 `tcp_fast_open` 做 **30 分钟 A/B 实测**，以数据定去留。**已实测（2026-10-14）：TFO 证伪，保留 `true`** —— 关掉 TFO 后 QoS 窗口内 `i/o timeout` 速率不变（130 次/30min ≈ 4.3 次/分 vs 开 TFO 时 65 次/14min ≈ 4.6 次/分），普通 SYN 同样被丢，`config/parser.go` 已回退并附实测注释 | F5 |
| D5 | 门日志改为报告**真正开闸的证据数**（`DialFails`），文案由 "slow or failed" 改为 "failed" | 可追溯性 |

## 非目标

- 不新增滞后/冷却/倍率层（用户已明确否决）。
- 不改 dual routing 的车道分配逻辑。
- 不实现自动换 POP（那属运维动作，作为 D4 之后的兜底手段）。

## 验收标准

1. `go build ./...`、`go test ./...` 全绿；`make build` 成功。
2. 新二进制运行 30 分钟后，在同等条件下：
   - `grep -c 'stall kick' ~/.mole/mole.log` == 0（D2）。
   - `grep -c 'LOCAL LINK BAD' ~/.mole/mole.log` 显著下降，且不再由"3/12 丢包 + 3ms 中位数"触发（D3）。
   - `grep -c 'fallback:' ~/.mole/mole.log` 仍为 0 或极少（D1 保持）。
   - 门日志中的计数与 `state.json` 的 `dial_samples` 失败数一致（D5）。
3. D4 的 A/B：两版之间可比 `grep -cE 'dial tcp .*i/o timeout'`、`grep -cE 'fresh dial (slow|failed)'`、`consecutive_ok`。
4. `mole status` 中 `consecutive_ok` 持续增长、`last_probe_verdict=alive`。

## 需要的批准

**D2 是本次唯一的删除型改动（移除一个引擎），改动面较大（跨 core/ 三文件与测试），请明确批准或否决。**
D3 为阈值口径修正；D4 为纯实测、不改产品代码；D5 为日志口径。批准后按 [tasks.md](file:///Users/leo/Projects/mole/tasks.md) 顺序执行。
