package logging

import (
	"fmt"
	"log/slog"
	"os"
	"strings"
)

var singleLineReplacer = strings.NewReplacer("\r\n", "\\n", "\n", "\\n", "\r", "\\r")

func init() { Configure("info", "json", false) }

func Configure(level, format string, addSource bool) {
	resolvedLevel := slog.LevelInfo
	switch strings.TrimSpace(strings.ToLower(level)) {
	case "debug":
		resolvedLevel = slog.LevelDebug
	case "warn":
		resolvedLevel = slog.LevelWarn
	case "error":
		resolvedLevel = slog.LevelError
	}
	options := &slog.HandlerOptions{Level: resolvedLevel, AddSource: addSource}
	if strings.TrimSpace(strings.ToLower(format)) == "text" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, options)))
		return
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, options)))
}

func message(format string, args ...any) string {
	return singleLineReplacer.Replace(fmt.Sprintf(format, args...))
}

func Infof(format string, args ...any)  { slog.Info(message(format, args...)) }
func Warnf(format string, args ...any)  { slog.Warn(message(format, args...)) }
func Errorf(format string, args ...any) { slog.Error(message(format, args...)) }
func Fatalf(format string, args ...any) { slog.Error(message(format, args...)); os.Exit(1) }
