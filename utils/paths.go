package utils

import (
	"os"
	"path/filepath"
)

// MoleDir returns the mole directory (~/.mole).
func MoleDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic("cannot find home directory")
	}
	dir := filepath.Join(home, ".mole")
	os.MkdirAll(dir, 0755)
	return dir
}

// BinDir returns the mole bin directory (~/.mole/bin).
func BinDir() string {
	dir := filepath.Join(MoleDir(), "bin")
	os.MkdirAll(dir, 0755)
	return dir
}

// ServersPath is the JSON file storing deployed VPS metadata.
func ServersPath() string { return filepath.Join(MoleDir(), "servers.json") }

// SingboxConfigPath is where the generated sing-box config is written.
func SingboxConfigPath() string { return filepath.Join(MoleDir(), "sing-box-config.json") }

// PIDPath is the running mole process pid file.
func PIDPath() string { return filepath.Join(MoleDir(), "mole.pid") }

// LogPath is where sing-box output is captured.
func LogPath() string { return filepath.Join(MoleDir(), "mole.log") }

// StatePath is where the running supervisor writes its health state.
func StatePath() string { return filepath.Join(MoleDir(), "state.json") }

// CustomRulesPath is the optional JSON file for user-defined routing rules.
func CustomRulesPath() string { return filepath.Join(MoleDir(), "custom-rules.json") }
