package tools

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// BenchmarkOutputCollector collects one command output in 4 KiB pipe
// chunks. The sizes are the median and the p90 of the bash results of
// Inkling Small on SWE-bench (313 and 2,183 characters), and a large output
// that goes to a file.
func BenchmarkOutputCollector(b *testing.B) {
	for _, size := range []int{313, 2183, 100_000} {
		var text strings.Builder
		for line := 0; text.Len() < size; line++ {
			fmt.Fprintf(&text, "tests/test_a.py::test_case_%d PASSED [%3d%%]\n", line, line%100)
		}
		output := []byte(text.String()[:size])
		directory := b.TempDir()
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			b.SetBytes(int64(size))
			b.ReportAllocs()
			for b.Loop() {
				collector := newOutputCollector(directory, func(string) bool { return false }, standardLimits)
				for start := 0; start < len(output); start += 4096 {
					if _, err := collector.Write(output[start:min(start+4096, len(output))]); err != nil {
						b.Fatal(err)
					}
				}
				if collector.finish() == "" {
					b.Fatal("empty result")
				}
				// A session keeps few saved outputs; without the removal
				// the folder grows with b.N, and pruning reads all of it.
				if collector.saved != nil {
					if err := os.Remove(collector.saved.file.Name()); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
