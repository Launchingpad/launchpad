package move

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"strings"
)

// PostgreSQL COPY text format: a row is one line, fields are separated by a
// tab, a field `\N` is NULL, and backslash escapes keep tab, newline and
// backslash out of the data. So a raw 0x0A always ends a row, and a raw 0x09
// always ends a field. Only the fields this package changes are unescaped.

var nullField = []byte(`\N`)

// unescapeCopy reverses COPY text escaping of one field.
func unescapeCopy(b []byte) []byte {
	if bytes.IndexByte(b, '\\') < 0 {
		return append([]byte(nil), b...)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c != '\\' || i+1 >= len(b) {
			out = append(out, c)
			continue
		}
		i++
		switch d := b[i]; d {
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'v':
			out = append(out, '\v')
		case '0', '1', '2', '3', '4', '5', '6', '7':
			v := int(d - '0')
			for k := 0; k < 2 && i+1 < len(b) && b[i+1] >= '0' && b[i+1] <= '7'; k++ {
				i++
				v = v*8 + int(b[i]-'0')
			}
			out = append(out, byte(v))
		case 'x':
			v, n := 0, 0
			for n < 2 && i+1 < len(b) && isHex(b[i+1]) {
				i++
				v = v*16 + hexVal(b[i])
				n++
			}
			if n == 0 {
				out = append(out, 'x')
			} else {
				out = append(out, byte(v))
			}
		default:
			out = append(out, d)
		}
	}
	return out
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func hexVal(c byte) int {
	switch {
	case c >= 'a':
		return int(c-'a') + 10
	case c >= 'A':
		return int(c-'A') + 10
	}
	return int(c - '0')
}

// escapeCopy applies COPY text escaping to one field.
func escapeCopy(b []byte) []byte {
	var out bytes.Buffer
	for _, c := range b {
		switch c {
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\v':
			out.WriteString(`\v`)
		default:
			out.WriteByte(c)
		}
	}
	return out.Bytes()
}

// byteaFromField reads a bytea field in text output form (`\x6162`).
func byteaFromField(raw []byte) ([]byte, error) {
	s := unescapeCopy(raw)
	if !bytes.HasPrefix(s, []byte(`\x`)) {
		return nil, fmt.Errorf("move: not a hex bytea")
	}
	return hex.DecodeString(string(s[2:]))
}

// byteaToField writes a bytea as a COPY text field.
func byteaToField(b []byte) []byte {
	return escapeCopy([]byte(`\x` + hex.EncodeToString(b)))
}

// row is one COPY text line split into raw (still escaped) fields.
type row struct {
	cols   map[string]int
	fields [][]byte
}

func newRow(cols map[string]int, line []byte) (*row, error) {
	f := bytes.Split(line, []byte{'\t'})
	if len(f) != len(cols) {
		return nil, fmt.Errorf("%w: a row has %d fields, expected %d", ErrBundleCorrupt, len(f), len(cols))
	}
	return &row{cols: cols, fields: f}, nil
}

// isNull reports whether the column is NULL.
func (r *row) isNull(col string) bool { return bytes.Equal(r.fields[r.cols[col]], nullField) }

// text returns the unescaped value of a text/uuid/boolean column.
func (r *row) text(col string) string { return string(unescapeCopy(r.fields[r.cols[col]])) }

// bytea returns the bytes of a non-NULL bytea column.
func (r *row) bytea(col string) ([]byte, error) { return byteaFromField(r.fields[r.cols[col]]) }

func (r *row) setNull(col string)            { r.fields[r.cols[col]] = nullField }
func (r *row) setText(col, v string)         { r.fields[r.cols[col]] = escapeCopy([]byte(v)) }
func (r *row) setBytea(col string, v []byte) { r.fields[r.cols[col]] = byteaToField(v) }

func (r *row) bytes() []byte { return bytes.Join(r.fields, []byte{'\t'}) }

func colIndex(cols []string) map[string]int {
	m := make(map[string]int, len(cols))
	for i, c := range cols {
		m[c] = i
	}
	return m
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
