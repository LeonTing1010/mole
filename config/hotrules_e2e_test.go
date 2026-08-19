package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Why（端到端）：单测只能证明 rule-set 文件的形状对，证明不了 sing-box 肯收。
// 而这条链路错了的后果是最重的一档 —— sing-box 对「引用了不存在/格式不对的
// rule-set」是启动即 FATAL，也就是隧道整个起不来。所以必须由 sing-box 自己判卷。
//
// 没有 sing-box 二进制时跳过：这是环境缺失，不是代码失败。
func TestBuild_ConfigWithHotRuleSetsPassesSingboxCheck(t *testing.T) {
	sb, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("环境无 sing-box，跳过端到端校验")
	}

	// 先在改 HOME 之前拿到真实的 ~/.mole，用来复制二进制 rule-set —— 否则
	// sing-box check 会因为缺 geoip-cn.srs 而失败，那与本测试无关。
	realMole := MoleDirForTest()

	home := t.TempDir()
	t.Setenv("HOME", home)
	copyIfExists(t, filepath.Join(realMole, "geoip-cn.srs"), filepath.Join(home, ".mole", "geoip-cn.srs"))
	copyIfExists(t, filepath.Join(realMole, "geosite-cn.srs"), filepath.Join(home, ".mole", "geosite-cn.srs"))

	// 造一份含「域名→出站」的自定义规则，逼 Build 走热规则集这条路。
	if err := os.MkdirAll(filepath.Join(home, ".mole"), 0o755); err != nil {
		t.Fatal(err)
	}
	custom := `[
	  {"domain_suffix": ["example.com", "sub.example.org"], "outbound": "proxy"},
	  {"domain_keyword": ["keyword-host"], "outbound": "proxy"},
	  {"domain_suffix": ["cn-service.cn"], "outbound": "direct"}
	]`
	if err := os.WriteFile(filepath.Join(home, ".mole", "custom-rules.json"), []byte(custom), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Build("hysteria2://pass@203.0.113.10:443/?sni=example.com&insecure=1")
	if err != nil {
		t.Fatalf("Build 失败: %v", err)
	}

	out := filepath.Join(home, "cfg.json")
	if err := Save(cfg, out); err != nil {
		t.Fatalf("Save 失败: %v", err)
	}

	for _, rs := range cfg.Route.RuleSet {
		if rs.Format == "binary" && rs.Path != "" {
			if _, err := os.Stat(rs.Path); os.IsNotExist(err) {
				t.Skipf("本机没有 %s，跳过（与热规则集无关）", filepath.Base(rs.Path))
			}
		}
	}

	if b, err := exec.Command(sb, "check", "-c", out).CombinedOutput(); err != nil {
		t.Fatalf("sing-box 拒绝了生成的配置: %v\n%s", err, b)
	}
}

// copyIfExists 复制文件，源不存在就静默跳过。
func copyIfExists(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
