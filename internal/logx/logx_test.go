package logx_test

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/emreuyguc/native-agent-api/internal/config"
	"github.com/emreuyguc/native-agent-api/internal/logx"
)

func TestLevelsAndCritical(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logger := logx.New(&buffer, config.Log{Level: slog.LevelWarn, Format: "json"})
	logger.Info("hidden")
	logger.Warn("shown")
	logx.Critical(t.Context(), logger, "fatal")

	out := buffer.String()
	if strings.Contains(out, "hidden") || !strings.Contains(out, `"level":"WARN"`) || !strings.Contains(out, `"level":"CRITICAL"`) {
		t.Fatalf("unexpected log output:\n%s", out)
	}
}

func TestTextFormat(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logx.New(&buffer, config.Log{Level: slog.LevelInfo, Format: "text"}).Info("hello", "k", "v")
	if !strings.Contains(buffer.String(), "level=INFO msg=hello k=v") {
		t.Fatalf("unexpected text output %q", buffer.String())
	}
}

func TestConsoleFormat(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logger := logx.New(&buffer, config.Log{Level: slog.LevelInfo, Format: logx.FormatConsole})
	logger.Info("request", "status", 503, "err", errors.New("boom"))
	logx.Critical(t.Context(), logger, "fatal")

	out := buffer.String()
	for _, want := range []string{"INF", "\x1b[", "CRT", "boom", "503"} {
		if !strings.Contains(out, want) {
			t.Fatalf("console output lacks %q:\n%q", want, out)
		}
	}
}
