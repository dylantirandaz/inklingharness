package ranges

import "errors"

var ErrRange = errors.New("reversed range")

// Range contains integers x where Start <= x && x < End.
// Equal endpoints form an empty range.
type Range struct { Start, End int }
