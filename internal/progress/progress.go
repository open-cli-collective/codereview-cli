// Package progress writes structured progress breadcrumbs to stderr for long-running CLI work.
package progress

import (
	"io"
	"sort"
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

// Field is one additional structured progress attribute.
type Field struct {
	Key   string
	Value string
}

// Span represents one started progress operation.
type Span struct {
	logger  *Logger
	command string
	op      string
	target  string
	fields  []Field
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
	return l.StartFields(command, op, target)
}

// StartFields writes a start line with additional structured fields and returns
// a span that can be ended exactly once.
func (l *Logger) StartFields(command, op, target string, fields ...Field) *Span {
	span := &Span{
		logger:  l,
		command: command,
		op:      op,
		target:  target,
		fields:  cloneFields(fields),
		started: l.now(),
	}
	l.writeLine(progressLine{
		event:   "start",
		command: command,
		op:      op,
		target:  target,
		fields:  span.fields,
	})
	return span
}

// InfoFields writes one instantaneous structured progress record.
func (l *Logger) InfoFields(command, op, target string, fields ...Field) {
	l.writeLine(progressLine{
		event:   "info",
		command: command,
		op:      op,
		target:  target,
		fields:  cloneFields(fields),
	})
}

// End writes a finish or error line and returns err. It is a no-op for disabled
// loggers or a span that has already ended.
func (s *Span) End(err error) error {
	s.EndFields(err)
	return err
}

// EndFields writes a finish or error line with additional structured fields.
// It is a no-op for disabled loggers or a span that has already ended.
func (s *Span) EndFields(err error, fields ...Field) {
	if s == nil || s.logger == nil || s.done || s.logger.disabled {
		return
	}
	s.done = true
	dur := durationMS(s.logger.now().Sub(s.started))
	line := progressLine{
		command:    s.command,
		op:         s.op,
		target:     s.target,
		fields:     mergeFields(s.fields, fields),
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
	fields     []Field
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
	for _, field := range normalizeFields(line.fields) {
		if field.Key == "" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(field.Key)
		b.WriteByte('=')
		b.WriteString(quoteValue(field.Value))
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

func cloneFields(fields []Field) []Field {
	if len(fields) == 0 {
		return nil
	}
	cloned := make([]Field, 0, len(fields))
	for _, field := range fields {
		key := strings.TrimSpace(field.Key)
		if key == "" {
			continue
		}
		cloned = append(cloned, Field{Key: key, Value: field.Value})
	}
	return cloned
}

func mergeFields(base, extra []Field) []Field {
	if len(base) == 0 && len(extra) == 0 {
		return nil
	}
	merged := cloneFields(base)
	if len(extra) == 0 {
		return merged
	}
	extraCloned := cloneFields(extra)
	if len(extraCloned) == 0 {
		return merged
	}
	byKey := make(map[string]int, len(merged))
	for i, field := range merged {
		byKey[field.Key] = i
	}
	for _, field := range extraCloned {
		if index, ok := byKey[field.Key]; ok {
			merged[index].Value = field.Value
			continue
		}
		byKey[field.Key] = len(merged)
		merged = append(merged, field)
	}
	return merged
}

func normalizeFields(fields []Field) []Field {
	if len(fields) == 0 {
		return nil
	}
	normalized := cloneFields(fields)
	sort.SliceStable(normalized, func(i, j int) bool {
		return normalized[i].Key < normalized[j].Key
	})
	return normalized
}
