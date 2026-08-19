package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Why: 只有「一组域名 → 一个出站」的规则能搬进 rule-set。带 ip_cidr/protocol/
// action/invert 的规则若被误搬，语义会静默改变（rule-set 表达不了动作与取反），
// 而且不会有任何报错 —— 这类错只能靠测试挡。
func TestSplitHotRules_OnlyPureDomainRulesMove(t *testing.T) {
	rules := []RouteRule{
		{DomainSuffix: []string{"a.com"}, Outbound: "proxy"},
		{DomainKeyword: []string{"kw"}, Outbound: "proxy"},
		{DomainSuffix: []string{"b.com"}, Outbound: "direct"},
		{IPCIDR: []string{"1.1.1.1/32"}, Outbound: "direct"},               // 有 IP → 留下
		{Protocol: "dns", Outbound: "direct"},                              // 有 protocol → 留下
		{DomainSuffix: []string{"c.com"}, Action: "reject"},                // 有 action → 留下
		{DomainSuffix: []string{"d.com"}, Outbound: "proxy", Invert: true}, // 取反 → 留下
		{RuleSet: []string{"geoip-cn"}, Outbound: "direct"},                // 已是 rule-set → 留下
	}
	by, order, rest := SplitHotRules(rules)

	if len(rest) != 5 {
		t.Fatalf("应留下 5 条内联规则，实际 %d 条: %+v", len(rest), rest)
	}
	if len(by["proxy"]) != 2 || len(by["direct"]) != 1 {
		t.Fatalf("分组错误: proxy=%d direct=%d", len(by["proxy"]), len(by["direct"]))
	}
	// order 必须按首次出现顺序，否则每次编译产生无意义 diff
	if len(order) != 2 || order[0] != "proxy" || order[1] != "direct" {
		t.Fatalf("order 应为 [proxy direct]，实际 %v", order)
	}
}

// Why（P0，2026-08-04 LT 定）：内联规则首个匹配胜出、顺序即优先级；按出站归组后
// 每个出站只剩一条引用规则、位置固定，重复域名谁赢会变。这个语义差异必须在编译期
// 变成硬错误，而不是埋进去等某天路由走错才发现。
func TestDetectHotRuleConflicts_SameDomainDifferentOutbound(t *testing.T) {
	by, _, _ := SplitHotRules([]RouteRule{
		{DomainSuffix: []string{"x.com"}, Outbound: "proxy"},
		{DomainSuffix: []string{"x.com"}, Outbound: "direct"},
	})
	err := DetectHotRuleConflicts(by)
	if err == nil {
		t.Fatal("同域名指向两个出站，必须报错")
	}
	if !strings.Contains(err.Error(), "x.com") {
		t.Fatalf("报错须指出是哪个域名，实际: %v", err)
	}
}

func TestDetectHotRuleConflicts_SameDomainSameOutboundIsFine(t *testing.T) {
	// 同一出站里重复出现是无害的（两条规则都指向同一个地方），不该误报。
	by, _, _ := SplitHotRules([]RouteRule{
		{DomainSuffix: []string{"x.com"}, Outbound: "proxy"},
		{DomainSuffix: []string{"x.com", "y.com"}, Outbound: "proxy"},
	})
	if err := DetectHotRuleConflicts(by); err != nil {
		t.Fatalf("同出站重复不该报错: %v", err)
	}
}

// Why：sing-box 在单条规则内对不同匹配种类取 AND —— suffix 与 keyword 混进
// 同一条 headless rule 会导致两者都匹配不到，且完全静默。types.go 上已为内联
// 规则记过这个坑，rule-set 侧同理，故用测试钉死。
func TestWriteHotRuleSets_SuffixAndKeywordSplitIntoSeparateRules(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	by, order, _ := SplitHotRules([]RouteRule{
		{DomainSuffix: []string{"a.com"}, DomainKeyword: []string{"kw"}, Outbound: "proxy"},
	})
	if err := WriteHotRuleSets(by, order); err != nil {
		t.Fatalf("写 rule-set 失败: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(HotRuleSetDir(), "mole-proxy.json"))
	if err != nil {
		t.Fatalf("读回 rule-set 失败: %v", err)
	}
	var got ruleSetSource
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("rule-set 不是合法 JSON: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("rule-set version 应为 1，实际 %d", got.Version)
	}
	if len(got.Rules) != 2 {
		t.Fatalf("suffix 与 keyword 必须拆成 2 条 headless rule，实际 %d 条: %+v", len(got.Rules), got.Rules)
	}
	for _, r := range got.Rules {
		if len(r.DomainSuffix) > 0 && len(r.DomainKeyword) > 0 {
			t.Fatalf("同一条 headless rule 里同时出现 suffix 与 keyword（会 AND 成永不匹配）: %+v", r)
		}
	}
}

// Why：sing-box 的文件监听对 rename 可靠、对「写了一半的文件」不可靠，
// 所以写入必须是原子替换，且不能留下 .tmp 残骸被误当成规则文件。
func TestWriteHotRuleSets_AtomicNoTempLeftBehind(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	by, order, _ := SplitHotRules([]RouteRule{
		{DomainSuffix: []string{"a.com"}, Outbound: "proxy"},
	})
	if err := WriteHotRuleSets(by, order); err != nil {
		t.Fatalf("写 rule-set 失败: %v", err)
	}
	entries, err := os.ReadDir(HotRuleSetDir())
	if err != nil {
		t.Fatalf("读目录失败: %v", err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Fatalf("留下了临时文件 %s —— 原子替换未完成", e.Name())
		}
	}
}

// Why：引用规则的 tag 必须与声明的 tag 一致，否则 sing-box 启动即 FATAL
// （引用了不存在的 rule-set）。这是纯机械对应，但错了就整个隧道起不来。
func TestHotRuleSetRefs_TagsMatchDeclarations(t *testing.T) {
	decls, refs := HotRuleSetRefs([]string{"proxy", "direct"})
	if len(decls) != 2 || len(refs) != 2 {
		t.Fatalf("应各生成 2 条，实际 decls=%d refs=%d", len(decls), len(refs))
	}
	for i := range decls {
		if len(refs[i].RuleSet) != 1 || refs[i].RuleSet[0] != decls[i].Tag {
			t.Fatalf("第 %d 条引用的 tag 与声明不符: ref=%v decl=%s", i, refs[i].RuleSet, decls[i].Tag)
		}
		if decls[i].Type != "local" || decls[i].Format != "source" {
			t.Fatalf("rule-set 必须是 local+source（否则不会被热监听）: %+v", decls[i])
		}
	}
}
