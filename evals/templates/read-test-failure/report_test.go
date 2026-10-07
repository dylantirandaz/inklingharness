package fixture

import "testing"

func TestReport(t *testing.T) {
	got := Report([]float64{0, 100, -40})
	want := "0.0°C = 32.0°F\n100.0°C = 212.0°F\n-40.0°C = -40.0°F\n"
	if got != want {
		t.Fatalf("Report() =\n%s\nwant\n%s", got, want)
	}
}
