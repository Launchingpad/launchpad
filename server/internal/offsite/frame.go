package offsite

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"devops/internal/backup"
)

// The fetch frame (decisions-T16 #39): what `server offsite fetch` writes to stdout
// and `server offsite unpack` reads from stdin.
//
//	version word      4 bytes, big endian, = FrameVersion
//	manifest length   8 bytes, big endian
//	manifest          that many bytes
//	object length     8 bytes, big endian
//	object            that many bytes
//	(end of stream: any further byte is refused)
//
// The frame carries no file name, link or path: unpack writes two fixed names.
const FrameVersion uint32 = 1

// MaxObjectBytes is the object cap of v1 (5 GiB, #40).
const MaxObjectBytes int64 = 5 << 30

// Fixed output names of unpack.
const (
	UnpackObject   = "object"
	UnpackManifest = "manifest.json"
)

// unpackSlack is the free space unpack wants beyond the object (#39).
const unpackSlack int64 = 1 << 30

// Errors of the frame. Fixed texts; none carries stream content.
var (
	ErrFrameVersion  = errors.New("offsite: the frame has a wrong version")
	ErrFrameManifest = errors.New("offsite: the frame's manifest is too large")
	ErrFrameObject   = errors.New("offsite: the frame's object is larger than the limit")
	ErrFrameShort    = errors.New("offsite: the frame is shorter than its lengths say")
	ErrFrameTrailing = errors.New("offsite: the frame has bytes after the object")
	ErrFrameNoSpace  = errors.New("offsite: not enough free space for the object")
)

// WriteFrame writes a frame: the manifest, then exactly objectLen bytes of obj.
// A short or long obj is an error (the reader of the stream would see a bad frame).
func WriteFrame(w io.Writer, manifest []byte, objectLen int64, obj io.Reader) error {
	if int64(len(manifest)) > backup.MaxManifestBytes || objectLen < 0 || objectLen > MaxObjectBytes {
		return ErrFrameObject
	}
	var h [12]byte
	binary.BigEndian.PutUint32(h[:4], FrameVersion)
	binary.BigEndian.PutUint64(h[4:], uint64(len(manifest)))
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	if _, err := w.Write(manifest); err != nil {
		return err
	}
	var l [8]byte
	binary.BigEndian.PutUint64(l[:], uint64(objectLen))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}
	n, err := io.Copy(w, io.LimitReader(obj, objectLen))
	if err != nil {
		return err
	}
	if n != objectLen {
		return ErrFrameShort
	}
	var one [1]byte
	if m, _ := obj.Read(one[:]); m > 0 {
		return ErrFrameTrailing // the remote sent more than it declared
	}
	return nil
}

// Unpack reads one frame from r and writes <out>/object and <out>/manifest.json
// (mode 0600, created new). It refuses a wrong version, a manifest above 1 MiB, an
// object above maxObject (and above MaxObjectBytes), a length that does not match
// the bytes, trailing bytes, and a file system without room for the declared object
// plus 1 GiB. On any failure it removes what it wrote. free reports the free bytes
// of a directory (nil: the real one).
func Unpack(r io.Reader, out string, maxObject int64, free func(string) (uint64, error)) (err error) {
	if maxObject <= 0 || maxObject > MaxObjectBytes {
		maxObject = MaxObjectBytes
	}
	if free == nil {
		free = freeBytes
	}
	br := bufio.NewReaderSize(r, 64<<10)
	var h [12]byte
	if _, err := io.ReadFull(br, h[:]); err != nil {
		return ErrFrameShort
	}
	if binary.BigEndian.Uint32(h[:4]) != FrameVersion {
		return ErrFrameVersion
	}
	mlen := binary.BigEndian.Uint64(h[4:])
	if mlen > uint64(backup.MaxManifestBytes) {
		return ErrFrameManifest
	}
	manifest := make([]byte, mlen)
	if _, err := io.ReadFull(br, manifest); err != nil {
		return ErrFrameShort
	}
	var l [8]byte
	if _, err := io.ReadFull(br, l[:]); err != nil {
		return ErrFrameShort
	}
	olen := binary.BigEndian.Uint64(l[:])
	if olen > uint64(maxObject) {
		return ErrFrameObject
	}
	if f, ferr := free(out); ferr != nil || f < olen+uint64(unpackSlack) {
		return ErrFrameNoSpace
	}
	mp, op := filepath.Join(out, UnpackManifest), filepath.Join(out, UnpackObject)
	var created []string // only files this call made are removed on failure
	defer func() {
		if err != nil {
			for _, p := range created {
				_ = os.Remove(p)
			}
		}
	}()
	if err := writeNew(mp, &created, func(f *os.File) error { _, e := f.Write(manifest); return e }); err != nil {
		return err
	}
	if err := writeNew(op, &created, func(f *os.File) error {
		n, e := io.Copy(f, io.LimitReader(br, int64(olen)))
		if e != nil {
			return e
		}
		if uint64(n) != olen {
			return ErrFrameShort
		}
		return nil
	}); err != nil {
		return err
	}
	if _, e := br.ReadByte(); e != io.EOF {
		return ErrFrameTrailing
	}
	return nil
}

func writeNew(path string, created *[]string, fill func(*os.File) error) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("offsite: cannot create an output file: %w", err)
	}
	*created = append(*created, path)
	if err := fill(f); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
