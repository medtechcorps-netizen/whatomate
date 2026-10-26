package storage

import (
	"context"
	"errors"
	"io"
	"math"
)

// ReadLimited reads at most maxBytes+1, including when Content-Length is absent
// or stale. The extra byte distinguishes exact-size success from truncation.
func ReadLimited(ctx context.Context, reader io.Reader, maxBytes int64) ([]byte, error) {
	if maxBytes <= 0 || maxBytes == math.MaxInt64 {
		return nil, errors.New("invalid object read limit")
	}
	data, err := io.ReadAll(io.LimitReader(contextReader{ctx, reader}, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, ErrObjectTooLarge
	}
	return data, ctx.Err()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
