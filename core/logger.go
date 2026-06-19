package core

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// LogLevel is the minimum severity a logger will emit.
type LogLevel string

const (
	LevelDebug LogLevel = "debug"
	LevelInfo  LogLevel = "info"
	LevelWarn  LogLevel = "warn"
	LevelError LogLevel = "error"
)

var levelRank = map[LogLevel]int{LevelDebug: 10, LevelInfo: 20, LevelWarn: 30, LevelError: 40}

// Fields is a set of structured key/value pairs attached to a log line.
type Fields map[string]any

// Logger emits structured JSON log lines. info/debug go to stdout; warn/error
// go to stderr — matching the original implementation.
type Logger struct {
	threshold int
}

// NewLogger returns a Logger that drops anything below level.
func NewLogger(level LogLevel) *Logger {
	r, ok := levelRank[level]
	if !ok {
		r = levelRank[LevelInfo]
	}
	return &Logger{threshold: r}
}

func (l *Logger) emit(level LogLevel, msg string, fields Fields) {
	if levelRank[level] < l.threshold {
		return
	}
	entry := map[string]any{
		"ts":    time.Now().UTC().Format(time.RFC3339Nano),
		"level": string(level),
		"msg":   msg,
	}
	for k, v := range fields {
		entry[k] = v
	}
	line, err := json.Marshal(entry)
	if err != nil {
		line = []byte(fmt.Sprintf(`{"level":%q,"msg":%q}`, level, msg))
	}
	if level == LevelError || level == LevelWarn {
		fmt.Fprintln(os.Stderr, string(line))
	} else {
		fmt.Fprintln(os.Stdout, string(line))
	}
}

func (l *Logger) Debug(msg string, fields Fields) { l.emit(LevelDebug, msg, fields) }
func (l *Logger) Info(msg string, fields Fields)  { l.emit(LevelInfo, msg, fields) }
func (l *Logger) Warn(msg string, fields Fields)  { l.emit(LevelWarn, msg, fields) }
func (l *Logger) Error(msg string, fields Fields) { l.emit(LevelError, msg, fields) }
