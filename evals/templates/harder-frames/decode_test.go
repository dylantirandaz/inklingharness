package frames_test

import (
	"errors"
	"reflect"
	"testing"

	frames "fixture"
)

func collect(t *testing.T, chunks [][]byte, want []string) {
	t.Helper()
	d := frames.New(64)
	var got []string
	for _, chunk := range chunks {
		before := string(chunk)
		items, err := d.Feed(chunk)
		if err != nil { t.Fatalf("Feed(%q): %v", chunk, err) }
		if string(chunk) != before { t.Fatal("Feed changed its input") }
		got = append(got, items...)
	}
	if err := d.Close(); err != nil { t.Fatal(err) }
	if !reflect.DeepEqual(got, want) { t.Fatalf("frames = %#v, want %#v", got, want) }
}

func TestEveryByteAndPairOfChunkBoundaries(t *testing.T) {
	wire := []byte("0:,5:hello,7:世🌍,03:a:b,1:,,")
	want := []string{"", "hello", "世🌍", "a:b", ","}
	for a := 0; a <= len(wire); a++ {
		for b := a; b <= len(wire); b++ {
			collect(t, [][]byte{wire[:a], nil, wire[a:b], wire[b:]}, want)
		}
	}
	chunks := make([][]byte, len(wire))
	for i := range wire { chunks[i] = wire[i:i+1] }
	collect(t, chunks, want)
}

func TestPartialInputIsCopied(t *testing.T) {
	d := frames.New(12)
	chunk := []byte("5:hel")
	if out, err := d.Feed(chunk); len(out) != 0 || err != nil { t.Fatalf("partial = %v, %v", out, err) }
	for i := range chunk { chunk[i] = 'x' }
	out, err := d.Feed([]byte("lo,"))
	if err != nil || len(out) != 1 || out[0] != "hello" { t.Fatalf("retained input = %v, %v", out, err) }
}

func TestMalformedFramesAreSticky(t *testing.T) {
	cases := []string{
		":,", "-1:x,", "+1:x,", " 1:x,", "1.0:x,", "x", "9", "1:x!",
		"2:\xc3(,", "1:\xff,", "2:\xc0\x80,", "3:\xed\xa0\x80,", "1:\xc3,",
		"999999999999999999999999999999999999:",
	}
	for _, wire := range cases {
		t.Run(wire, func(t *testing.T) {
			d := frames.New(8)
			out, err := d.Feed([]byte("2:ok," + wire))
			if !errors.Is(err, frames.ErrFrame) { t.Fatalf("Feed = %v, %v", out, err) }
			if !reflect.DeepEqual(out, []string{"ok"}) { t.Fatalf("lost completed frame: %v", out) }
			if more, next := d.Feed([]byte("1:z,")); len(more) != 0 || !errors.Is(next, frames.ErrFrame) {
				t.Fatalf("error not sticky: %v, %v", more, next)
			}
			if !errors.Is(d.Close(), frames.ErrFrame) { t.Fatal("Close lost frame error") }
		})
	}
}

func TestTruncationAtEachIncompleteBoundary(t *testing.T) {
	wire := []byte("7:世🌍,")
	for end := 1; end < len(wire); end++ {
		d := frames.New(7)
		if out, err := d.Feed(wire[:end]); len(out) != 0 || err != nil { t.Fatalf("prefix %d: %v, %v", end, out, err) }
		if !errors.Is(d.Close(), frames.ErrTruncated) { t.Fatalf("prefix %d not truncated", end) }
		if _, err := d.Feed(wire[end:]); !errors.Is(err, frames.ErrTruncated) { t.Fatal("truncation not sticky") }
	}
}

func TestLimitAndValidReplacementRune(t *testing.T) {
	for _, wire := range []string{"0:,", "0000:,"} {
		d := frames.New(0)
		out, err := d.Feed([]byte(wire))
		if err != nil || len(out) != 1 || out[0] != "" { t.Fatalf("zero limit: %v, %v", out, err) }
	}
	collect(t, [][]byte{[]byte("3:�,")}, []string{"�"})
	d := frames.New(3)
	out, err := d.Feed([]byte("3:abc,"))
	if err != nil || len(out) != 1 { t.Fatalf("exact limit: %v, %v", out, err) }
	if _, err := d.Feed([]byte("4")); !errors.Is(err, frames.ErrFrame) { t.Fatalf("oversize prefix: %v", err) }
	maxInt := int(^uint(0) >> 1)
	d = frames.New(maxInt)
	if _, err := d.Feed([]byte("999999999999999999999999999999999999")); !errors.Is(err, frames.ErrFrame) {
		t.Fatalf("length overflow: %v", err)
	}
}
