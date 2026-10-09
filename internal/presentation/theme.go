package presentation

import (
	"fmt"
	"math"
	"strings"
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

// Shade is the brightness of the terminal background. A light background
// gets ink on paper, as on thinkingmachines.ai; a dark one gets paper on ink.
type Shade uint8

const (
	DarkBackground Shade = iota
	LightBackground
)

// ShadeOf classifies a background color by its relative luminance, with the
// boundary at perceptual middle gray.
func ShadeOf(r, g, b uint8) Shade {
	if 0.2126*linear(r)+0.7152*linear(g)+0.0722*linear(b) > 0.18 {
		return LightBackground
	}
	return DarkBackground
}

func linear(channel uint8) float64 {
	value := float64(channel) / 255
	if value <= 0.04045 {
		return value / 12.92
	}
	return math.Pow((value+0.055)/1.055, 2.4)
}

// Accent is the color of a model family, as on the Inkling model cards.
type Accent uint8

const (
	AccentGreen Accent = iota
	AccentBlue
	AccentPlum
)

// AccentFor gives Inkling-Small plum, Inkling blue, and other models green.
func AccentFor(model string) Accent {
	slug := strings.ToLower(model)
	if _, after, found := strings.Cut(slug, "/"); found {
		slug = after
	}
	switch {
	case strings.HasPrefix(slug, "inkling-small"):
		return AccentPlum
	case strings.HasPrefix(slug, "inkling"):
		return AccentBlue
	default:
		return AccentGreen
	}
}

type rgb struct{ r, g, b uint8 }

// shaded holds one color for each background shade.
type shaded struct{ dark, light rgb }

func (s shaded) on(shade Shade) rgb {
	switch shade {
	case DarkBackground:
		return s.dark
	case LightBackground:
		return s.light
	}
	panic(fmt.Sprintf("presentation: unknown shade %d", shade))
}

type role uint8

const (
	roleInk role = iota
	roleGraphite
	roleHairline
	roleGreen
	roleBlue
	roleRed
	roleAmber
	roleAccent
	roleCount
)

type swatch struct {
	color  shaded
	ansi16 string
}

// The brand colors come from the Inkling mark; the grays from the site text.
var (
	green = shaded{dark: rgb{45, 190, 144}, light: rgb{14, 153, 114}}
	blue  = shaded{dark: rgb{91, 151, 242}, light: rgb{1, 85, 191}}
	plum  = shaded{dark: rgb{208, 122, 174}, light: rgb{143, 63, 113}}

	palette = [roleAccent]swatch{
		roleInk:      {shaded{dark: rgb{232, 232, 232}, light: rgb{40, 40, 40}}, "39"},
		roleGraphite: {shaded{dark: rgb{160, 160, 160}, light: rgb{103, 103, 103}}, "90"},
		roleHairline: {shaded{dark: rgb{64, 64, 64}, light: rgb{226, 226, 226}}, "90"},
		roleGreen:    {green, "32"},
		roleBlue:     {blue, "34"},
		roleRed:      {shaded{dark: rgb{242, 97, 76}, light: rgb{214, 58, 39}}, "31"},
		roleAmber:    {shaded{dark: rgb{247, 162, 36}, light: rgb{184, 116, 16}}, "33"},
	}

	accents = [...]swatch{
		AccentGreen: {green, "32"},
		AccentBlue:  {blue, "34"},
		AccentPlum:  {plum, "35"},
	}

	// chipColor is the faint tint behind chips and the user's prompts.
	chipColor = shaded{dark: rgb{35, 35, 35}, light: rgb{245, 245, 245}}
	// The shapes of the mark keep their brand colors on both shades; only the
	// blot follows the ink.
	markGreen, markBlue, markRed = rgb{14, 153, 114}, rgb{1, 85, 191}, rgb{239, 64, 44}
)

// Theme holds the escape sequences of each role for one color depth, shade,
// and accent. The zero value is NoColor: every method then returns its text
// unchanged, so callers never branch on color themselves.
type Theme struct {
	depth  ColorDepth
	shade  Shade
	accent Accent
	sgr    [roleCount]string
	// chip is the background sequence of chips; empty below 256 colors,
	// where a tint cannot be expressed.
	chip string
}

// NewTheme computes every sequence once.
func NewTheme(depth ColorDepth, shade Shade, accent Accent) Theme {
	theme := Theme{depth: depth, shade: shade, accent: accent}
	if depth == NoColor {
		return theme
	}
	for index, entry := range palette {
		theme.sgr[index] = theme.foreground(entry.color.on(shade), entry.ansi16)
	}
	model := accents[accent]
	theme.sgr[roleAccent] = theme.foreground(model.color.on(shade), model.ansi16)
	if depth >= ANSI256 {
		theme.chip = theme.background(chipColor.on(shade))
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

// background has no ANSI 16 form; callers check the depth first.
func (t Theme) background(color rgb) string {
	switch t.depth {
	case TrueColor:
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", color.r, color.g, color.b)
	case ANSI256:
		return fmt.Sprintf("\x1b[48;5;%dm", nearest256(color))
	case ANSI16, NoColor:
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

func (t Theme) attribute(code, text string) string {
	if t.depth == NoColor || text == "" {
		return text
	}
	return code + text + reset
}

func (t Theme) Ink(text string) string      { return t.paint(roleInk, text) }
func (t Theme) Graphite(text string) string { return t.paint(roleGraphite, text) }
func (t Theme) Hairline(text string) string { return t.paint(roleHairline, text) }
func (t Theme) Green(text string) string    { return t.paint(roleGreen, text) }
func (t Theme) Blue(text string) string     { return t.paint(roleBlue, text) }
func (t Theme) Red(text string) string      { return t.paint(roleRed, text) }
func (t Theme) Amber(text string) string    { return t.paint(roleAmber, text) }

// Accent is the model color: plum for Inkling-Small, blue for Inkling.
func (t Theme) Accent(text string) string { return t.paint(roleAccent, text) }

// Bold, Italic, and Underline keep the current color.
func (t Theme) Bold(text string) string      { return t.attribute(bold, text) }
func (t Theme) Italic(text string) string    { return t.attribute("\x1b[3m", text) }
func (t Theme) Underline(text string) string { return t.attribute("\x1b[4m", text) }

// Chip sets text on the faint tint of a site badge. Without a tint it stays
// ink, so a chip never depends on its background to be read.
func (t Theme) Chip(text string) string {
	if t.chip == "" {
		return t.Ink(text)
	}
	return t.chip + t.sgr[roleInk] + text + reset
}

// Code marks inline code: a chip when a tint exists, else the accent.
func (t Theme) Code(text string) string {
	if t.chip == "" {
		return t.Accent(text)
	}
	return t.Chip(text)
}

// Key labels a key to press: a chip when a tint exists, bold accent with
// only ANSI 16 colors, and brackets without color.
func (t Theme) Key(key string) string {
	switch {
	case t.depth == NoColor:
		return "[" + key + "]"
	case t.chip == "":
		return t.Bold(t.Accent(key))
	default:
		return t.Chip(" " + key + " ")
	}
}

// Rule is a hairline of width cells.
func (t Theme) Rule(width int) string {
	if width <= 0 {
		return ""
	}
	return t.Hairline(strings.Repeat("─", width))
}

// squares writes one cell per entry of styles. Neighbors of the same style
// share one sequence, so an animated row stays a few dozen bytes.
func (t Theme) squares(styles []role, glyphs func(role) string) string {
	var out strings.Builder
	for start := 0; start < len(styles); {
		end := start
		for end < len(styles) && styles[end] == styles[start] {
			end++
		}
		out.WriteString(t.paint(styles[start], strings.Repeat(glyphs(styles[start]), end-start)))
		start = end
	}
	return out.String()
}

// squareGlyph is the glyph of one cell. Without color an empty cell is a dot,
// so the state stays visible in plain text.
func (t Theme) squareGlyph(r role) string {
	if t.depth == NoColor && r == roleHairline {
		return "·"
	}
	return "▪"
}

// Blends reports whether the theme can draw the intermediate colors that the
// fades need: drying ink, the breathing bar, and the mark reveal.
func (t Theme) Blends() bool { return t.depth >= ANSI256 }

// mix is the splitmix64 finalizer.
func mix(value uint64) uint64 {
	value = (value ^ value>>30) * 0xbf58476d1ce4e5b9
	value = (value ^ value>>27) * 0x94d049bb133111eb
	return value ^ value>>31
}

// Wet draws text that just arrived: wetness 1 is pale, 0 is dry ink. Without
// intermediate colors the text is plain.
func (t Theme) Wet(text string, wetness float64) string {
	if !t.Blends() || text == "" {
		return text
	}
	ink := palette[roleInk].color.on(t.shade)
	pale := palette[roleHairline].color.on(t.shade)
	return t.foreground(blend(ink, pale, min(max(wetness, 0), 1)), "") + text + reset
}

// Glow draws text in amber that breathes: intensity 1 is full amber, 0 is
// half way to the hairline. Without intermediate colors it stays amber.
func (t Theme) Glow(text string, intensity float64) string {
	if !t.Blends() {
		return t.Amber(text)
	}
	amber := palette[roleAmber].color.on(t.shade)
	pale := palette[roleHairline].color.on(t.shade)
	return t.foreground(blend(pale, amber, 0.5+0.5*min(max(intensity, 0), 1)), "") + text + reset
}

// Gauge shows used of limit as a row of squares. Used squares take the
// accent, turn amber near the limit, and red above it. glow from 0 to 1
// dims and restores the amber and red squares, so a caller can make them
// breathe; 1 is steady.
func (t Theme) Gauge(used, limit, cells int, glow float64) string {
	if limit <= 0 || cells <= 0 {
		return ""
	}
	filled := int(math.Round(float64(min(max(used, 0), limit)) * float64(cells) / float64(limit)))
	if used > 0 && filled == 0 {
		filled = 1
	}
	fill := roleAccent
	switch share := float64(used) / float64(limit); {
	case share >= 1:
		fill = roleRed
	case share >= 0.8:
		fill = roleAmber
	}
	styles := make([]role, cells)
	for index := range styles {
		styles[index] = roleHairline
		if index < filled {
			styles[index] = fill
		}
	}
	if fill == roleAccent || glow >= 1 || !t.Blends() {
		return t.squares(styles, t.squareGlyph)
	}
	color := blend(palette[roleHairline].color.on(t.shade), palette[fill].color.on(t.shade), 0.5+0.5*max(glow, 0))
	return t.foreground(color, "") + strings.Repeat("▪", filled) + reset + t.Hairline(strings.Repeat("▪", cells-filled))
}

// inklingMark is the Inkling mark at 16 by 12 pixels, sampled from the logo
// on thinkingmachines.ai/inkling: the ink blot (K) with its green shape (G)
// and its hole, the blue dot (B), and the red pill (R).
var inklingMark = [...]string{
	"..........KKKKK.",
	"...KKK..GGGGGKKK",
	"...KKK.GGGGGGKK.",
	"..KKKKKGGGGGGKKK",
	"..KKKKKKGGGGKKKK",
	"...KKKKKGGGKKKKK",
	"...KKKKKKKKK.KK.",
	"....KKKKKKKKKR..",
	".BB..KKKKKKRRRR.",
	"BBBB..KK..RRRRR.",
	"BBBB......RRR...",
	".BB.......RRR...",
}

// MarkWidth is the number of cells of each Mark row.
const MarkWidth = 16

// markDrying is the share of the reveal over which a new pixel dries.
const markDrying = 0.2

// Mark draws the Inkling mark in six rows with half blocks: each cell holds
// two pixels, the top one as foreground and the bottom one as background.
// The blot takes the ink color, so it inverts with the shade. Below 256
// colors the mark cannot be drawn and Mark returns nil.
func (t Theme) Mark() []string { return t.MarkReveal(1) }

// MarkReveal draws the mark as ink that bleeds from one drop: at progress 0
// nothing shows; the blot spreads from its center, then the green shape, the
// blue dot, and the red pill appear; each new pixel starts pale and dries.
// At progress 1 the result equals Mark.
func (t Theme) MarkReveal(progress float64) []string {
	if t.depth < ANSI256 {
		return nil
	}
	reveal := markRevealTimes()
	pale := palette[roleHairline].color.on(t.shade)
	pixel := func(x, y int) (rgb, bool) {
		code := inklingMark[y][x]
		if code == '.' || progress < reveal[y][x] {
			return rgb{}, false
		}
		var color rgb
		switch code {
		case 'K':
			color = palette[roleInk].color.on(t.shade)
		case 'G':
			color = markGreen
		case 'B':
			color = markBlue
		case 'R':
			color = markRed
		default:
			panic(fmt.Sprintf("presentation: unknown mark pixel %q", code))
		}
		wetness := 1 - min((progress-reveal[y][x])/markDrying, 1)
		return blend(color, pale, 0.7*wetness), true
	}
	rows := make([]string, 0, len(inklingMark)/2)
	for y := 0; y < len(inklingMark); y += 2 {
		var out strings.Builder
		for x := range MarkWidth {
			top, topSet := pixel(x, y)
			bottom, bottomSet := pixel(x, y+1)
			switch {
			case !topSet && !bottomSet:
				out.WriteString(reset + " ")
			case topSet && !bottomSet:
				out.WriteString(reset + t.foreground(top, "") + "▀")
			case !topSet && bottomSet:
				out.WriteString(reset + t.foreground(bottom, "") + "▄")
			case top == bottom:
				out.WriteString(reset + t.foreground(top, "") + "█")
			default:
				out.WriteString(t.foreground(top, "") + t.background(bottom) + "▀")
			}
		}
		out.WriteString(reset)
		rows = append(rows, out.String())
	}
	return rows
}

// markRevealTimes gives each pixel the progress at which it appears. The blot
// spreads by distance from its centroid with a little hashed jitter, so the
// edge looks like bleeding ink rather than a circle; each shape follows in
// turn. Every pixel appears by 1-markDrying, so it is dry at progress 1.
func markRevealTimes() [len(inklingMark)][MarkWidth]float64 {
	type window struct{ start, span float64 }
	windows := map[byte]window{'K': {0, 0.45}, 'G': {0.4, 0.15}, 'B': {0.5, 0.15}, 'R': {0.6, 0.15}}
	var times [len(inklingMark)][MarkWidth]float64
	for code, slot := range windows {
		var sumX, sumY, count float64
		for y, row := range inklingMark {
			for x := range MarkWidth {
				if row[x] == code {
					sumX, sumY, count = sumX+float64(x), sumY+float64(y), count+1
				}
			}
		}
		centerX, centerY := sumX/count, sumY/count
		farthest := 0.0
		for y, row := range inklingMark {
			for x := range MarkWidth {
				if row[x] == code {
					farthest = max(farthest, math.Hypot(float64(x)-centerX, float64(y)-centerY))
				}
			}
		}
		for y, row := range inklingMark {
			for x := range MarkWidth {
				if row[x] != code {
					continue
				}
				distance := math.Hypot(float64(x)-centerX, float64(y)-centerY) / max(farthest, 1)
				jitter := float64(mix(uint64(y*MarkWidth+x))%1000)/1000*0.25 - 0.125
				times[y][x] = slot.start + slot.span*min(max(distance+jitter, 0), 1)
			}
		}
	}
	return times
}

func blend(from, to rgb, share float64) rgb {
	share = min(max(share, 0), 1)
	channel := func(a, b uint8) uint8 {
		return uint8(math.Round(float64(a) + (float64(b)-float64(a))*share))
	}
	return rgb{channel(from.r, to.r), channel(from.g, to.g), channel(from.b, to.b)}
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
