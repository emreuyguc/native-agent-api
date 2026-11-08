// Package iox holds small io helpers shared by the gateway packages.
package iox

import "bytes"

// TailBuffer is an io.Writer that keeps only the last Limit bytes written.
// It never returns an error, so it is safe to use as a child process sink.
// A zero or negative Limit discards everything.
type TailBuffer struct {
	Limit  int
	buffer bytes.Buffer
}

// NewTailBuffer returns a TailBuffer that keeps at most limit bytes.
func NewTailBuffer(limit int) *TailBuffer { return &TailBuffer{Limit: limit} }

// Write implements io.Writer.
func (b *TailBuffer) Write(data []byte) (int, error) {
	original := len(data)
	if b.Limit <= 0 {
		return original, nil
	}
	if len(data) >= b.Limit {
		b.buffer.Reset()
		b.buffer.Write(data[len(data)-b.Limit:])

		return original, nil
	}
	if overflow := b.buffer.Len() + len(data) - b.Limit; overflow > 0 {
		b.buffer.Next(overflow)
	}
	b.buffer.Write(data)

	return original, nil
}

// String returns the retained bytes.
func (b *TailBuffer) String() string { return b.buffer.String() }
