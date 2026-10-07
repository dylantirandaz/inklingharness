package ranges

import "sort"

// Normalize returns the sorted union of the input ranges.
func Normalize(input []Range) ([]Range, error) {
	sort.Slice(input, func(i, j int) bool { return input[i].Start < input[j].Start })
	out := make([]Range, 0, len(input))
	for _, r := range input {
		if r.Start >= r.End { return nil, ErrRange }
		if len(out) == 0 || r.Start > out[len(out)-1].End+1 {
			out = append(out, r)
		} else {
			out[len(out)-1].End = r.End
		}
	}
	return out, nil
}
