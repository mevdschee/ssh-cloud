// Package logging writes SSH-Cloud's debug log to a file in the settings
// directory. All packages log through log/slog; this package only sets up
// the destination. Crashes (unrecovered panics) are written to the same file.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
)

// maxSize is the size at which the log is rotated to ssh-cloud.log.old.
const maxSize = 5 << 20

// Setup opens <dir>/ssh-cloud.log at debug level and makes it the default
// slog logger. It returns the log file path and a close function.
func Setup(dir string) (string, func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	file := filepath.Join(dir, "ssh-cloud.log")
	if fi, err := os.Stat(file); err == nil && fi.Size() > maxSize {
		_ = os.Rename(file, file+".old")
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", nil, err
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})))
	// Fatal panics bypass slog; send them to the same file.
	_ = debug.SetCrashOutput(f, debug.CrashOptions{})
	return file, func() { f.Close() }, nil
}

// Recover logs a panic with its stack. Use as: defer logging.Recover("what").
// The panic is swallowed, so use it only where continuing is safe.
func Recover(where string) {
	if r := recover(); r != nil {
		slog.Error("panic recovered", "where", where, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
	}
}

// Discard silences logging (for tests or tools that do not want a log).
func Discard() { slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil))) }
