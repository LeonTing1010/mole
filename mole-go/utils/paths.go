package utils

import (
	"os"
	"path/filepath"
)

// MoleDir returns the mole configuration directory (~/.mole)
func MoleDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		panic("cannot find home directory")
	}
	dir := filepath.Join(home, ".mole")
	os.MkdirAll(dir, 0755)
	return dir
}

// BinDir returns the mole bin directory (~/.mole/bin)
func BinDir() string {
	dir := filepath.Join(MoleDir(), "bin")
	os.MkdirAll(dir, 0755)
	return dir
}

// ConfigPath returns the mole config path (~/.mole/config.yaml)
func ConfigPath() string {
	return filepath.Join(MoleDir(), "config.yaml")
}

// HiddifyConfigPath returns the Hiddify config path (~/.mole/hiddify-config.json)
func HiddifyConfigPath() string {
	return filepath.Join(MoleDir(), "hiddify-config.json")
}

// PIDPath returns the PID file path (~/.mole/mole.pid)
func PIDPath() string {
	return filepath.Join(MoleDir(), "mole.pid")
}

// LogPath returns the mole log path (~/.mole/mole.log)
func LogPath() string {
	return filepath.Join(MoleDir(), "mole.log")
}

// HiddifyLogPath returns the Hiddify log path (~/.mole/hiddify.log)
func HiddifyLogPath() string {
	return filepath.Join(MoleDir(), "hiddify.log")
}

// HiddifyCliPath returns the HiddifyCli binary path (~/.mole/bin/HiddifyCli)
func HiddifyCliPath() string {
	return filepath.Join(BinDir(), "HiddifyCli")
}
