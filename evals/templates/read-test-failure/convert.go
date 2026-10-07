package fixture

// CelsiusToFahrenheit converts a temperature.
func CelsiusToFahrenheit(celsius float64) float64 {
	return (celsius + 32) * 9 / 5
}

// FahrenheitToCelsius converts a temperature.
func FahrenheitToCelsius(fahrenheit float64) float64 {
	return (fahrenheit - 32) * 5 / 9
}
