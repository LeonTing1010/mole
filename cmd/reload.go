package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LeonTing1010/mole/config"
	"github.com/LeonTing1010/mole/utils"
	"github.com/spf13/cobra"
)

var reloadCmd = &cobra.Command{
	Use:   "reload",
	Short: "Apply custom-rules.json domain changes without reconnecting",
	Long: `Recompile the域名 rule-sets from custom-rules.json (+ builtin-rules.json) and
let the running sing-box pick them up in place.

The TUN, DNS takeover, PF kill switch and every in-flight connection are
untouched — sing-box watches its local rule-set files and reloads them itself.
No sudo, no reconnect.

Only "a set of domains → one outbound" rules travel this path. Anything else
(ip_cidr, protocol, action, invert), a new outbound, a changed bandwidth ladder
or DNS strategy still needs "mole down && mole up", because those are baked into
sing-box's config at load time and no file watch can change them.

⚠️ The reload is SILENT: sing-box logs nothing when it re-reads a rule-set, so
"no log line" is not evidence it failed. Verify by behaviour (curl the domain),
never by grepping the log.`,
	Args: cobra.NoArgs,
	RunE: runReload,
}

func init() { rootCmd.AddCommand(reloadCmd) }

func runReload(_ *cobra.Command, _ []string) error {
	// 没在跑也允许 reload：文件写好了，下次 `mole up` 直接就是新的。
	// 但要如实说明「现在没有进程会读它」，否则用户会以为已经生效。
	running := true
	if pid, err := utils.ReadPID(); err != nil || !utils.IsRunning(pid) {
		running = false
	}

	// 走与守护进程同一份实现，避免「手动 reload」与「自动生效」给出两套规则。
	total, err := RecompileHotRules()
	if err != nil {
		return err
	}
	byOutbound, order, _ := config.SplitHotRules(config.LoadRoutingRules())
	for _, ob := range order {
		n := 0
		for _, r := range byOutbound[ob] {
			n += len(r.DomainSuffix) + len(r.DomainKeyword)
		}
		fmt.Printf("  %-8s %3d 条域名  → %s\n", ob, n,
			shortPath(filepath.Join(config.HotRuleSetDir(), "mole-"+ob+".json")))
	}

	// 规则变了，PF 的 direct 豁免表也得跟着变 —— 但只在「把一个域名从 direct 里
	// 删掉」时才需要动 PF：豁免是 fail-closed 规则集上的洞，规则没了洞必须跟着关。
	// 新加的域名不在这里预开洞（守护进程看到第一次拨号自己会补上），所以常见的
	// reload 依旧一次 pfctl 都不碰，也就依旧不要 sudo。
	if dropped := utils.PruneDirectExemptions(config.DirectDomains()); len(dropped) > 0 {
		fmt.Printf("🛡️  已收回 %d 个主机名的直连豁免（不再是 direct）：%s\n",
			len(dropped), strings.Join(dropped, ", "))
	}

	if !running {
		fmt.Printf("✅ 已重写 %d 条域名规则，但 mole 没在运行 —— 下次 `mole up` 生效\n", total)
		return nil
	}
	fmt.Printf("✅ 已重写 %d 条域名规则，运行中的 sing-box 会就地重载（TUN/连接不受影响）\n", total)
	fmt.Println("   验证请用行为，别看日志：sing-box 重载 rule-set 时不打任何日志")
	fmt.Println("   DNS 域名策略仍是启动时的配置；新增/调整直连或代理域名后需 `mole down && mole up` 同步 DNS")
	return nil
}

func shortPath(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !filepath.IsAbs(rel) {
			return "~/" + rel
		}
	}
	return p
}

// RecompileHotRules 重新编译域名 rule-set，返回域名条数。
//
// 唯一实现，两个调用方共用：`mole reload`（显式）与守护进程的规则监听（自动）。
// 不做成两份是因为它们必须永远给出同一个结果 —— 一旦漂移，
// 「我 reload 过了」和「守护进程自动生效的」就会是两套规则。
func RecompileHotRules() (int, error) {
	rules, err := config.LoadRoutingRulesStrict()
	if err != nil {
		return 0, err
	}
	byOutbound, order, _ := config.SplitHotRules(rules)
	if err := config.DetectHotRuleConflicts(byOutbound); err != nil {
		return 0, err
	}
	if err := config.WriteHotRuleSets(byOutbound, order); err != nil {
		return 0, err
	}
	n := 0
	for _, ob := range order {
		for _, r := range byOutbound[ob] {
			n += len(r.DomainSuffix) + len(r.DomainKeyword)
		}
	}
	return n, nil
}
