package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

const (
	defaultLogLevel     = "info"
	defaultLogFormat    = "json"
	defaultLogAddSource = false
)

var singleLineReplacer = strings.NewReplacer(
	"\r\n", "\\n",
	"\n", "\\n",
	"\r", "\\r",
)

func formatSingleLine(format string, args ...any) string {
	return singleLineReplacer.Replace(fmt.Sprintf(format, args...))
}

func init() {
	Configure(defaultLogLevel, defaultLogFormat, defaultLogAddSource)
}

func Configure(level string, format string, addSource bool) {
	slog.SetDefault(newLogger(level, format, addSource))
}

func Infof(format string, args ...any) {
	slog.Info(formatSingleLine(format, args...))
}

func Warnf(format string, args ...any) {
	slog.Warn(formatSingleLine(format, args...))
}

func Errorf(format string, args ...any) {
	slog.Error(formatSingleLine(format, args...))
}

func Fatalf(format string, args ...any) {
	slog.Error(formatSingleLine(format, args...))
	os.Exit(1)
}

func newLogger(level string, format string, addSource bool) *slog.Logger {
	resolvedLevel := slog.LevelInfo
	switch strings.TrimSpace(strings.ToLower(level)) {
	case "debug":
		resolvedLevel = slog.LevelDebug
	case "warn":
		resolvedLevel = slog.LevelWarn
	case "error":
		resolvedLevel = slog.LevelError
	}

	options := &slog.HandlerOptions{
		Level:     resolvedLevel,
		AddSource: addSource,
	}
	if strings.TrimSpace(strings.ToLower(format)) == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, options))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, options))
}
