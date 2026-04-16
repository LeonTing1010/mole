package utils

import (
	"fmt"
	"os"
	"time"
)

// MoleLog writes a timestamped log message to ~/.mole/mole.log
func MoleLog(level string, msg string) {
	timestamp := time.Now().Format("2006-01-02T15:04:05")
	logLine := fmt.Sprintf("%s [%s] %s\n", timestamp, level, msg)

	f, err := os.OpenFile(LogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	f.WriteString(logLine)
}

// MoleLogInfo logs an info message
func MoleLogInfo(msg string) {
	MoleLog("INFO", msg)
}

// MoleLogError logs an error message
func MoleLogError(msg string) {
	MoleLog("ERROR", msg)
}

// MoleLogDebug logs a debug message
func MoleLogDebug(msg string) {
	MoleLog("DEBUG", msg)
}

// MoleLogWarn logs a warning message
func MoleLogWarn(msg string) {
	MoleLog("WARN", msg)
}

// ReadLogTail reads the last n lines from the log file
func ReadLogTail(n int) ([]string, error) {
	data, err := os.ReadFile(LogPath())
	if err != nil {
		return nil, err
	}

	lines := []string{}
	start := 0
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == '\n' {
			line := string(data[start:i])
			if line != "" {
				lines = append([]string{line}, lines...)
			}
			start = i + 1
			if len(lines) >= n {
				break
			}
		}
	}

	return lines, nil
}
