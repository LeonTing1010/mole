package core

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/LeonTing1010/mole/utils"
)

// 规则热更监听：改 custom-rules.json 即时生效，不需要敲任何命令。
//
// 为什么放在守护进程里而不是让用户跑 `mole reload`（2026-08-04 LT 定）：
//
//	守护进程本来就活着，而「改完文件还要记得敲一条命令」正是本项目已经踩过的坑 ——
//	2026-08-03 排查 iOS 上传时，规则写进了 custom-rules.json 却没重启，
//	于是「已经加了规则」这个假前提差点让整条排查跑偏。
//	⇒ 让「改了文件」和「生效」之间不留人工步骤，是消除这类误判的唯一办法。
//
// 为什么是轮询 mtime 而不是 fsnotify：本项目不依赖 fsnotify，为一个 2 秒延迟
//
//	无所谓的场景引入新依赖不划算；而且 mtime 轮询对「原子替换(rename)」天然可靠，
//	fsnotify 反而常在 rename 上漏事件。
//
// ⚠️ 重编译逻辑通过 OnRulesChanged 回调注入，core 不 import config ——
//
//	config 已经 import core（parser.go 取服务器地址），直连会成循环依赖。
//
// ⚠️ 只覆盖域名规则（范围见 config/hotrules.go）。新增出站、改带宽阶梯、改 DNS 策略
//
//	仍需 `mole down && mole up` —— 那些在 sing-box 载入配置时就烘焙进去了。
const rulesWatchInterval = 2 * time.Second

// watchRules 轮询规则源文件，变了就调用注入的重编译回调。
// sing-box 自己监听生成出来的 rule-set 文件，写完即生效 —— 本函数不碰进程、不碰 TUN。
func (s *Supervisor) watchRules(ctx context.Context) {
	if s.onRulesChanged == nil {
		return // 没注入回调 = 该功能未启用，安静退出而不是空转
	}
	paths := []string{utils.CustomRulesPath()}
	last := stampFiles(paths)

	ticker := time.NewTicker(rulesWatchInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cur := stampFiles(paths)
			if cur == last {
				continue
			}
			last = cur
			// 失败只记日志不中断守护进程：一份写坏的规则文件不该把还在正常工作的
			// 隧道拖下水 —— 上一份 rule-set 仍然生效。
			n, err := s.onRulesChanged()
			if err != nil {
				log.Printf("rules: 规则未应用，沿用上一份生效的规则: %v", err)
				continue
			}
			// ⚠️ 这行日志证明的是「mole 重写了 rule-set」，**不是**「sing-box 已重载」——
			// sing-box 重载 rule-set 时不打任何日志（2026-08-04 实测）。要确认最终效果
			// 只能看行为（curl 那个域名），不能看日志。
			log.Printf("rules: custom-rules.json 已变更 → 重写 %d 条域名规则，sing-box 就地重载（TUN/连接不受影响）", n)
		}
	}
}

// stampFiles 把一组文件的「修改时间+大小」揉成一个可比较的字符串。
// 缺失的文件也参与比较（记为 -），所以「删掉规则文件」同样会被视为一次变更。
func stampFiles(paths []string) string {
	out := ""
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			out += p + ":-;"
			continue
		}
		out += p + ":" + fi.ModTime().UTC().Format(time.RFC3339Nano) + ":" +
			strconv.FormatInt(fi.Size(), 10) + ";"
	}
	return out
}
