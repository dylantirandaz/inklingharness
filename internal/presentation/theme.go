package presentation

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"
)

// ColorDepth is the color capability of the output terminal.
type ColorDepth uint8

const (
	NoColor ColorDepth = iota
	ANSI16
	ANSI256
	TrueColor
)

// DetectColorDepth follows NO_COLOR, COLORTERM, and TERM. getenv is usually
// os.Getenv; a parameter keeps the decision testable without process state.
func DetectColorDepth(getenv func(string) string) ColorDepth {
	if getenv("NO_COLOR") != "" || getenv("TERM") == "dumb" {
		return NoColor
	}
	switch strings.ToLower(getenv("COLORTERM")) {
	case "truecolor", "24bit":
		return TrueColor
	}
	if strings.Contains(getenv("TERM"), "256color") {
		return ANSI256
	}
	return ANSI16
}

type rgb struct{ r, g, b uint8 }

// The Signal palette: the terminal background is the void, light is the
// accent. Each role has a 24-bit color and an ANSI 16 fallback.
type role uint8

const (
	roleIon role = iota
	roleNebula
	roleHaze
	roleVerdant
	roleEmber
	rolePlasma
	roleAmber
	roleFlare
	roleCount
)

var palette = [roleCount]struct {
	color  rgb
	ansi16 string
}{
	roleIon:     {rgb{94, 242, 232}, "36"},
	roleNebula:  {rgb{167, 139, 250}, "35"},
	roleHaze:    {rgb{122, 132, 168}, "90"},
	roleVerdant: {rgb{92, 255, 176}, "32"},
	roleEmber:   {rgb{255, 92, 122}, "31"},
	rolePlasma:  {rgb{255, 106, 213}, "95"},
	roleAmber:   {rgb{255, 198, 109}, "33"},
	roleFlare:   {rgb{232, 255, 254}, "96"},
}

// Theme holds the escape sequence of each role for one color depth. The zero
// value is NoColor: every method then returns its text unchanged, so callers
// never branch on color themselves.
type Theme struct {
	depth ColorDepth
	sgr   [roleCount]string
}

// NewTheme computes every role sequence once.
func NewTheme(depth ColorDepth) Theme {
	theme := Theme{depth: depth}
	if depth == NoColor {
		return theme
	}
	for index, entry := range palette {
		theme.sgr[index] = theme.foreground(entry.color, entry.ansi16)
	}
	return theme
}

// Colored reports whether the theme writes any escape sequence.
func (t Theme) Colored() bool { return t.depth != NoColor }

func (t Theme) foreground(color rgb, ansi16 string) string {
	switch t.depth {
	case TrueColor:
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", color.r, color.g, color.b)
	case ANSI256:
		return fmt.Sprintf("\x1b[38;5;%dm", nearest256(color))
	case ANSI16:
		return "\x1b[" + ansi16 + "m"
	case NoColor:
		return ""
	}
	panic(fmt.Sprintf("presentation: unknown color depth %d", t.depth))
}

func (t Theme) paint(r role, text string) string {
	if t.depth == NoColor || text == "" {
		return text
	}
	return t.sgr[r] + text + reset
}

func (t Theme) Ion(text string) string     { return t.paint(roleIon, text) }
func (t Theme) Nebula(text string) string  { return t.paint(roleNebula, text) }
func (t Theme) Haze(text string) string    { return t.paint(roleHaze, text) }
func (t Theme) Verdant(text string) string { return t.paint(roleVerdant, text) }
func (t Theme) Ember(text string) string   { return t.paint(roleEmber, text) }
func (t Theme) Plasma(text string) string  { return t.paint(rolePlasma, text) }
func (t Theme) Amber(text string) string   { return t.paint(roleAmber, text) }

// Bold keeps the current color.
func (t Theme) Bold(text string) string {
	if t.depth == NoColor || text == "" {
		return text
	}
	return bold + text + reset
}

// Gradient colors text from ion to nebula, one step per rune. ANSI 16 has no
// intermediate colors and uses ion for the whole text.
func (t Theme) Gradient(text string) string {
	switch t.depth {
	case NoColor:
		return text
	case ANSI16:
		return t.Ion(text)
	case ANSI256, TrueColor:
	}
	count := utf8.RuneCountInString(text)
	ion, nebula := palette[roleIon].color, palette[roleNebula].color
	var out strings.Builder
	out.Grow(len(text) + count*20)
	last, index := "", 0
	for _, r := range text {
		code := t.foreground(blend(ion, nebula, fraction(index, count)), "")
		if code != last {
			out.WriteString(code)
			last = code
		}
		out.WriteRune(r)
		index++
	}
	out.WriteString(reset)
	return out.String()
}

// ruleSegments bounds the color changes in a rule, so a wide terminal does
// not multiply the bytes of each composer redraw.
const ruleSegments = 24

// Rule is a hairline of width cells: ion to nebula over the first three
// quarters, then a fade into haze, like a beam that leaves the hull.
func (t Theme) Rule(width int) string {
	if width <= 0 {
		return ""
	}
	line := strings.Repeat("━", width)
	switch t.depth {
	case NoColor:
		return line
	case ANSI16:
		return t.Ion(line)
	case ANSI256, TrueColor:
	}
	ion, nebula, haze := palette[roleIon].color, palette[roleNebula].color, palette[roleHaze].color
	segment := max(1, (width+ruleSegments-1)/ruleSegments)
	var out strings.Builder
	out.Grow(width*3 + ruleSegments*20)
	last := ""
	for start := 0; start < width; start += segment {
		position := fraction(start, width)
		color := blend(ion, nebula, position/0.75)
		if position > 0.75 {
			color = blend(nebula, haze, (position-0.75)/0.25)
		}
		if code := t.foreground(color, ""); code != last {
			out.WriteString(code)
			last = code
		}
		out.WriteString(strings.Repeat("━", min(segment, width-start)))
	}
	out.WriteString(reset)
	return out.String()
}

// Shimmer draws text in haze with a bright band at phase that moves one rune
// per frame and wraps. Without 24-bit color the text stays ion, because a few
// palette steps would flicker rather than glide.
func (t Theme) Shimmer(text string, phase int) string {
	switch t.depth {
	case NoColor:
		return text
	case ANSI16, ANSI256:
		return t.Ion(text)
	case TrueColor:
	}
	const band = 3
	count := utf8.RuneCountInString(text)
	period := count + 2*band
	center := phase%period - band
	base, ion, flare := palette[roleHaze].color, palette[roleIon].color, palette[roleFlare].color
	var out strings.Builder
	out.Grow(len(text) + 12*20)
	last, index := "", 0
	for _, r := range text {
		distance := index - center
		if distance < 0 {
			distance = -distance
		}
		color := base
		switch {
		case distance == 0:
			color = flare
		case distance < band:
			color = blend(ion, base, float64(distance)/band)
		}
		if code := t.foreground(color, ""); code != last {
			out.WriteString(code)
			last = code
		}
		out.WriteRune(r)
		index++
	}
	out.WriteString(reset)
	return out.String()
}

// Gauge shows used of limit as cells of ▰ and ▱. The fill turns amber near
// the limit and ember above it.
func (t Theme) Gauge(used, limit, cells int) string {
	if limit <= 0 || cells <= 0 {
		return ""
	}
	filled := int(math.Round(float64(min(used, limit)) * float64(cells) / float64(limit)))
	fill := t.Ion
	switch share := float64(used) / float64(limit); {
	case share >= 1:
		fill = t.Ember
	case share >= 0.8:
		fill = t.Amber
	}
	return fill(strings.Repeat("▰", filled)) + t.Haze(strings.Repeat("▱", cells-filled))
}

func fraction(index, count int) float64 {
	if count <= 1 {
		return 0
	}
	return float64(index) / float64(count-1)
}

func blend(from, to rgb, share float64) rgb {
	share = min(max(share, 0), 1)
	mix := func(a, b uint8) uint8 {
		return uint8(math.Round(float64(a) + (float64(b)-float64(a))*share))
	}
	return rgb{mix(from.r, to.r), mix(from.g, to.g), mix(from.b, to.b)}
}

// nearest256 maps a color to the closest entry of the xterm 6x6x6 cube or the
// 24-step gray ramp.
func nearest256(color rgb) int {
	levels := [...]int{0, 95, 135, 175, 215, 255}
	closest := func(value uint8) int {
		best := 0
		for index, level := range levels {
			if abs(int(value)-level) < abs(int(value)-levels[best]) {
				best = index
			}
		}
		return best
	}
	r, g, b := closest(color.r), closest(color.g), closest(color.b)
	cube := 16 + 36*r + 6*g + b
	cubeDistance := square(levels[r]-int(color.r)) + square(levels[g]-int(color.g)) + square(levels[b]-int(color.b))
	average := (int(color.r) + int(color.g) + int(color.b)) / 3
	step := min(max((average-8+5)/10, 0), 23)
	gray := 8 + 10*step
	grayDistance := square(gray-int(color.r)) + square(gray-int(color.g)) + square(gray-int(color.b))
	if grayDistance < cubeDistance {
		return 232 + step
	}
	return cube
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func square(value int) int { return value * value }
