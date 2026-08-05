package main

import (
	"os"
	"path/filepath"

	"github.com/rs/zerolog"
)

// NewLogger creates a zerolog.Logger that writes to both the console (human-readable,
// colored) and a JSON log file named fileName inside logDir. If the directory cannot
// be created or the file cannot be opened, it falls back to console-only output.
// logLevel controls the minimum level ("debug", "info", "warn", "error").
func NewLogger(logDir, logLevel, fileName string) zerolog.Logger {
	level, err := zerolog.ParseLevel(logLevel)
	if err != nil {
		level = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(level)

	consoleWriter := zerolog.ConsoleWriter{Out: os.Stdout}

	if err := os.MkdirAll(logDir, 0755); err != nil {
		return zerolog.New(consoleWriter).With().Timestamp().Logger()
	}

	logFile := filepath.Join(logDir, fileName)
	file, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return zerolog.New(consoleWriter).With().Timestamp().Logger()
	}

	multi := zerolog.MultiLevelWriter(consoleWriter, file)
	return zerolog.New(multi).With().Timestamp().Logger()
}
