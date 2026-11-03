package core

import (
	"context"
	"log"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// direct 豁免学习：让 PF 的放行表跟上 custom-rules.json 里「判给 direct 的境外域名」
// 实际拨出去的地址。背景与判据见 utils/directexempt.go 顶部。
//
// 为什么是「观察已发生的拨号」而不是「预先解析域名」（2026-09-16 实测定）：
//
//	这些域名是 CDN 任播。同一秒内问 223.5.5.5 要 npmjs.org，拿到的是十二个地址，
//	而 sing-box 给 registry.npmjs.org 拨的 104.16.0.35 一个都不在里面 —— 池子在转，
//	一次解析只是它的一个切片。靠解析预填的表会一直漏，而且漏得没有规律。
//	反过来，被 PF 丢掉的连接照样会出现在 /connections 里（up=0 down=0，一个字节没走），
//	所以「拨了什么」是能直接读到的事实，不需要猜。
//
// 代价是首次拨一个从没见过的地址会被丢一次。ExemptDirectDest 顺手把该主机名当前
// 解析到的整池一起放行，把代价压到「每个主机名一次」而不是「每个地址一次」；
// 学到的地址落盘，下次 `mole up` 直接重放（SeedDirectExemptions），于是长期看是零。
//
// ⚠️ 只观察、只按名字开洞：chains[0] 必须是 direct，且 host 必须命中一条
// custom-rules.json 里写下的 direct 域名规则。没有主机名的裸 IP 直连一律不碰 ——
// 那种连接没有任何东西可以拿来核对，而这张表是 fail-closed 规则集上的洞。
const directExemptInterval = 5 * time.Second

// runDirectExempt 轮询 sing-box 的连接表，把 direct 出站拨往境外的地址补进 PF 豁免表。
func (s *Supervisor) runDirectExempt(ctx context.Context) {
	if s.directDomains == nil {
		return // 没注入回调 = 该功能未启用，安静退出而不是空转
	}
	ticker := time.NewTicker(directExemptInterval)
	defer ticker.Stop()

	// 本进程内已经处理过的 host|ip，避免每 5 秒对同一条长连接重复问 pfctl。
	// 不落盘：落盘的是 utils 那边的地址并集，这里只是省一次 exec。
	seen := map[string]bool{}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// 每拍重读规则，因为 custom-rules.json 是热改的：把一个域名从 direct
			// 里删掉之后，不应该还在给它开洞。（已经开出去的洞由下一次
			// SeedDirectExemptions 收回 —— 那是 `mole reload` 和 `mole up` 的活。）
			set := s.directDomains()
			if set.Empty() {
				continue
			}
			conns, err := s.clash.Connections()
			if err != nil {
				continue // sing-box 正在重启之类，下一拍再说
			}
			for _, d := range directExemptCandidates(conns, set) {
				key := d.Host + "|" + d.IP
				if seen[key] {
					continue
				}
				seen[key] = true
				if err := utils.ExemptDirectDest(d.Host, d.IP); err != nil {
					// 放行失败要留痕：症状是那个域名继续静默不通，而不通的原因
					// 恰恰是我们自己的防火墙 —— 没有这行日志就查不到这里。
					log.Printf("kill switch: %s → %s 未能放行: %v", d.Host, d.IP, err)
					delete(seen, key) // 下一拍重试
				}
			}
		}
	}
}

// directDest 是一条「该考虑放行」的直连目的地。
type directDest struct {
	Host string
	IP   string
}

// directExemptCandidates 从连接表里挑出「按规则该直连、但会被 kill switch 丢掉」的目的地。
//
// 三个条件缺一不可，也是这张表的全部安全边界：
//   - chains[0] == "direct"：真正开 socket 的出站是 direct，包才会走物理网卡；
//     走隧道的连接由 VPS 那条 pass 规则负责，与这张表无关。
//   - DestIP 非空：只有直连才带地址，进隧道的连接带的是域名。
//   - Host 非空且命中一条写下来的 direct 域名规则：没有名字就没有东西可核对。
//     裸 IP 直连一律不碰 —— 它可能正是「路由判错了」本身，而那种时候更不该开洞。
func directExemptCandidates(conns []Connection, set utils.DirectDomainSet) []directDest {
	var out []directDest
	for _, c := range conns {
		if len(c.Chains) == 0 || c.Chains[0] != "direct" {
			continue
		}
		if c.DestIP == "" || c.Host == "" || !set.Match(c.Host) {
			continue
		}
		out = append(out, directDest{Host: c.Host, IP: c.DestIP})
	}
	return out
}
