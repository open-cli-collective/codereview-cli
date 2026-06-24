// Package progress writes structured progress breadcrumbs to stderr for long-running CLI work.
package progress

import (
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Clock returns the current time.
type Clock func() time.Time

// Logger writes structured progress lines.
type Logger struct {
	w        io.Writer
	disabled bool
	now      Clock
	mu       sync.Mutex
}

// Span represents one started progress operation.
type Span struct {
	logger  *Logger
	command string
	op      string
	target  string
	started time.Time
	done    bool
}

// New returns a logger that writes progress to w unless disabled is true.
func New(w io.Writer, disabled bool, now Clock) *Logger {
	if now == nil {
		now = time.Now
	}
	return &Logger{w: w, disabled: disabled, now: now}
}

// Start writes a start line and returns a span that can be ended exactly once.
func (l *Logger) Start(command, op, target string) *Span {
	span := &Span{
		logger:  l,
		command: command,
		op:      op,
		target:  target,
		started: l.now(),
	}
	l.writeLine(progressLine{
		event:   "start",
		command: command,
		op:      op,
		target:  target,
	})
	return span
}

// End writes a finish or error line. It is a no-op for disabled loggers or a
// span that has already ended.
func (s *Span) End(err error) {
	if s == nil || s.logger == nil || s.done || s.logger.disabled {
		return
	}
	s.done = true
	dur := durationMS(s.logger.now().Sub(s.started))
	line := progressLine{
		command:    s.command,
		op:         s.op,
		target:     s.target,
		durationMS: dur,
	}
	if err != nil {
		line.event = "error"
		line.status = "error"
		line.errSummary = sanitizeErrorSummary(err.Error())
	} else {
		line.event = "finish"
		line.status = "ok"
	}
	s.logger.writeLine(line)
}

type progressLine struct {
	event      string
	command    string
	op         string
	target     string
	durationMS int64
	status     string
	errSummary string
}

func (l *Logger) writeLine(line progressLine) {
	if l == nil || l.disabled || l.w == nil {
		return
	}
	var b strings.Builder
	b.WriteString("cr progress")
	if line.event != "" {
		b.WriteString(" event=")
		b.WriteString(line.event)
	}
	if line.command != "" {
		b.WriteString(" command=")
		b.WriteString(quoteValue(line.command))
	}
	if line.op != "" {
		b.WriteString(" op=")
		b.WriteString(quoteValue(line.op))
	}
	if line.target != "" {
		b.WriteString(" target=")
		b.WriteString(quoteValue(line.target))
	}
	if line.durationMS > 0 || line.event == "finish" || line.event == "error" {
		b.WriteString(" duration_ms=")
		b.WriteString(strconv.FormatInt(line.durationMS, 10))
	}
	if line.status != "" {
		b.WriteString(" status=")
		b.WriteString(line.status)
	}
	if line.errSummary != "" {
		b.WriteString(" error=")
		b.WriteString(quoteValue(line.errSummary))
	}
	b.WriteByte('\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, b.String())
}

func quoteValue(value string) string {
	var b strings.Builder
	b.Grow(len(value) + 2)
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\', '"', '=':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func sanitizeErrorSummary(err string) string {
	err = strings.ReplaceAll(err, "\r", " ")
	err = strings.ReplaceAll(err, "\n", " ")
	err = strings.TrimSpace(err)
	if err == "" {
		return "error"
	}
	err = redactPathLikeSegments(err)
	if len(err) > 160 {
		err = err[:157] + "..."
	}
	return err
}

func redactPathLikeSegments(s string) string {
	parts := strings.Fields(s)
	for i, part := range parts {
		if strings.Contains(part, "/") || strings.Contains(part, `\`) {
			parts[i] = "[path]"
		}
	}
	return strings.Join(parts, " ")
}

func durationMS(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}
