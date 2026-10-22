package presentation

import (
	"math"
	"strconv"
)

// This file holds the ink engine of the artwork: a canvas of pixels drawn
// four to a cell with the quadrant block glyphs, and the noise that gives ink
// its fibers and its uneven tone. Pixels hold a pigment and a coverage, and
// the canvas lays them over the real background of the terminal, so an
// anti-aliased edge melts into the page of the user.

// inkPixel is a pigment and how much of the pixel it covers, from 0 to 1.
type inkPixel struct {
	color rgb
	alpha float32
}

// inkCanvas is columns cells wide and rows cells tall. A cell holds two by two
// pixels, so the canvas is 2*columns pixels wide and 2*rows pixels tall. A
// pixel is half a cell wide and half a cell tall, which on a terminal is about
// twice as tall as it is wide.
type inkCanvas struct {
	columns, rows int
	pixels        []inkPixel
	// line is the scratch buffer of one drawn row.
	line []byte
}

func newInkCanvas(columns, rows int) *inkCanvas {
	return &inkCanvas{columns: columns, rows: rows, pixels: make([]inkPixel, 4*columns*rows)}
}

func (c *inkCanvas) width() int  { return 2 * c.columns }
func (c *inkCanvas) height() int { return 2 * c.rows }

func (c *inkCanvas) reset() {
	clear(c.pixels)
}

// over lays pigment with coverage alpha over the pixel at x, y, as wet ink
// lays over dry ink: the result is the "over" operator of compositing.
func (c *inkCanvas) over(x, y int, color rgb, alpha float64) {
	if alpha <= 0 || x < 0 || y < 0 || x >= c.width() || y >= c.height() {
		return
	}
	alpha = min(alpha, 1)
	pixel := &c.pixels[y*c.width()+x]
	below := float64(pixel.alpha)
	total := alpha + below*(1-alpha)
	mixChannel := func(top, bottom uint8) uint8 {
		return uint8(math.Round((float64(top)*alpha + float64(bottom)*below*(1-alpha)) / total))
	}
	pixel.color = rgb{mixChannel(color.r, pixel.color.r), mixChannel(color.g, pixel.color.g), mixChannel(color.b, pixel.color.b)}
	pixel.alpha = float32(total)
}

// invisibleInk is the coverage below which a pixel shows the page.
const invisibleInk = 1.0 / 40

// Quadrant glyphs by mask: bit 0 is the top left quadrant, bit 1 the top
// right, bit 2 the bottom left, bit 3 the bottom right. A set bit is drawn in
// the foreground color.
var quadrantGlyphs = [16]string{" ", "▘", "▝", "▀", "▖", "▌", "▞", "▛", "▗", "▚", "▐", "▜", "▄", "▙", "▟", "█"}

// drawCanvas returns one string for each row of cells, or nil below 256
// colors. A cell can show two colors, so each cell takes the split of its
// four pixels into two groups that is closest to them. Pixels without ink
// show the terminal background itself, so the art never paints a box around
// itself. Neighbors of one color share their escape sequence.
func (t Theme) drawCanvas(c *inkCanvas) []string {
	if t.depth < ANSI256 {
		return nil
	}
	paper := t.paperColor()
	width := c.width()
	rows := make([]string, c.rows)
	for row := range c.rows {
		line := c.line[:0]
		var foreground, background rgb
		haveForeground, haveBackground := false, false
		for column := range c.columns {
			var colors [4]rgb
			empty := 0
			for index := range 4 {
				pixel := c.pixels[(2*row+index/2)*width+2*column+index%2]
				if pixel.alpha < invisibleInk {
					colors[index] = paper
					empty |= 1 << index
				} else {
					colors[index] = blend(paper, pixel.color, float64(pixel.alpha))
				}
			}
			if empty == 15 {
				if haveBackground {
					line = append(line, "\x1b[49m"...)
					haveBackground = false
				}
				line = append(line, ' ')
				continue
			}
			mask, ink, ground := bestSplit(colors)
			if empty != 0 && mask == empty {
				// The empty pixels came out on the glyph side; the inverse
				// glyph with swapped colors draws the same cell.
				mask, ink, ground = 15^mask, ground, ink
			}
			// The empty pixels of the page keep the terminal's own background
			// when they are exactly the background group.
			groundIsPage := empty != 0 && empty == 15&^mask
			if groundIsPage {
				if haveBackground {
					line = append(line, "\x1b[49m"...)
					haveBackground = false
				}
			} else if !haveBackground || ground != background {
				line = t.appendColor(line, ground, true)
				background, haveBackground = ground, true
			}
			if mask != 0 && (!haveForeground || ink != foreground) {
				line = t.appendColor(line, ink, false)
				foreground, haveForeground = ink, true
			}
			line = append(line, quadrantGlyphs[mask]...)
		}
		line = append(line, reset...)
		rows[row] = string(line)
		c.line = line
	}
	return rows
}

// bestSplit chooses the glyph mask and the two colors that come closest to
// the four pixels of a cell: each group takes the mean color of its pixels,
// and the split with the smallest squared error wins. A split must gain more
// than splitPenalty over a flat cell, because a glyph between two close
// colors shows as a seam and not as a tone. Among equal splits the one with
// more background wins, because a background fills its whole cell and a
// glyph can leave a seam.
func bestSplit(colors [4]rgb) (mask int, ink, ground rgb) {
	bestCost, bestGround := math.Inf(1), -1
	for candidate := range 16 {
		var inside, outside [3]float64
		insideCount, outsideCount := 0, 0
		for index, color := range colors {
			sum, count := &outside, &outsideCount
			if candidate>>index&1 == 1 {
				sum, count = &inside, &insideCount
			}
			sum[0], sum[1], sum[2] = sum[0]+float64(color.r), sum[1]+float64(color.g), sum[2]+float64(color.b)
			*count++
		}
		mean := func(sum [3]float64, count int) rgb {
			if count == 0 {
				return rgb{}
			}
			return rgb{uint8(math.Round(sum[0] / float64(count))), uint8(math.Round(sum[1] / float64(count))), uint8(math.Round(sum[2] / float64(count)))}
		}
		in, out := mean(inside, insideCount), mean(outside, outsideCount)
		cost := 0.0
		if candidate != 0 && candidate != 15 {
			cost = splitPenalty
		}
		for index, color := range colors {
			target := out
			if candidate>>index&1 == 1 {
				target = in
			}
			cost += float64(square(int(color.r)-int(target.r)) + square(int(color.g)-int(target.g)) + square(int(color.b)-int(target.b)))
		}
		if cost < bestCost-1 || cost <= bestCost+1 && outsideCount > bestGround {
			mask, ink, ground, bestCost, bestGround = candidate, in, out, cost, outsideCount
		}
	}
	if mask == 15 {
		// A full block and a plain background look the same; the background
		// needs no glyph color.
		return 0, rgb{}, ink
	}
	return mask, ink, ground
}

// splitPenalty is the squared color error, summed over the four pixels, that
// a glyph must save to be drawn: about 12 levels on each channel of one
// pixel.
const splitPenalty = 3 * 12 * 12

// appendColor appends the escape sequence of a foreground or background color
// without formatting through fmt, because an animation writes thousands of
// them each second.
func (t Theme) appendColor(line []byte, color rgb, background bool) []byte {
	line = append(line, "\x1b["...)
	if background {
		line = append(line, '4', '8')
	} else {
		line = append(line, '3', '8')
	}
	switch t.depth {
	case TrueColor:
		line = append(line, ";2;"...)
		line = strconv.AppendUint(line, uint64(color.r), 10)
		line = append(line, ';')
		line = strconv.AppendUint(line, uint64(color.g), 10)
		line = append(line, ';')
		line = strconv.AppendUint(line, uint64(color.b), 10)
	case ANSI256:
		line = append(line, ";5;"...)
		line = strconv.AppendInt(line, int64(nearest256(color)), 10)
	case ANSI16, NoColor:
		panic("presentation: the ink canvas needs 256 colors or more")
	}
	return append(line, 'm')
}

// OnPaper gives the theme the exact background color of the terminal, as the
// terminal reported it. Ink then blends into the real page instead of an
// estimate, so soft edges show no seam. The shade follows the color.
func (t Theme) OnPaper(r, g, b uint8) Theme {
	if shade := ShadeOf(r, g, b); shade != t.shade {
		t = NewTheme(t.depth, shade, t.accent)
	}
	t.paper, t.paperKnown = rgb{r, g, b}, true
	return t
}

// hash01 maps a lattice point to a number from 0 to 1 that looks random.
func hash01(x, y, z int64) float64 {
	h := mix(uint64(x)*0x9E3779B97F4A7C15 ^ uint64(y)*0xC2B2AE3D27D4EB4F ^ uint64(z)*0x165667B19E3779F9)
	return float64(h>>11) / (1 << 53)
}

func smoothStep(t float64) float64 { return t * t * (3 - 2*t) }

// valueNoise is smooth noise from 0 to 1 in three dimensions: the third is
// usually time, so a field changes without jumps.
func valueNoise(x, y, z float64) float64 {
	x0, y0, z0 := math.Floor(x), math.Floor(y), math.Floor(z)
	fx, fy, fz := smoothStep(x-x0), smoothStep(y-y0), smoothStep(z-z0)
	ix, iy, iz := int64(x0), int64(y0), int64(z0)
	lerp := func(a, b, t float64) float64 { return a + (b-a)*t }
	corner := func(dx, dy, dz int64) float64 { return hash01(ix+dx, iy+dy, iz+dz) }
	bottom := lerp(lerp(corner(0, 0, 0), corner(1, 0, 0), fx), lerp(corner(0, 1, 0), corner(1, 1, 0), fx), fy)
	top := lerp(lerp(corner(0, 0, 1), corner(1, 0, 1), fx), lerp(corner(0, 1, 1), corner(1, 1, 1), fx), fy)
	return lerp(bottom, top, fz)
}

// fractalNoise adds three octaves of value noise, so a field has large
// shapes with fine detail on them, as ink does.
func fractalNoise(x, y, z float64) float64 {
	return (4*valueNoise(x, y, z) + 2*valueNoise(2*x+17, 2*y+31, 2*z) + valueNoise(4*x+59, 4*y+7, 4*z)) / 7
}
