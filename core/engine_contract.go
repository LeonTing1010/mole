package core

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The config/cache contract is qualified against stable 1.14.3 and 1.14.4.
// A new minor may remove fields or change the cache schema. Fail before network
// takeover rather than enable deprecated options until an unknown engine boots.
func validateEngineVersion(output string) error {
	match := regexp.MustCompile(`(?m)^sing-box version (\d+)\.(\d+)\.(\d+)\s*$`).FindStringSubmatch(output)
	if len(match) == 4 {
		patch, _ := strconv.Atoi(match[3])
		if match[1] == "1" && match[2] == "14" && patch >= 3 {
			return nil
		}
	}
	return fmt.Errorf("unqualified sing-box version: require stable 1.14.x >= 1.14.3; install qualified 1.14.4 before starting Mole")
}

func engineEnv() []string {
	return append(os.Environ(), "ENABLE_DEPRECATED_LEGACY_DNS_SERVERS=true", "ENABLE_DEPRECATED_MISSING_DOMAIN_RESOLVER=true")
}

func checkEngineConfig(engine, path string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, engine, "version").Output()
	if err != nil {
		return fmt.Errorf("read sing-box version: %w", err)
	}
	if err := validateEngineVersion(string(out)); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, engine, "check", "-c", path)
	cmd.Env = engineEnv()
	// Bound the diagnostic returned to the foreground or daemon log.
	out, err = cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(out))
		if len(detail) > 1200 {
			detail = detail[:1200]
		}
		return fmt.Errorf("sing-box config check failed: %w: %s", err, detail)
	}
	return nil
}

// PreflightEngine is read-only: do not open or repair a live cache here.
func PreflightEngine(configPath string) error {
	path, err := findSingbox()
	if err != nil {
		return err
	}
	return checkEngineConfig(path, configPath)
}
