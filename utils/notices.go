package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// noticeRepeatInterval is how long the same warning stays quiet after it has
// been printed once. A cache refresh that cannot reach the upstream fails on
// every single `mole up`; printing it each time turns a real signal into
// wallpaper, and the operator learns to scroll past it. Once a day is enough
// to know it is still broken, and it leaves the per-run detail in the log.
const noticeRepeatInterval = 24 * time.Hour

var noticeMu sync.Mutex

// WarnOncePerDay prints a warning at most once per key in any 24h window.
// State lives in ~/.mole/notices.json; a missing or unreadable file simply
// means "nothing was warned yet", never an error of its own.
func WarnOncePerDay(key, format string, args ...any) {
	noticeMu.Lock()
	defer noticeMu.Unlock()

	path := filepath.Join(MoleDir(), "notices.json")
	seen := map[string]string{}
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &seen)
	}
	if at, ok := seen[key]; ok {
		if t, err := time.Parse(time.RFC3339, at); err == nil && time.Since(t) < noticeRepeatInterval {
			return
		}
	}

	fmt.Printf(format, args...)

	seen[key] = time.Now().UTC().Format(time.RFC3339)
	data, err := json.MarshalIndent(seen, "", "  ")
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0644); err == nil {
		ChownToMoleDirOwner(path)
	}
}
