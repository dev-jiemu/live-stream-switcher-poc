package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/dev-jiemu/live-stream-switcher-poc/config"
	"gopkg.in/natefinch/lumberjack.v2"
)

func slogLevelParser(lvStr string) (slog.Level, error) {
	dict := map[string]slog.Level{
		"debug": slog.LevelDebug,
		"info":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	result, ok := dict[strings.ToLower(lvStr)]
	if !ok {
		return result, fmt.Errorf("%s is not valid log level", lvStr)
	}
	return result, nil
}

func SlogInit() error {
	slogLogger := config.EnvConfig.Logger

	logLevel, err := slogLevelParser(slogLogger.Level)
	if err != nil {
		return err
	}

	var logWriter io.Writer
	if slogLogger.Path == "" {
		logWriter = os.Stdout
	} else {
		fileWriter := &lumberjack.Logger{
			Filename:   slogLogger.Path,
			MaxSize:    slogLogger.MaxSizeMb,
			MaxBackups: slogLogger.MaxBackups,
			Compress:   true, // disabled by default
		}
		if slogLogger.PrintStdOut {
			logWriter = io.MultiWriter(os.Stdout, fileWriter)
		} else {
			logWriter = fileWriter
		}
	}

	handler := slog.NewJSONHandler(logWriter, &slog.HandlerOptions{
		Level:     logLevel,
		AddSource: true,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.SourceKey {
				s := a.Value.Any().(*slog.Source)
				s.File = getSimplePath(s.File)
			}
			return a
		},
	})

	reqLog := slog.New(handler)
	slog.SetDefault(reqLog)
	return nil
}

func getSimplePath(path string) string {
	if len(path) <= 10 {
		return path
	}
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return path
	}
	jdx := strings.LastIndex(path[:idx], "/")
	if jdx < 0 {
		return path
	}
	return path[jdx:]
}
