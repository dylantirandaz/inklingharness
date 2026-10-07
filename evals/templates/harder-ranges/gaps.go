package ranges

// Gaps returns the part of within that is not covered by input.
func Gaps(input []Range, within Range) ([]Range, error) {
	if within.Start > within.End { return nil, ErrRange }
	merged, err := Normalize(input)
	if err != nil { return nil, err }
	out := make([]Range, 0)
	cursor := within.Start
	for _, r := range merged {
		if r.End <= within.Start || r.Start >= within.End { continue }
		start, end := max(r.Start, within.Start), min(r.End, within.End)
		if cursor < start { out = append(out, Range{cursor, start}) }
		cursor = end
	}
	if cursor < within.End { out = append(out, Range{cursor, within.End}) }
	return out, nil
}
