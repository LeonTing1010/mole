package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LeonTing1010/mole/utils"
)

// 域名路由规则改走 local rule-set，使其可以热改而不重启 sing-box。
//
// 为什么（2026-08-04 立，实测）：
//   sing-box 只在进程启动时读一次 sing-box-config.json，所以改 custom-rules.json
//   里的一个域名，过去必须 `mole down && mole up` —— 而那条路会连带拆掉 TUN、
//   卸掉 PF kill switch（卸载与重装之间存在真空窗口）、重做 DNS 接管与健康探测。
//   一次「给 Apple 加个域名」的改动，代价是把整套网络栈推倒重来，且打断所有在途连接。
//
//   但 sing-box 对 **local rule-set 文件** 是带文件监听的：改文件即时生效，
//   进程不重启、TUN 不断、在途连接不掉。隔离实例实测四次切换全部生效
//   （改文件 / 原子替换 mv 两种写法都能被捕获）。
//   ⚠️ 该重载 **完全静默** —— 日志里一个字都不打，判据只能靠功能验证，不能靠日志。
//   （首次排查时正是因为「日志没动静」差点误判它不支持。）
//
// 范围（有意收窄）：只搬 **纯域名匹配 + 指定 outbound** 的规则。
//   带 ip_cidr / protocol / action / invert 的规则留在 route.rules 内联，
//   因为它们要么表达不了，要么语义与 rule-set 的「匹配集合」模型不一致。
//   判据一句话：这条规则是不是「一组域名 → 一个出站」？是才搬。

// HotRuleSetDir 是热改规则集的存放目录（每个 outbound 一个文件）。
func HotRuleSetDir() string { return filepath.Join(utils.MoleDir(), "rule-sets") }

func hotRuleSetPath(outbound string) string {
	return filepath.Join(HotRuleSetDir(), "mole-"+outbound+".json")
}

func hotRuleSetTag(outbound string) string { return "mole-hot-" + outbound }

// headlessRule 是 sing-box rule-set source 格式里的一条规则。
type headlessRule struct {
	DomainSuffix  []string `json:"domain_suffix,omitempty"`
	DomainKeyword []string `json:"domain_keyword,omitempty"`
}

type ruleSetSource struct {
	Version int            `json:"version"`
	Rules   []headlessRule `json:"rules"`
}

// isHotEligible 判断一条规则能否搬进 rule-set：纯域名匹配 + 有 outbound。
func isHotEligible(r RouteRule) bool {
	if r.Outbound == "" || r.Action != "" || r.Protocol != "" || r.Invert {
		return false
	}
	if len(r.IPCIDR) > 0 || len(r.RuleSet) > 0 {
		return false
	}
	return len(r.DomainSuffix) > 0 || len(r.DomainKeyword) > 0
}

// SplitHotRules 把规则分成「可热改的域名规则（按 outbound 分组）」与「其余原样内联的」。
// 返回的 order 保证分组遍历顺序稳定，避免每次编译产生无意义的 diff。
func SplitHotRules(rules []RouteRule) (byOutbound map[string][]RouteRule, order []string, rest []RouteRule) {
	byOutbound = map[string][]RouteRule{}
	for _, r := range rules {
		if !isHotEligible(r) {
			rest = append(rest, r)
			continue
		}
		if _, seen := byOutbound[r.Outbound]; !seen {
			order = append(order, r.Outbound)
		}
		byOutbound[r.Outbound] = append(byOutbound[r.Outbound], r)
	}
	return byOutbound, order, rest
}

// DetectHotRuleConflicts 报出「同一域名落在不同 outbound 分组」的冲突。
//
// 为什么要拦而不是默默选一个（2026-08-04 LT 定）：内联规则是 **首个匹配胜出**，
// 顺序即优先级；一旦按 outbound 归组，每个出站只剩一条引用规则、位置固定，
// 重复域名谁赢就变了。与其让这个语义差异埋进去，不如在编译期直接拒绝 ——
// 把「靠顺序解决的隐式冲突」变成「必须显式改掉的错误」。
func DetectHotRuleConflicts(byOutbound map[string][]RouteRule) error {
	owner := map[string]string{} // domain -> outbound
	dup := map[string][]string{} // domain -> outbounds
	outs := make([]string, 0, len(byOutbound))
	for ob := range byOutbound {
		outs = append(outs, ob)
	}
	sort.Strings(outs) // 报错内容与 map 遍历顺序无关，否则同一份配置报出的错会随机变
	for _, ob := range outs {
		for _, r := range byOutbound[ob] {
			for _, d := range append(append([]string{}, r.DomainSuffix...), r.DomainKeyword...) {
				prev, ok := owner[d]
				if !ok {
					owner[d] = ob
					continue
				}
				if prev != ob {
					dup[d] = []string{prev, ob}
				}
			}
		}
	}
	if len(dup) == 0 {
		return nil
	}
	keys := make([]string, 0, len(dup))
	for d := range dup {
		keys = append(keys, d)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("规则冲突：同一域名被指向了不同出站，归组后无法用顺序消歧，请在 custom-rules.json 里改掉其中一处\n")
	for _, d := range keys {
		fmt.Fprintf(&b, "  %s → %s 与 %s\n", d, dup[d][0], dup[d][1])
	}
	return fmt.Errorf("%s", b.String())
}

// WriteHotRuleSets 把分组后的域名规则写成 rule-set 文件（原子替换）。
// 已实测 sing-box 的监听能捕获 rename，所以写临时文件再 mv 是安全的，
// 且避免 sing-box 读到写了一半的文件。
func WriteHotRuleSets(byOutbound map[string][]RouteRule, order []string) error {
	if err := os.MkdirAll(HotRuleSetDir(), 0o755); err != nil {
		return fmt.Errorf("create rule-set dir: %w", err)
	}
	for _, ob := range order {
		src := ruleSetSource{Version: 1}
		for _, r := range byOutbound[ob] {
			if len(r.DomainSuffix) > 0 {
				src.Rules = append(src.Rules, headlessRule{DomainSuffix: r.DomainSuffix})
			}
			// ⚠️ suffix 与 keyword 必须拆成两条 headless rule：sing-box 在
			// 单条规则内对不同匹配种类取 AND，混在一条里等于两者都匹配不到
			// （同 types.go RouteRule.DomainKeyword 上的注释，同一个坑）。
			if len(r.DomainKeyword) > 0 {
				src.Rules = append(src.Rules, headlessRule{DomainKeyword: r.DomainKeyword})
			}
		}
		data, err := json.MarshalIndent(src, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal rule-set %s: %w", ob, err)
		}
		data = append(data, '\n')
		final := hotRuleSetPath(ob)
		tmp := final + ".tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return fmt.Errorf("write rule-set %s: %w", ob, err)
		}
		if err := os.Rename(tmp, final); err != nil {
			return fmt.Errorf("install rule-set %s: %w", ob, err)
		}
	}
	return nil
}

// HotRuleSetRefs 生成 sing-box 侧的引用：route.rule_set 声明 + route.rules 里的引用规则。
func HotRuleSetRefs(order []string) (decls []RuleSet, refs []RouteRule) {
	for _, ob := range order {
		decls = append(decls, RuleSet{
			Type:   "local",
			Tag:    hotRuleSetTag(ob),
			Format: "source",
			Path:   hotRuleSetPath(ob),
		})
		refs = append(refs, RouteRule{
			RuleSet:  []string{hotRuleSetTag(ob)},
			Outbound: ob,
		})
	}
	return decls, refs
}

// LoadRoutingRules 返回 builtin + custom 的合并规则集，不需要 server URI。
//
// 为什么单独开这个口子：Build() 要求一个 server URI（它同时在拼出站），而 `mole reload`
// 只关心域名分组，不该因为「当前没有活动服务器」这种与它无关的状态而失败。
func LoadRoutingRules() []RouteRule {
	return append(append([]RouteRule{}, loadBuiltinRules()...), loadCustomRules()...)
}

// MoleDirForTest 返回当前 ~/.mole。测试用它在改写 HOME 之前抓住真实目录，
// 以便把 geoip-cn.srs 这类二进制 rule-set 复制进临时环境。
func MoleDirForTest() string { return utils.MoleDir() }
