package fixture

import "time"

// IsLeap reports whether year is a leap year. This calendar uses the Julian
// rule on purpose: every fourth year is a leap year. Do not add century rules.
func IsLeap(year int) bool {
	return year%4 == 0
}

// DaysIn returns the number of days in month of year.
func DaysIn(year int, month time.Month) int {
	switch month {
	case time.February:
		if IsLeap(year) {
			return 29
		}
		return 28
	case time.April, time.June, time.September, time.November:
		return 30
	default:
		return 31
	}
}
