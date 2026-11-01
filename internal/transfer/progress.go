package transfer

import (
	"context"
	"io"
)

// ByteProgress is called with the number of content bytes moved so far for
// the current file. It may be called often and from transfer goroutines.
type ByteProgress func(done int64)

type progressKey struct{}

// WithProgress returns a context whose transfers report byte progress.
func WithProgress(ctx context.Context, fn ByteProgress) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

func progressOf(ctx context.Context) ByteProgress {
	fn, _ := ctx.Value(progressKey{}).(ByteProgress)
	return fn
}

// countingReader reports bytes read and stops when the context ends.
type countingReader struct {
	ctx context.Context
	r   io.Reader
	n   int64
	fn  ByteProgress
}

func (c *countingReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.r.Read(b)
	if n > 0 && c.fn != nil {
		c.n += int64(n)
		c.fn(c.n)
	}
	return n, err
}

// countingWriter reports bytes written and stops when the context ends.
type countingWriter struct {
	ctx context.Context
	w   io.Writer
	n   int64
	fn  ByteProgress
}

func (c *countingWriter) Write(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := c.w.Write(b)
	if n > 0 && c.fn != nil {
		c.n += int64(n)
		c.fn(c.n)
	}
	return n, err
}
