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
// The returned bytes are valid until the next read from reader.
// EOF is returned only when there is no remaining line, including an empty one.
func boundedLine(ctx context.Context, reader *bufio.Reader, capacity int) ([]byte, bool, error) {
	var line []byte
	present, truncated := false, false
	for {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		part, err := reader.ReadSlice('\n')
		if len(part) > 0 {
			present = true
			if part[len(part)-1] == '\n' {
				part = part[:len(part)-1]
			}
			remaining := capacity - len(line)
			if len(part) > remaining {
				truncated = true
				part = part[:remaining]
			}
			if line == nil && err != bufio.ErrBufferFull {
				line = part
			} else {
				line = append(line, part...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, false, err
		}
		if !present && errors.Is(err, io.EOF) {
			return nil, false, io.EOF
		}
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		return line, truncated, nil
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
	if offset > 1 {
		fmt.Fprintf(&output, "[from line %d]\n", offset)
	}
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
			output.WriteString(notice)
			return Result{Content: output.String()}, nil
		}
		if err != nil {
			return fileFailure(err)
		}
		if number < offset {
			continue
		}
		output.Write(text)
		output.WriteByte('\n')
		shown++
		if truncated || output.Len() >= outputBodyBytes-32 {
			output.WriteString("[truncated: 256 KiB output limit; a line may be partial]")
			return Result{Content: output.String()}, nil
		}
		if shown == limit {
			_, err := reader.Peek(1)
			if errors.Is(err, io.EOF) {
				fmt.Fprintf(&output, "[end of file: %d lines]", number)
				return Result{Content: output.String()}, nil
			}
			if err != nil {
				return fileFailure(err)
			}
			fmt.Fprintf(&output, "[truncated: line limit; continue with offset %d]", number+1)
			return Result{Content: output.String()}, nil
		}
	}
}
