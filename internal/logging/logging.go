// Package logging sets up the process-wide logger for the manager and agent:
// log/slog behind -log-format text|json and -log-level.
//
// Text mode is built to keep the log readable exactly as it was before slog:
//
//	2026/10/09 12:00:01 workload.completed workload=ab12 node=cd34
//
// the same timestamp the standard logger printed, the message, then key=value
// fields, with a [WARN] / [ERROR] / [DEBUG] tag only when the level is not
// INFO. Existing log.Printf calls need no change: slog.SetDefault routes the
// standard logger through the same handler (at INFO), so a line written the
// old way is formatted the new way (in JSON mode it becomes a record whose
// msg is the whole sentence).
//
// Field names used across the code base, so logs can be filtered the same way
// everywhere: node, workload, job, workflow, capability, reason, request.
// Never log secrets, tokens, prompts, request text, file contents or a full
// argv. As a safety net against a mistake, a field whose key names such a
// thing (token, key, secret, password, authorization, prompt, argv, args,
// command, stdin, stdout, stderr, content) is written as [redacted] by both
// handlers.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Formats and levels accepted by the flags.
const (
	FormatText = "text"
	FormatJSON = "json"
)

// Setup installs the default logger writing to out. format is "text" or
// "json"; level is debug, info, warn or error.
func Setup(format, level string, out io.Writer) error {
	h, err := NewHandler(format, level, out)
	if err != nil {
		return err
	}
	slog.SetDefault(slog.New(h))
	return nil
}

// NewHandler builds the handler Setup installs (exposed for tests).
func NewHandler(format, level string, out io.Writer) (slog.Handler, error) {
	var lv slog.LevelVar
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lv.Set(slog.LevelDebug)
	case "", "info":
		lv.Set(slog.LevelInfo)
	case "warn", "warning":
		lv.Set(slog.LevelWarn)
	case "error":
		lv.Set(slog.LevelError)
	default:
		return nil, fmt.Errorf("-log-level %q: want debug, info, warn or error", level)
	}
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", FormatText:
		return &textHandler{out: out, level: &lv, mu: &sync.Mutex{}}, nil
	case FormatJSON:
		return slog.NewJSONHandler(out, &slog.HandlerOptions{Level: &lv, ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && secretKey(a.Key) {
				return slog.String(a.Key, redacted)
			}
			return a
		}}), nil
	default:
		return nil, fmt.Errorf("-log-format %q: want text or json", format)
	}
}

const redacted = "[redacted]"

var secretWords = []string{"token", "secret", "password", "passwd", "authorization", "prompt", "argv", "stdin", "stdout", "stderr", "content", "apikey", "api_key", "credential"}

// secretKey reports whether a field named key must never carry its value.
func secretKey(key string) bool {
	k := strings.ToLower(key)
	switch k {
	case "key", "args", "command", "cmd":
		return true
	}
	for _, w := range secretWords {
		if strings.Contains(k, w) {
			return true
		}
	}
	return false
}

// textHandler prints "2006/01/02 15:04:05 [LEVEL] msg k=v k=v".
type textHandler struct {
	out    io.Writer
	level  *slog.LevelVar
	mu     *sync.Mutex
	attrs  []slog.Attr
	groups []string
}

func (h *textHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level.Level() }

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	c := *h
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), qualify(h.groups, attrs)...)
	return &c
}

func (h *textHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.groups = append(append([]string(nil), h.groups...), name)
	return &c
}

func qualify(groups []string, attrs []slog.Attr) []slog.Attr {
	if len(groups) == 0 {
		return attrs
	}
	prefix := strings.Join(groups, ".") + "."
	out := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		out[i] = slog.Attr{Key: prefix + a.Key, Value: a.Value}
	}
	return out
}

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	b.WriteString(t.Format("2006/01/02 15:04:05"))
	b.WriteByte(' ')
	if r.Level != slog.LevelInfo {
		b.WriteString("[" + r.Level.String() + "] ")
	}
	b.WriteString(strings.TrimRight(r.Message, "\r\n"))
	for _, a := range h.attrs {
		writeAttr(&b, "", a)
	}
	r.Attrs(func(a slog.Attr) bool {
		for _, q := range qualify(h.groups, []slog.Attr{a}) {
			writeAttr(&b, "", q)
		}
		return true
	})
	b.WriteByte('\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.out, b.String())
	return err
}

func writeAttr(b *strings.Builder, prefix string, a slog.Attr) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, g := range a.Value.Group() {
			writeAttr(b, prefix+a.Key+".", g)
		}
		return
	}
	key := prefix + a.Key
	b.WriteByte(' ')
	b.WriteString(key)
	b.WriteByte('=')
	if secretKey(a.Key) {
		b.WriteString(redacted)
		return
	}
	s := a.Value.String()
	if s == "" || strings.ContainsAny(s, " \t\r\n\"=") {
		s = strconv.Quote(s)
	}
	b.WriteString(s)
}
