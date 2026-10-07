package frames

import "errors"

var ErrFrame = errors.New("invalid frame")
var ErrTruncated = errors.New("truncated frame")

// Frames use decimal byte length, a colon, UTF-8 payload, and a comma.
// A zero-length payload is valid. Leading zeroes in the length are valid.
type Decoder struct {
	max int
	pending []byte
	failed error
}

// New sets the largest allowed payload length in bytes. max is nonnegative.
func New(max int) *Decoder { return &Decoder{max: max} }
