# Mole 性能审计与优化验证方案 — 2026-11-05

## 结论

有进一步优化空间，优先级是：补齐部署与实际运行配置的闭环；验证队列和 Brutal 速率的组合；补齐服务器 TCP Fast Open；改善 DNS 启停就绪与失败恢复。当前证据不支持直接换协议、打开双路、加大所有缓冲区或升级 VPS。

这是源码、上游固定版本和当前节点的只读审计，以及有限流量的实测。未修改生产配置、未重启生产服务、未部署候选优化。因此以下候选方案的速度增益均未验证，不承诺提升百分比。

## 范围与证据

- 项目基线：`fb2592e9806bc81959f0f81b2ca1e24e97a2d06f`。工作区另有 `cmd/server.go`、`cmd/lid_phantom_debt_test.go` 等用户改动，未纳入本次成果；安装二进制曾含额外修复，不宣称与干净 HEAD 完全一致。
- 当前节点：Vultr icn，`vc2-1c-1gb`，1 vCPU / 1 GB，Ubuntu 24.04，内核 `6.8.0-147-generic`；本报告不公开用户出口地址、节点认证材料或云密钥。
- 运行版本：sing-box `1.14.4`，Hysteria `2.13.0`；上游追踪固定版本，不以最新网站文档替代安装版源码。
- 现状：REALITY + Vision + smux 主用（8/1），TCP Brutal 600 Mbps 组速率；HY2 400 Mbps 备用。TUN gVisor / MTU 9000；双路未启用。
- 本地无线：5 GHz / 80 MHz / 802.11ax，RSSI -59 dBm，噪声 -95 dBm；某时刻 PHY 408 Mbps。PHY 是协商速率快照，不是有效吞吐上限。
- 私有原始证据：`~/.mole/diagnostics/performance-audit-20261105/`。包括 node-runtime.json、throughput.json、load-confirmation.json、node-load.py.stdout、load-analysis.json、node-egress.py.stdout、vps-metadata.json、deployment-template-sanitized.json、采集脚本和 upstream/。
- `node-runtime.json` SHA-256：`fdc93977f8fd307cf601d261f3c69e50474e6f9ef020eac653aa60d097b795a7`。
- 云端启动模板只在内存解码，保存白名单性能字段。SSH 通过独立临时 sing-box 和既有 SSH 钥匙串密钥到达节点；临时代理退出后删除配置。原始证据目录只对本用户开放。

## 本轮测量

通过生产引擎的固定转发端口，读取 VPS 自有 loopback keeper 的确定长度 payload，按实际收齐字节 / 墙钟时间计算。每轮 REALITY/HY2 顺序交替；每路先 4 MiB 预热，再 16 MiB 单连接和 32 MiB 八连接。双路为 4+4，共八连接。共三轮、408 MiB；另做每阶段 64 MiB、共 192 MiB 的八连接复测。总 payload 600 MiB，另有协议与探测开销。

| 模式 | 第一轮 Mbps | 第二轮 Mbps | 第三轮 Mbps | 三轮中位数 Mbps | 64 MiB 复测 Mbps |
|---|---:|---:|---:|---:|---:|
| REALITY 单连接 | 186.4 | 161.7 | 188.8 | 186.4 | 未测 |
| REALITY 八连接 | 290.7 | 259.5 | 329.7 | 290.7 | 328.9 |
| HY2 单连接 | 109.0 | 131.8 | 107.6 | 109.0 | 未测 |
| HY2 八连接 | 202.3 | 201.9 | 189.1 | 201.9 | 227.6 |
| 双路八连接，4+4 | 288.0 | 261.5 | 258.9 | 261.5 | 307.8 |

全部预算收齐，无请求错误，无新增客户端 ENOBUFS。首组同时发起的 40 个 gstatic HTTPS 小请求全部返回 204，TTFB 中位数 103.2 ms、最大 249.9 ms；没有匹配的空载对照，不能据此量化 bufferbloat。探测与负载时间有重叠，但不保证每个探测全程处于满载。

边界：这些是秒级、预热后的节点到客户端短测，不是长时间 ISP 容量、上传能力、完整 TUN 路径吞吐或任意网站速度。大流量走本地固定 forwarder，绕过应用到 TUN 的入站处理。双路测的是等量 4+4，不是生产按源端口分配的 6/2 策略；可否定“直接把两路测速相加”，不能证明所有双路策略都无价值。

30 秒 VPS 采样从 08:16:03 开始，晚于 REALITY 复测主体，覆盖 HY2 和双路复测：

- 250 ms 采样窗口最大 CPU busy 69.2%，最大 steal 3.8%；Hysteria 进程最大约 55.8% 单核。未观察到持续 CPU 打满。未完整覆盖 REALITY 满载，不能外推其 CPU 峰值。
- 快照内存 available 559 MiB，swap 使用 0；服务无 CPU quota / MemoryMax 限制，FD 上限充足。
- 全主机计数增量：UDP SndbufErrors +2，TCP RetransSegs +114，ListenDrops +1。不是按测试连接归因的计数；不可把它们直接当作 HY2 丢包率或 REALITY 重传率。
- 初始网卡 qdisc drop 0，结束累计 653，跨越多轮测试及正常业务；说明测试期间出现过排队丢弃，不能证明由哪条协议、参数或队列算法引起。

VPS 到源站 curl -4/-6，各三次，18/18 完成，DNS 约 1–2 ms：

| 目标 | IPv4 TLS 就绪中位数 ms | IPv6 TLS 就绪中位数 ms | IPv4 TTFB ms | IPv6 TTFB ms |
|---|---:|---:|---:|---:|
| API 入口 | 122.6 | 53.1 | 423.4 | 372.4 |
| Cloudflare | 123.4 | 67.1 | 127.2 | 71.2 |
| gstatic | 158.7 | 128.9 | 191.9 | 188.8 |

这是节点 curl 出口对比，非客户端完整代理请求；API 响应包含未认证状态，未测模型首 token、生成速度或账号可用性。当前样本支持保留节点 prefer_ipv6，不支持一刀切 IPv4。

## 源码与协议发现

### 1. qdisc 配置意图和实际运行不一致（确定事实，提速收益待测）

`cmd/deploy_vultr.go:497–537` 在 HY2 安装片段写入 `net.core.default_qdisc=fq`，使用 `sysctl -p ... || true`。运行读取为 fq，但 `tc -s qdisc show` 显示物理接口 enp1s0 仍是 **fq_codel**。设置默认值不会证明已存在网卡的队列已改变。

此外通用 sysctl 被放在 HY2 片段，独立 REALITY 安装路径不能据此保证获得同一调优。应提取共享的幂等网络准备阶段，记录参数失败和实际设备配置；区分“协议可用”和“调优已生效”。

fq_codel 不是错误算法；存在内核 pacing 不代表只能用 fq。下一步要做同节点 fq_codel/fq 的匹配对照，观察有效 payload、负载延迟、重传、UDP 缓冲错误和 CPU，再决定默认值。不要只为让文字一致就直接替换生产队列。

### 2. TCP Fast Open 只配置了客户端（确定配置缺口）

`config/parser.go:592–607` 设置客户端 `TCPFastOpen=true`；`cmd/deploy_vultr.go:686–710` 的 VLESS 监听器没有 `tcp_fast_open`，实际服务器 JSON 同样没有。Linux `tcp_fastopen=3` 仅启用客户端/服务器能力，没有全监听器自动启用位 0x400。

安装版 sing-box `common/listener/listener_tcp.go` 仅在 listenOptions.TCPFastOpen 为 true 时走 TFO listener。候选改动是在服务器 VLESS inbound 显式启用，并让生成配置测试与运行回读覆盖此字段。

收益限定于新的外层 TCP 会话，取决于 cookie、客户端实现和路径支持；已复用的 smux 流没有这次握手可省。不能把“最多可省握手往返”写成“每次网页请求必省约 100 ms”，也不会因此提高稳态 Mbps。验收要看到 SYN 数据 / FastOpen 成功计数，不能只看配置 true。

### 3. 主连接已经是 Brutal；全局 BBR 不是当前主隧道的开关

VPS `ss -tin` 的实际 :443 连接为 brutal。规则按客户端前缀分组，8 条 smux 外层连接共享 600 Mbps，总量不是 8×600。节点到互联网的普通 TCP 出站仍受默认 BBR 控制，这是不同方向。

`core/supervisor.go:1619–1626` 使用独立 realityRateMbps 覆盖 HY2 的 down；因此 HY2 梯级退避不能当作 REALITY 600 也会自动退避的证据。源码同处还有“REALITY 是 TCP 所以没有 Brutal ceiling”的旧注释，与当前逻辑矛盾，应随相应修复更新。

Brutal 使用给定速率，不自动发现可用带宽。当前有效速度低于 600 不能单独证明应提高或降低它：短测、Wi-Fi、路径竞争、排队和单流均可能影响。先在当前路径做 300/400/500/600 匹配扫描，并同时观测网页延迟；当前不建议直接加到 800/1000。此前另一时间/节点的 600 相对 400 有益结果不能充当今日验收。

### 4. smux 已启用，被动错误感知也已实现

`config/parser.go:608–616` 使用 smux max_connections=8/min_streams=1；上游 mux client 先开新 session，达到上限后共享已有 session。8 是外层池上限，不是最多八个应用请求。

`core/node_dial.go` 和 `core/supervisor.go:786–820` 已用真实应用拨号错误促使更早进行新探测，当前节点错误检查约 15 秒而非正常 60 秒；不会把任意日志直接当成切换依据。不能把这一已交付机制再次列为待实现优化。

若测试 4/8/16 池，应同时测冷启动、热复用、并发、失败恢复；扩大池可能增加握手和资源开销，不能默认更快。当前 8/1 作为基线保留。

### 5. HY2 的 MTU 和缓冲需要实证，不宜继续堆参数

`config/parser.go:725–729` 固定 InitialPacketSize=1200、DisablePathMTU=true。可在隔离客户端验证 PMTUD 开启后的包尺寸、PPS、CPU、上传及下载；客户端发送包设置不等于服务器下行也按同一尺寸发包，需分别观测两端。跨网 MTU 黑洞仍是回退条件。

上游默认 stream window 约 8 MiB、connection window 约 20 MiB。在 105 ms RTT、400 Mbps 下 BDP 约 5.25 MB；目前没有流控窗口已成为瓶颈的证据。Linux rmem/wmem max 已 16 MiB、TCP 自动调优 max 32 MiB；1 GB VPS 上继续放大应有实际 skmem / 错误证据。

部署注释把 udp_rmem_min 当作“首次 UDP socket 初始缓冲”，把其与 QUIC slow start 相连，论据不成立：内核文档描述的是 UDP 内存压力下的最低接收保障，不是 rmem_default，也不是 QUIC 拥塞窗口。需要按安装内核版本复核注释及参数，不以这些注释推导性能结论。

### 6. 节点 IPv6 路径值得保留；旧版 fallback 注释已过时

`cmd/deploy_vultr.go:638–660` 注释称 prefer_ipv6 只尝试第一个地址。固定版 1.14.4 的 direct.NewOutbound → dialer.NewWithOptions → NewResolveDialer 表明：当前 direct 配置无 detour、未开 outbound TFO，parallel=true；resolve.go 使用 DialParallel / DialParallelNetwork。不能沿用旧实验断言当前版本永不 fallback。

需要补充“AAAA 不可达、A 可达”的隔离集成验收，并记录回退时延。当前出口样本中 IPv6 TLS 更快，无证据支持全局禁用 IPv6；Mac 到节点 IPv4 与节点到源站 IPv6 是两段路径。

### 7. DNS 生命周期比进一步堆吞吐参数更影响“点不开”体验

`cmd/up.go:262` 在引擎启动前接管系统 DNS；`core/singbox.go:29–113` 仅通过配置检查及进程存活 2 秒判断启动成功，没有实际 DNS 响应就绪门。

`utils/dns.go:36–114` 已防止旧备份被 TUN 地址覆盖，但忽略逐服务 setDNS 错误；RestoreDNS 忽略恢复错误后仍删除备份。建议补齐备份持久化成功、逐服务操作结果、DNS 实际查询就绪、失败保留备份与可重试恢复。不能简单把 DNS 接管搬到任意位置，必须维持 PF fail-closed 和现有 TUN 启动顺序的约束。

这能减少启动/恢复时的解析故障窗口，属于可用性与等待时间优化，不是大文件 Mbps 提升。此前微信重启后恢复与浏览器内部状态有关，但本审计未证明初始 DNS 超时就是上述生命周期缺口造成的。

## 按收益与风险排序的实施计划

以下为候选工作；本次未执行生产切换。

### 任务 1：让部署回执反映真实网络状态（P0，其他调优的前提）

- 意图：消除“写了参数就算成功”的假设；共享 HY2/REALITY 网络准备阶段，保持一条协议可用时的降级能力。
- 验证：生成脚本和执行失败模拟覆盖独立/双协议、参数不支持、重复执行；解析实际 tc、sysctl、socket CC、版本、协议验收结果，区分 ready 与 tuning-degraded。
- TDD：先构造“sysctl 写入成功但设备仍 fq_codel”的回执测试，再实现状态区分；每个子改动控制在约 200 行以内。
- 回退：仅新增观测先落地；改变队列另行验收。不把可选调优失败扩大为所有协议不可用。

### 任务 2：找到当前路径吞吐与交互延迟的合适点（P1，最直接的下载候选）

- 意图：对照 qdisc 和 Brutal 速率，选有效接收更快且网页不明显变慢的组合。
- 顺序：固定速率先比较 fq_codel/fq；选出候选后再扫 300/400/500/600，避免同时改变两个变量。先在隔离/候选节点验证自动恢复命令，再安排生产窗口。
- 验证：同节点、同 Wi-Fi、相近时段、相同 payload/连接数，至少五对轮换；1/8/16 应用连接，冷/热分开；每次至少 20 秒稳态并设 1 GiB 流量上限，预算先耗尽则标记短样本；补完整 TUN HTTPS 实际下载对照。
- 同步记录：实际 socket CC、qdisc 增量、负载与空载 TTFB、CPU/steal、丢包/重传、UDP 错误、超时。单独验证上传，不以下载外推。
- 接受标准建议：收益超过基线轮间噪声；可预注册吞吐中位数改善至少 10%、负载 p95 TTFB 不恶化超过 10%、无新增失败的门槛。阈值是未来判据，不是本次结果。
- 回退：保存原 qdisc 类型及选项和原速率，设置独立超时恢复；新旧连接都确认 Brutal 分组。测试中任何持续超时或错误增长即停止。

### 任务 3：补齐 TFO 冷连接路径（P1，小改动，有限收益面）

- 意图：让客户端配置真正有服务端配合，缩短可成功使用 TFO 的新外层会话建立时间。
- 验证：先增加生成配置行为测试，再添加 inbound 字段；sing-box check 通过；冷/热 cookie、启用/关闭各做至少 30 次隔离连接，比较成功率与 p50/p95，并观察 SYN 数据/内核 FastOpen 计数。
- 约束：保持 smux8/1；不把热连接复用混入冷连接样本；不全局设置 0x400 代替精确监听器配置。
- 回退：恢复原服务配置，保持 HY2 可用；服务重启会影响连接，属于未来部署阶段。

### 任务 4：减少 DNS 启停与恢复的失败窗口（P1，体感与可靠性）

- 意图：将真实解析响应作为就绪证据，恢复失败可追踪、可重试。
- TDD：模拟备份写失败、某服务 setDNS 失败、恢复部分失败、进程存活但 DNS 无响应、异常退出后的已有备份；先失败再实现。
- 验证：保留未恢复服务的备份；国内真实解析与代理 DNS 路径分别验收；真实启停/睡眠恢复时观察微信与系统解析，但不自动退出用户应用。
- 约束：保持 PF、防泄漏、FakeIP 持久化、用户 DNS 原值和合盖策略；恢复失败不以清空全部系统 DNS 作为掩盖手段。

### 任务 5：有明确瓶颈证据后再试 HY2 / 池大小 / 硬件（P2）

- 意图：用包级/CPU/流控证据选择后续实验，而非一轮同时改十个 sysctl。
- 验证：隔离配置逐项测试 PMTUD、smux4/8/16；如发现 QUIC flow-control blocked 或 socket buffer 错误，再改对应窗口；若重复长测 CPU 接近饱和，再比较高频/更多核心方案。保持相同地区与路径，防止把换节点路由差异当硬件收益。
- 回退：所有实验均独立开关，保持 REALITY 主用与 HY2 fallback；没有稳定增益就保留原值。

代码实施后运行项目要求的 Go 测试、vet 和相关 race 检查，再做独立认证的双协议 HTTPS 验收。当前交付仅报告与测量摘要，不宣称上述代码验证或优化验收已完成。

## 暂不建议

- 直接切换协议或打开双路：本轮 REALITY 领先，4+4 双路未稳定领先；不据此删除 HY2 备用。
- 直接加速率到 800/1000、无限扩大 socket / backlog / MTU：没有瓶颈证据，可能把带宽压力变成排队与重传。
- 立即升级 2 核或换地区：当前 CPU 样本不支持必需升级；路线质量、Wi-Fi 和不同时段仍可能主导。没有进行其他套餐/节点的等条件比较。
- 全局关闭 IPv6：当前源站出口测试反而支持保留。
- 再做一次“启用 smux / 添加被动错误感知”：已在运行；应验证与维护现有实现。

## 上游依据

- [Linux TCP Fast Open 和 UDP 参数定义](https://www.kernel.org/doc/html/latest/networking/ip-sysctl.html)：TFO listener opt-in；UDP min 的语义。最新文档涉及其他参数的差异须按安装内核复核。
- [Linux default_qdisc](https://www.kernel.org/doc/html/latest/admin-guide/sysctl/net.html)：默认队列语义，不能替代设备实际读取。
- [sing-box 监听字段](https://sing-box.sagernet.org/configuration/shared/listen/)；[1.14.4 TCP listener 源码](https://github.com/SagerNet/sing-box/blob/v1.14.4/common/listener/listener_tcp.go)。
- [1.14.4 dialer 构造](https://github.com/SagerNet/sing-box/blob/v1.14.4/common/dialer/dialer.go)；[resolver 拨号](https://github.com/SagerNet/sing-box/blob/v1.14.4/common/dialer/resolve.go)。
- [sing-mux 固定版本池选择](https://github.com/SagerNet/sing-mux/blob/caf09fe32475/client.go)；版本来自 sing-box v1.14.4/go.mod。
- [TCP Brutal 分组、速率与规则生效语义](https://github.com/HyNetworks/tcp-brutal)。
- [Hysteria 性能说明](https://v2.hysteria.network/docs/advanced/Performance/)；[当前 sing-quic 客户端](https://github.com/SagerNet/sing-quic/blob/7e0a5869a5f4/hysteria2/client.go)。
