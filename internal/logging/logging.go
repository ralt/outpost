package logging

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
)

// Component is the short subsystem tag attached to every log line.
type Component string

const (
	CompDaemon Component = "daemon"
	CompConfig Component = "config"
	CompFUSE   Component = "fuse"
	CompCtl    Component = "ctl"
	CompSSH    Component = "ssh"
	CompSync   Component = "sync"
)

type Options struct {
	Level  string // debug|info|warn|error
	Format string // text|json
	Writer io.Writer
}

// New creates the root slog.Logger honoring OUTPOST_LOG_LEVEL > opts.Level.
func New(opts Options) *slog.Logger {
	level := slog.LevelInfo
	source := opts.Level
	if env := os.Getenv("OUTPOST_LOG_LEVEL"); env != "" {
		source = env
	}
	switch strings.ToLower(source) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	w := opts.Writer
	if w == nil {
		w = os.Stderr
	}
	if strings.ToLower(opts.Format) == "json" {
		return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
	}
	return slog.New(newTextHandler(w, level))
}

type textHandler struct {
	w     io.Writer
	mu    *sync.Mutex
	level slog.Level
	attrs []slog.Attr
	group string
}

func newTextHandler(w io.Writer, level slog.Level) *textHandler {
	return &textHandler{w: w, mu: &sync.Mutex{}, level: level}
}

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Time.UTC().Format("2006-01-02T15:04:05.000Z"))
	b.WriteByte(' ')
	b.WriteString(levelName(r.Level))
	b.WriteByte(' ')

	component := ""
	reqID := ""
	taskID := ""
	other := make([]slog.Attr, 0, 4)

	collect := func(a slog.Attr) {
		switch a.Key {
		case "component":
			component = a.Value.String()
		case "req":
			reqID = a.Value.String()
		case "task":
			taskID = a.Value.String()
		default:
			other = append(other, a)
		}
	}
	for _, a := range h.attrs {
		collect(a)
	}
	r.Attrs(func(a slog.Attr) bool {
		collect(a)
		return true
	})

	if component == "" {
		component = "-"
	}
	b.WriteString(padRight(component, 8))
	b.WriteByte(' ')
	if reqID != "" {
		b.WriteString("req=")
		b.WriteString(reqID)
		b.WriteByte(' ')
	}
	if taskID != "" {
		b.WriteString("task=")
		b.WriteString(taskID)
		b.WriteByte(' ')
	}
	b.WriteString(r.Message)
	for _, a := range other {
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		b.WriteString(formatVal(a.Value))
	}
	b.WriteByte('\n')

	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &textHandler{w: h.w, mu: h.mu, level: h.level, attrs: merged, group: h.group}
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	return &textHandler{w: h.w, mu: h.mu, level: h.level, attrs: h.attrs, group: name}
}

func levelName(l slog.Level) string {
	switch {
	case l >= slog.LevelError:
		return "ERROR"
	case l >= slog.LevelWarn:
		return "WARN "
	case l >= slog.LevelInfo:
		return "INFO "
	default:
		return "DEBUG"
	}
}

func formatVal(v slog.Value) string {
	switch v.Kind() {
	case slog.KindString:
		s := v.String()
		if strings.ContainsAny(s, " \t\"") {
			return fmt.Sprintf("%q", s)
		}
		return s
	case slog.KindDuration:
		return v.Duration().String()
	case slog.KindTime:
		return v.Time().UTC().Format(time.RFC3339Nano)
	default:
		return v.String()
	}
}

func padRight(s string, n int) string {
	if len(s) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(s))
}

// WithComponent returns a logger that tags every line with the given component.
func WithComponent(l *slog.Logger, c Component) *slog.Logger {
	return l.With("component", string(c))
}

// NewTraceID returns an 8-char base32 trace id (uppercase letters + 2-7).
func NewTraceID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))[:8]
}
