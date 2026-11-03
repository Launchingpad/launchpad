package driver

import (
	"errors"
	"io"
)

// ErrSourceLong is the read error of a source that holds more than its declared
// size. It is a plain error with a fixed text; a driver maps it to ServerError
// (MapNetError does, as it does for any error it does not know) and never shows
// a remote's words with it.
var ErrSourceLong = errors.New("the source is longer than its declared size")

// sized is the reader behind Sized.
type sized struct {
	r    io.Reader
	left int64
}

// Sized wraps src for a transport that sends exactly size bytes (LAUN-156,
// LAUN-158). It refuses a source that ends before size (io.ErrUnexpectedEOF) AND
// one that holds more (ErrSourceLong). The long case is settled BEFORE the last
// declared byte is handed over: net/http probes for an extra body byte only after
// it has written the declared bytes, and a server that answers at once with those
// bytes can win that race, so a long source would "succeed" with a cut copy. The
// read that reaches the declared size also reads one byte ahead (io.ReadFull, so a
// (0, nil) read is retried); a byte there fails the request before the transport
// sees the final bytes. An exact source ends with all its bytes and io.EOF.
func Sized(src io.Reader, size int64) io.Reader { return &sized{r: src, left: size} }

func (s *sized) Read(b []byte) (int, error) {
	if s.left <= 0 {
		// Past the declared size: any further byte is too many.
		n, err := s.r.Read(b[:min(len(b), 1)])
		if n > 0 {
			return 0, ErrSourceLong
		}
		return 0, err
	}
	if int64(len(b)) > s.left {
		b = b[:s.left]
	}
	n, err := s.r.Read(b)
	s.left -= int64(n)
	if err == io.EOF && s.left > 0 {
		return n, io.ErrUnexpectedEOF
	}
	if s.left == 0 && n > 0 && err == nil {
		// The last declared bytes: look one byte ahead before returning them.
		var one [1]byte
		m, perr := io.ReadFull(s.r, one[:])
		if m > 0 {
			return 0, ErrSourceLong
		}
		if perr != nil && perr != io.EOF {
			return 0, perr
		}
		return n, io.EOF
	}
	return n, err
}

// NoMore reports whether src is empty, for a declared size of 0 (which sends no
// body and would never read the source). One byte there is ErrSourceLong; a read
// error is returned as it is; end of source is nil. A (0, nil) read is retried.
func NoMore(src io.Reader) error {
	var one [1]byte
	n, err := io.ReadFull(src, one[:])
	if n > 0 {
		return ErrSourceLong
	}
	if err != nil && err != io.EOF {
		return err
	}
	return nil
}
