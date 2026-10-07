package catalog

import "errors"

var ErrMissing = errors.New("missing catalog key")
var ErrClosed = errors.New("catalog closed")
