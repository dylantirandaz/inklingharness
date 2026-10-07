package fixture

// Truncate shortens s to at most limit characters and adds "…" when it cuts.
func Truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}
