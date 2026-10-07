package fixture

import "testing"

func TestParseSize(t *testing.T) {
	valid := map[string]int64{
		"0":             0,
		"512":           512,
		"512B":          512,
		"10KiB":         10240,
		"1.5KiB":        1536,
		"0.5KiB":        512,
		"2MiB":          2097152,
		"1GiB":          1073741824,
		"8589934591GiB": 9223372035781033984,
	}
	for text, want := range valid {
		got, err := ParseSize(text)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v; want %d", text, got, err, want)
		}
	}
	invalid := []string{"", "-1", "+1", "1e3", "10kb", "10KIB", "10 KiB", " 10", "1.1B", "1.", ".5", "KiB", "1.5.5", "8589934592GiB", "99999999999999999999"}
	for _, text := range invalid {
		if got, err := ParseSize(text); err == nil {
			t.Errorf("ParseSize(%q) = %d, want an error", text, got)
		}
	}
}
