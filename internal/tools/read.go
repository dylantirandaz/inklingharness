package tools

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Reserve room for terminal notices inside the result byte limit.
const outputBodyBytes = maxReadBytes - 256

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

// boundedLine consumes a line without retaining more than capacity bytes.
// EOF is returned only when there is no remaining line, including an empty one.
func boundedLine(ctx context.Context, reader *bufio.Reader, capacity int) (string, bool, error) {
	var line strings.Builder
	present, truncated := false, false
	for {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		part, err := reader.ReadSlice('\n')
		if len(part) > 0 {
			present = true
			if part[len(part)-1] == '\n' {
				part = part[:len(part)-1]
			}
			remaining := capacity - line.Len()
			if len(part) > remaining {
				truncated = true
				part = part[:remaining]
			}
			line.Write(part)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return "", false, err
		}
		if !present && errors.Is(err, io.EOF) {
			return "", false, io.EOF
		}
		return strings.TrimSuffix(line.String(), "\r"), truncated, nil
	}
}

func readLines(ctx context.Context, path string, offset, limit int) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return fileFailure(err)
	}
	defer file.Close()
	reader := bufio.NewReader(contextReader{ctx: ctx, reader: file})
	var output strings.Builder
	for number, shown := 1, 0; ; number++ {
		capacity := outputBodyBytes - output.Len() - 32
		if number < offset {
			capacity = 0
		}
		text, truncated, err := boundedLine(ctx, reader, capacity)
		if errors.Is(err, io.EOF) {
			notice := fmt.Sprintf("[end of file: %d lines]", number-1)
			if offset > 1 && offset >= number {
				notice = fmt.Sprintf("[end of file: %d lines; offset %d is beyond EOF]", number-1, offset)
			}
			return Result{Content: output.String() + notice}, nil
		}
		if err != nil {
			return fileFailure(err)
		}
		if number < offset {
			continue
		}
		fmt.Fprintf(&output, "%d: %s\n", number, text)
		shown++
		if truncated || output.Len() >= outputBodyBytes-32 {
			return Result{Content: output.String() + "[truncated: 256 KiB output limit; a line may be partial]"}, nil
		}
		if shown == limit {
			_, err := reader.Peek(1)
			if errors.Is(err, io.EOF) {
				return Result{Content: output.String() + fmt.Sprintf("[end of file: %d lines]", number)}, nil
			}
			if err != nil {
				return fileFailure(err)
			}
			return Result{Content: output.String() + fmt.Sprintf("[truncated: line limit; continue with offset %d]", number+1)}, nil
		}
	}
}
