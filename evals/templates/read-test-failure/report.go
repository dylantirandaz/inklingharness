package fixture

import (
	"fmt"
	"strings"
)

// Report returns one line per reading in Celsius and Fahrenheit.
func Report(celsius []float64) string {
	var builder strings.Builder
	for _, reading := range celsius {
		fmt.Fprintf(&builder, "%.1f°C = %.1f°F\n", reading, CelsiusToFahrenheit(reading))
	}
	return builder.String()
}
