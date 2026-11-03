package importer

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// NewV7At is a UUIDv7 (RFC 9562) whose 48-bit timestamp is t, not now, so
// an imported row sorts by its ORIGINAL creation time like a row the server
// made at that time. The other 74 bits are random. Like the database's
// uuidv7(), ids made in the same millisecond are not ordered among
// themselves.
func NewV7At(t time.Time) (uuid.UUID, error) {
	return newV7At(t, rand.Reader)
}

func newV7At(t time.Time, r io.Reader) (uuid.UUID, error) {
	ms := t.UnixMilli()
	if ms < 0 || ms >= 1<<48 {
		return uuid.Nil, fmt.Errorf("uuidv7: time %s is outside the 48-bit millisecond range", t.UTC().Format(time.RFC3339))
	}
	var u uuid.UUID
	if _, err := io.ReadFull(r, u[6:]); err != nil {
		return uuid.Nil, fmt.Errorf("uuidv7: random bits: %w", err)
	}
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(ms))
	copy(u[0:6], ts[2:8])
	u[6] = (u[6] & 0x0f) | 0x70 // version 7
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return u, nil
}

// V7Time is the millisecond timestamp inside a UUIDv7.
func V7Time(u uuid.UUID) time.Time {
	var ts [8]byte
	copy(ts[2:8], u[0:6])
	return time.UnixMilli(int64(binary.BigEndian.Uint64(ts[:]))).UTC()
}
