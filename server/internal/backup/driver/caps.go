package driver

import (
	"io"

	"devops/internal/backup/remotename"
)

// CheckName refuses a name a driver must not use as a remote file name. It is
// the driver's last defence against path traversal: a request URL or path is
// built only from a name that passed it.
func CheckName(name string) error {
	if !remotename.Valid(name) {
		return Errf(NotFound)
	}
	return nil
}

// ReadCapped reads all of r, up to max bytes. A longer body is too_large; the
// rest is not read.
func ReadCapped(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, MapNetError(err)
	}
	if int64(len(b)) > max {
		return nil, Errf(TooLarge)
	}
	return b, nil
}

// CapReader returns a reader that fails with too_large once more than max
// bytes were read.
func CapReader(r io.Reader, max int64) io.Reader { return &capReader{r: r, left: max} }

// DownloadCap is the cap of a download of a declared size.
func DownloadCap(declared int64) int64 {
	if declared < 0 {
		declared = 0
	}
	return declared + DownloadSlack
}

type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left < 0 {
		return 0, Errf(TooLarge)
	}
	if int64(len(p)) > c.left+1 {
		p = p[:c.left+1]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	if c.left < 0 {
		return n - int(-c.left), Errf(TooLarge)
	}
	return n, err
}
