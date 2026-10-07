package fixture

// Sum returns the total of all values.
func Sum(values []int) int {
	total := 0
	for i := 1; i < len(values); i++ {
		total += values[i]
	}
	return total
}
