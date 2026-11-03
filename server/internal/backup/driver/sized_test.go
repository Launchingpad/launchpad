package driver

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
)

// zeroNilReader returns (0, nil) on every other call, as io.Reader allows.
type zeroNilReader struct {
	r    io.Reader
	flip bool
}

func (z *zeroNilReader) Read(b []byte) (int, error) {
	z.flip = !z.flip
	if z.flip {
		return 0, nil
	}
	return z.r.Read(b)
}

// TestSizedReader pins the Sized reader on its own: an exact source ends with io.EOF
// and all its bytes; a short one fails; a long one always fails with ErrSourceLong
// and the last declared byte is never handed over (LAUN-156, LAUN-158).
func TestSizedReader(t *testing.T) {
	wraps := map[string]func(io.Reader) io.Reader{
		"plain":   func(r io.Reader) io.Reader { return r },
		"onebyte": iotest.OneByteReader,
		"dataerr": iotest.DataErrReader,
		"half":    iotest.HalfReader,
		"zeronil": func(r io.Reader) io.Reader { return &zeroNilReader{r: r} },
	}
	sizes := []int64{1, 2, 3, 511, 512, 513, 4095, 4096, 4097, 32768, 32769}
	bufs := []int{1, 2, 7, 512, 4096, 32768, 100000}
	for _, size := range sizes {
		for _, delta := range []int64{-1, 0, 1, 5} {
			if size+delta < 0 {
				continue
			}
			data := make([]byte, size+delta)
			for i := range data {
				data[i] = byte(i*7 + 3)
			}
			for wn, wrap := range wraps {
				for _, bn := range bufs {
					name := fmt.Sprintf("size%d/delta%d/%s/buf%d", size, delta, wn, bn)
					s := Sized(wrap(bytes.NewReader(data)), size)
					buf := make([]byte, bn)
					var got []byte
					var err error
					for i := 0; i < 10_000_000; i++ {
						var n int
						n, err = s.Read(buf)
						got = append(got, buf[:n]...)
						if err != nil {
							break
						}
					}
					switch {
					case delta == 0:
						if err != io.EOF || !bytes.Equal(got, data) {
							t.Fatalf("%s: exact source: err %v, %d of %d bytes", name, err, len(got), len(data))
						}
					case delta < 0:
						if err == nil || err == io.EOF || errors.Is(err, ErrSourceLong) || !bytes.Equal(got, data[:len(got)]) {
							t.Fatalf("%s: short source: err %v", name, err)
						}
					default:
						if !errors.Is(err, ErrSourceLong) {
							t.Fatalf("%s: long source: err %v", name, err)
						}
						if int64(len(got)) >= size || !bytes.Equal(got, data[:len(got)]) {
							t.Fatalf("%s: long source: %d bytes handed over for a declared size of %d", name, len(got), size)
						}
					}
				}
			}
		}
	}
}

// TestSizedErrorMapsToServerError: the long-source error is a plain error that the
// shared mapper turns into the fixed server_error code, with no extra text.
func TestSizedErrorMapsToServerError(t *testing.T) {
	err := fmt.Errorf("Put: %w", ErrSourceLong)
	if c := CodeOf(err); c != ServerError {
		t.Fatalf("code %q", c)
	}
	if got := MapNetError(err).Error(); got != "server_error: the server returned an error" {
		t.Fatalf("text %q", got)
	}
}

// A source error that is not EOF passes through unchanged, on the look-ahead too.
func TestSizedPassesSourceErrors(t *testing.T) {
	boom := errors.New("boom")
	r := Sized(io.MultiReader(bytes.NewReader([]byte("abc")), iotest.ErrReader(boom)), 3)
	var got []byte
	var err error
	buf := make([]byte, 10)
	for err == nil {
		var n int
		n, err = r.Read(buf)
		got = append(got, buf[:n]...)
	}
	if !errors.Is(err, boom) || len(got) >= 3 {
		t.Fatalf("err %v, %d bytes", err, len(got))
	}
}

// errAfter hands over n bytes together with a non-EOF error once, then reads on.
type errAfter struct {
	r   io.Reader
	n   int
	err error
	hit bool
}

func (e *errAfter) Read(b []byte) (int, error) {
	if !e.hit {
		e.hit = true
		m, _ := io.ReadFull(e.r, b[:min(len(b), e.n)])
		return m, e.err
	}
	return e.r.Read(b)
}

// A read past the declared size (after the declared bytes came with an error) never
// hands a further byte over.
func TestSizedReadPastTheSizeIsRefused(t *testing.T) {
	tmp := errors.New("temporary")
	r := Sized(&errAfter{r: bytes.NewReader([]byte("abcd")), n: 3, err: tmp}, 3)
	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if n != 3 || !errors.Is(err, tmp) {
		t.Fatalf("first read: %d, %v", n, err)
	}
	if n, err = r.Read(buf); n != 0 || !errors.Is(err, ErrSourceLong) {
		t.Fatalf("read past the size: %d, %v", n, err)
	}
}

func TestNoMore(t *testing.T) {
	boom := errors.New("boom")
	if err := NoMore(bytes.NewReader(nil)); err != nil {
		t.Fatal(err)
	}
	if err := NoMore(&zeroNilReader{r: bytes.NewReader(nil)}); err != nil {
		t.Fatal(err)
	}
	for name, r := range map[string]io.Reader{
		"plain":   bytes.NewReader([]byte("x")),
		"zeronil": &zeroNilReader{r: bytes.NewReader([]byte("x"))},
		"dataerr": iotest.DataErrReader(bytes.NewReader([]byte("x"))),
	} {
		if err := NoMore(r); !errors.Is(err, ErrSourceLong) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := NoMore(iotest.ErrReader(boom)); !errors.Is(err, boom) {
		t.Fatal(err)
	}
}
