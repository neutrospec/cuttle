package logx

import (
	"fmt"
	"io"
	"log/slog"
	"time"
)

// NewLogger returns a slog text logger that writes times in UTC with
// millisecond precision, as in design 0002 D8, so logs carry no local time
// zone.
func NewLogger(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey && len(groups) == 0 {
				return slog.String(slog.TimeKey, a.Value.Time().UTC().Format("2006-01-02T15:04:05.000Z"))
			}
			return a
		},
	}))
}

// ParseLevel maps a configuration level name to a slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}

// Duration formats a duration for log lines, rounded to milliseconds.
func Duration(d time.Duration) string {
	return d.Round(time.Millisecond).String()
}
