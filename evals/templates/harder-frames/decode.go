package frames

import (
	"bytes"
	"strconv"
	"unicode/utf8"
)

// Feed consumes a chunk and returns each complete frame in order.
func (d *Decoder) Feed(chunk []byte) ([]string, error) {
	if d.failed != nil { return nil, d.failed }
	d.pending = append(d.pending, chunk...)
	var out []string
	for len(d.pending) > 0 {
		colon := bytes.IndexByte(d.pending, ':')
		if colon < 0 { return out, nil }
		n, err := strconv.Atoi(string(d.pending[:colon]))
		if err != nil || n < 0 || n > d.max {
			d.failed = ErrFrame
			return out, d.failed
		}
		body := d.pending[colon+1:]
		if len(body) <= n { return out, nil }
		if body[n] != ',' {
			d.failed = ErrFrame
			return out, d.failed
		}
		if !utf8.Valid(body[:n]) {
			out = append(out, string([]rune(string(body[:n]))))
		} else {
			out = append(out, string(body[:n]))
		}
		d.pending = body[n+1:]
	}
	return out, nil
}

// Close checks that the last frame is complete.
func (d *Decoder) Close() error {
	if d.failed != nil { return d.failed }
	return nil
}
