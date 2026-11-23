package utils

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// The manual ceiling pin. Writing it makes the supervisor's bandwidth scheduler
// stop following the clock and hold the pinned rung; clearing it hands control
// back. The value is the Brutal DOWN ceiling in Mbps, which is also what names
// the selector member the scheduler switches to (core.BandwidthRungTag).
//
// The ceiling file remains a bare integer so an older running daemon still
// honors the pin when startup tuning is enabled for its replacement.

type CeilingSetting struct {
	Down int
	Tune bool
}

// ReadCeiling returns the fixed rung or the starting rung for a startup tune.
func ReadCeiling() int { return readPositiveCeiling(CeilingPath()) }

func ReadCeilingSetting() CeilingSetting {
	down := ReadCeiling()
	return CeilingSetting{Down: down, Tune: down > 0 && readPositiveCeiling(ceilingTunePath()) == down}
}

func ceilingTunePath() string { return filepath.Join(MoleDir(), "ceiling-tune") }

func readPositiveCeiling(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// WriteCeiling pins the ceiling to down Mbps and disables startup tuning.
func WriteCeiling(down int) error {
	return WriteCeilingSetting(CeilingSetting{Down: down})
}

func WriteCeilingSetting(setting CeilingSetting) error {
	if setting.Down <= 0 {
		return fmt.Errorf("ceiling must be positive, got %d", setting.Down)
	}
	if !setting.Tune {
		if err := removeCeilingFile(ceilingTunePath()); err != nil {
			return err
		}
	}
	data := []byte(strconv.Itoa(setting.Down) + "\n")
	if err := writeCeilingFile(CeilingPath(), data); err != nil {
		return err
	}
	if setting.Tune {
		return writeCeilingFile(ceilingTunePath(), data)
	}
	return nil
}

// The result is separate from the user's setting so the daemon cannot
// overwrite a manual override that arrives during a benchmark.
type tunedCeiling struct {
	Server string
	Seed   int
	Down   int
}

func ReadTunedCeiling(server string, seed int) int {
	data, err := os.ReadFile(filepath.Join(MoleDir(), "tuned-ceiling.json"))
	var last tunedCeiling
	if err != nil || json.Unmarshal(data, &last) != nil || last.Server != server || last.Seed != seed {
		return 0
	}
	return last.Down
}

func WriteTunedCeiling(server string, seed, down int) error {
	data, err := json.Marshal(tunedCeiling{Server: server, Seed: seed, Down: down})
	if err != nil {
		return err
	}
	return writeCeilingFile(filepath.Join(MoleDir(), "tuned-ceiling.json"), data)
}

func writeCeilingFile(path string, data []byte) error {
	file, err := os.CreateTemp(MoleDir(), ".ceiling-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(data)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	ChownToMoleDirOwner(file.Name())
	return os.Rename(file.Name(), path)
}

// ClearCeiling removes the pin, returning the scheduler to clock control.
// A missing file is success — clearing an unpinned ceiling is a no-op.
func ClearCeiling() error {
	if err := removeCeilingFile(ceilingTunePath()); err != nil {
		return err
	}
	return removeCeilingFile(CeilingPath())
}

func removeCeilingFile(path string) error {
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
