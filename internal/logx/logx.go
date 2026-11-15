// Package logx provides the structured logging the core uses. Every record is
// one line of key/value pairs after a fixed header, so a production log can be
// grepped for one session or one failure instead of read in full, and a field
// added later never changes how an earlier line is parsed.
package logx

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

// Level orders how much detail a build writes.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
)

// ParseLevel maps a configured name onto a level; an unknown name is Info.
func ParseLevel(name string) Level {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error":
		return LevelError
	default:
		return LevelInfo
	}
}

// Logger writes one structured line per call. It is safe for concurrent use.
type Logger struct {
	mu    sync.Mutex
	level Level
	// sink receives a finished line. It defaults to the standard logger, so a
	// build that installs no logger still writes where it always did.
	sink func(string)
}

var defaultLogger = &Logger{level: LevelInfo, sink: standardSink}

// SetLevel changes the minimum level a record needs to be written.
func SetLevel(level Level) {
	defaultLogger.mu.Lock()
	defaultLogger.level = level
	defaultLogger.mu.Unlock()
}

// Level returns the current minimum level.
func LevelOf() Level {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	return defaultLogger.level
}

// SetSink replaces where finished lines go. Tests use it to capture output.
func SetSink(sink func(string)) {
	defaultLogger.mu.Lock()
	defer defaultLogger.mu.Unlock()
	if sink == nil {
		sink = standardSink
	}
	defaultLogger.sink = sink
}

// Field is one key/value pair on a record.
type Field struct {
	Key   string
	Value any
}

// F builds one field.
func F(key string, value any) Field { return Field{Key: key, Value: value} }

// Err records an error, leaving the field out when there is none.
func Err(err error) Field {
	if err == nil {
		return Field{Key: "", Value: nil}
	}
	return Field{Key: "error", Value: err.Error()}
}

// Session records which conversation a record belongs to.
func Session(id string) Field { return F("session", id) }

// Component records which part of the core wrote the record.
func Component(name string) Field { return F("component", name) }

// Debug writes a record only at the debug level.
func Debug(message string, fields ...Field) { defaultLogger.write(LevelDebug, message, fields) }

// Info writes an ordinary record.
func Info(message string, fields ...Field) { defaultLogger.write(LevelInfo, message, fields) }

// Warn writes a record that needs attention but did not stop work.
func Warn(message string, fields ...Field) { defaultLogger.write(LevelWarn, message, fields) }

// Error writes a record for a failure.
func Error(message string, fields ...Field) { defaultLogger.write(LevelError, message, fields) }

func (l *Logger) write(level Level, message string, fields []Field) {
	l.mu.Lock()
	min := l.level
	sink := l.sink
	l.mu.Unlock()
	if level < min {
		return
	}
	if sink == nil {
		sink = standardSink
	}
	sink(format(level, message, fields))
}

// standardSink writes a finished line through the standard logger, so a build
// that installs no logger keeps writing where it always did.
func standardSink(line string) { log.Print(line) }

// format renders one record. Fields are sorted so two runs of the same code
// write comparable lines, and a value with spaces is quoted so a reader can
// split the line on single spaces.
func format(level Level, message string, fields []Field) string {
	kept := make([]Field, 0, len(fields))
	for _, field := range fields {
		if field.Key == "" {
			continue
		}
		kept = append(kept, field)
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].Key < kept[j].Key })

	var b strings.Builder
	b.WriteString(levelName(level))
	b.WriteString(" ")
	b.WriteString(message)
	for _, field := range kept {
		b.WriteString(" ")
		b.WriteString(field.Key)
		b.WriteString("=")
		b.WriteString(value(field.Value))
	}
	return b.String()
}

func levelName(level Level) string {
	switch level {
	case LevelDebug:
		return "DEBUG"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// value renders one field value. Strings with a space or an equals sign are
// quoted; everything else uses its plain form.
func value(input any) string {
	switch typed := input.(type) {
	case nil:
		return "null"
	case string:
		return quoteIfNeeded(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case time.Duration:
		return typed.String()
	case time.Time:
		return typed.UTC().Format(time.RFC3339Nano)
	case error:
		return quoteIfNeeded(typed.Error())
	case fmt.Stringer:
		return quoteIfNeeded(typed.String())
	case int:
		return fmt.Sprintf("%d", typed)
	case int64:
		return fmt.Sprintf("%d", typed)
	case float64:
		return fmt.Sprintf("%g", typed)
	default:
		return quoteIfNeeded(fmt.Sprintf("%v", typed))
	}
}

func quoteIfNeeded(text string) string {
	if text == "" {
		return `""`
	}
	if strings.ContainsAny(text, " =\n\t\"") {
		return fmt.Sprintf("%q", text)
	}
	return text
}

// WithContext carries the session a record belongs to, so a call site deep in a
// request does not have to thread it through every function.
type contextKey struct{}

// NewContext attaches a session to a context.
func NewContext(ctx context.Context, sessionID string) context.Context {
	if sessionID == "" {
		return ctx
	}
	return context.WithValue(ctx, contextKey{}, sessionID)
}

// FromContext returns the session attached to a context, if any.
func FromContext(ctx context.Context) string {
	value, _ := ctx.Value(contextKey{}).(string)
	return value
}

// Timing measures how long one operation took, for a record written when it
// finishes.
func Timing(start time.Time) Field {
	return F("elapsed", time.Since(start).Round(time.Millisecond))
}
