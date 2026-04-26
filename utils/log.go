package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// RotateLog renames the current LogPath() to a timestamped backup
// (mole.log.YYYYMMDD-HHMMSS) and prunes everything beyond the newest `keep`
// backups. The old behavior — truncating mole.log on every `mole up` — meant
// that if a startup failed and the user re-ran, the failed run's sing-box log
// was already gone before they could read it. Keeping a small ring of past
// logs is the cheapest fix and adds predictable disk usage (~5 * one-run size).
//
// Returns nil if there's nothing to rotate. All errors are best-effort: a
// rotation failure must never block `mole up`, so callers should ignore the
// return except for telemetry.
func RotateLog(keep int) error {
	src := LogPath()
	st, err := os.Stat(src)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st.Size() == 0 {
		// Nothing useful to keep; reuse the file as-is.
		return nil
	}

	// Use a deterministic, sortable suffix so pruning by name = pruning by age.
	stamp := time.Now().Format("20060102-150405")
	dst := fmt.Sprintf("%s.%s", src, stamp)

	// Avoid overwriting an existing backup if the same timestamp lands twice
	// (sub-second `mole down && mole up` loops); append a counter.
	if _, err := os.Stat(dst); err == nil {
		for i := 1; i < 100; i++ {
			candidate := fmt.Sprintf("%s.%d", dst, i)
			if _, err := os.Stat(candidate); os.IsNotExist(err) {
				dst = candidate
				break
			}
		}
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	pruneOldLogs(keep)
	return nil
}

// pruneOldLogs deletes mole.log.* backups beyond the newest `keep`.
func pruneOldLogs(keep int) {
	if keep < 0 {
		keep = 0
	}
	dir := MoleDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	prefix := filepath.Base(LogPath()) + "."
	var backups []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); len(name) > len(prefix) && name[:len(prefix)] == prefix {
			backups = append(backups, name)
		}
	}
	if len(backups) <= keep {
		return
	}
	// Sortable timestamps mean lexicographic sort = chronological sort.
	sort.Strings(backups)
	for _, name := range backups[:len(backups)-keep] {
		_ = os.Remove(filepath.Join(dir, name))
	}
}
