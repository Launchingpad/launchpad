package move

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// magic starts the decrypted stream. A wrong-format file that happens to be
// an age file fails here, with ErrBundleCorrupt.
const magic = "LAUNCHPAD-MOVE-1\n"

// formatVersion is the bundle layout version (the frames below).
const formatVersion = 1

// Frame types. A frame is: 1 type byte, 4 bytes big-endian payload length,
// the payload.
const (
	frameManifest   byte = 'M' // JSON Manifest, always first
	frameTable      byte = 'T' // JSON tableHeader, opens a table
	frameData       byte = 'D' // COPY text rows (whole rows only)
	frameTableEnd   byte = 'E' // JSON tableEnd
	frameObject     byte = 'O' // JSON objectHeader, opens an object
	frameObjectData byte = 'B' // object bytes
	frameObjectEnd  byte = 'H' // 32 bytes sha256 of the object
	frameEnd        byte = 'Z' // end of the bundle, always last
)

// maxFrame bounds one payload read, so a damaged length cannot make the
// reader allocate without limit. Export writes frames of about dataChunk, but
// one row (a big document or event payload) can be larger.
const (
	maxFrame  = 256 << 20
	dataChunk = 256 << 10
)

type tableHeader struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
}

type tableEnd struct {
	Rows int64 `json:"rows"`
}

type objectHeader struct {
	Key         string `json:"key"`
	ContentType string `json:"contentType"`
	Size        int64  `json:"size"`
}

type frameWriter struct {
	w io.Writer
}

func (f *frameWriter) write(t byte, payload []byte) error {
	if len(payload) > maxFrame {
		return fmt.Errorf("move: a frame of %d bytes is above the limit", len(payload))
	}
	var h [5]byte
	h[0] = t
	binary.BigEndian.PutUint32(h[1:], uint32(len(payload)))
	if _, err := f.w.Write(h[:]); err != nil {
		return err
	}
	_, err := f.w.Write(payload)
	return err
}

func (f *frameWriter) writeJSON(t byte, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return f.write(t, b)
}

type frameReader struct {
	r io.Reader
}

// read returns the next frame. A short read anywhere is ErrBundleCorrupt.
func (f *frameReader) read() (byte, []byte, error) {
	var h [5]byte
	if _, err := io.ReadFull(f.r, h[:]); err != nil {
		return 0, nil, wrapRead(err)
	}
	n := binary.BigEndian.Uint32(h[1:])
	if n > maxFrame {
		return 0, nil, fmt.Errorf("%w: frame length", ErrBundleCorrupt)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(f.r, p); err != nil {
		return 0, nil, wrapRead(err)
	}
	return h[0], p, nil
}

// expect reads a frame and requires its type.
func (f *frameReader) expect(t byte) ([]byte, error) {
	got, p, err := f.read()
	if err != nil {
		return nil, err
	}
	if got != t {
		return nil, fmt.Errorf("%w: unexpected frame", ErrBundleCorrupt)
	}
	return p, nil
}

func (f *frameReader) expectJSON(t byte, v any) error {
	p, err := f.expect(t)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(p, v); err != nil {
		return fmt.Errorf("%w: frame body", ErrBundleCorrupt)
	}
	return nil
}

// wrapRead turns any failure of the decrypting reader (a cut file, a changed
// byte: age authenticates every chunk) into ErrBundleCorrupt. A context
// error passes through.
func wrapRead(err error) error {
	if errors.Is(err, errContext) {
		return err
	}
	return fmt.Errorf("%w", ErrBundleCorrupt)
}

// errContext marks a caller cancellation inside a reader; see ctxReader.
var errContext = errors.New("move: canceled")

func unmarshal(p []byte, v any) error {
	if err := json.Unmarshal(p, v); err != nil {
		return fmt.Errorf("%w: frame body", ErrBundleCorrupt)
	}
	return nil
}
