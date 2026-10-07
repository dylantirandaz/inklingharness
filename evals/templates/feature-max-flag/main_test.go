package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	cases := []struct {
		args       []string
		wantOut    string
		wantErr    string
		wantStatus int
	}{
		{nil, "1\ta\n2\tb\n3\tc\n", "", 0},
		{[]string{"-start", "10"}, "10\ta\n11\tb\n12\tc\n", "", 0},
		{[]string{"-max", "2"}, "1\ta\n2\tb\n", "", 0},
		{[]string{"-max", "0"}, "1\ta\n2\tb\n3\tc\n", "", 0},
		{[]string{"-max", "5"}, "1\ta\n2\tb\n3\tc\n", "", 0},
		{[]string{"-start", "7", "-max", "1"}, "7\ta\n", "", 0},
		{[]string{"-max", "-1"}, "", "numbered: -max must not be negative\n", 2},
	}
	for _, c := range cases {
		var stdout, stderr bytes.Buffer
		status := run(c.args, strings.NewReader("a\nb\nc\n"), &stdout, &stderr)
		if status != c.wantStatus || stdout.String() != c.wantOut || stderr.String() != c.wantErr {
			t.Errorf("run(%q) = %d, stdout %q, stderr %q; want %d, %q, %q", c.args, status, stdout.String(), stderr.String(), c.wantStatus, c.wantOut, c.wantErr)
		}
	}
}
