package presentation

import (
	"math"
	"strings"
)

// BlotPicture is a Blot cut to a box of terminal cells. A cell holds four
// quadrants drawn with block characters, so the picture is twice as fine as
// the cell grid along each axis.
type BlotPicture struct {
	Columns, Rows int
	// The next two hold one entry per quadrant, row by row: 2*Columns by
	// 2*Rows. A layer is 0 for the page and 1 to 4 for the blot and shapes.
	layer []uint8
	// reveal is the progress at which the quadrant starts to appear.
	reveal []float32
	colors [blotLayers - 1]shapeColor
}

// A quadrant is sampled at this many points along each axis.
const quadrantSamples = 2

// Each layer appears in a window of the opening animation: the ink blot
// spreads first, then the shapes, one after another.
var blotWindows = [blotLayers]struct{ start, span float64 }{{0, 0.45}, {0.4, 0.15}, {0.5, 0.15}, {0.6, 0.15}}

// blotDrying is the share of the animation over which new ink dries.
const blotDrying = 0.2

// NewBlotPicture samples a blot for a box of at most maxColumns by maxRows
// cells. It keeps the proportions of the picture, and returns nil when the
// box is too small to show a shape.
func NewBlotPicture(blot *Blot, maxColumns, maxRows int) *BlotPicture {
	if maxColumns < 8 || maxRows < 3 {
		return nil
	}
	left, top, right, bottom := blot.bounds()
	width, height := right-left, bottom-top
	// A cell is about twice as tall as wide, so the picture has as many
	// columns per grid cell as it has rows per two grid cells.
	scale := min(float64(maxColumns)/width, 2*float64(maxRows)/height)
	columns := min(maxColumns, int(math.Ceil(width*scale-1e-9)))
	rows := min(maxRows, int(math.Ceil(height*scale/2-1e-9)))
	originX := (left+right)/2 - float64(columns)/scale/2
	originY := (top+bottom)/2 - float64(rows)*2/scale/2
	picture := &BlotPicture{
		Columns: columns, Rows: rows,
		layer:  make([]uint8, 4*columns*rows),
		reveal: make([]float32, 4*columns*rows),
		colors: blot.colors,
	}
	wide := 2 * columns
	for row := range 2 * rows {
		for column := range wide {
			var votes [blotLayers + 1]int
			for sampleY := range quadrantSamples {
				for sampleX := range quadrantSamples {
					cellX := (float64(column) + (float64(sampleX)+0.5)/quadrantSamples) / 2
					cellY := (float64(row) + (float64(sampleY)+0.5)/quadrantSamples) / 2
					votes[blot.layerAt(originX+cellX/scale, originY+cellY*2/scale)+1]++
				}
			}
			// The page loses ties, so a shape keeps its thin parts.
			winner := 0
			for layer := 1; layer <= blotLayers; layer++ {
				if votes[layer] > 0 && votes[layer] >= votes[winner] {
					winner = layer
				}
			}
			picture.layer[row*wide+column] = uint8(winner)
		}
	}
	picture.schedule()
	return picture
}

// schedule gives each quadrant its moment: ink spreads from the middle of
// its layer, with a little jitter so that the edge looks like bleeding ink.
func (p *BlotPicture) schedule() {
	wide := 2 * p.Columns
	for layer := 1; layer <= blotLayers; layer++ {
		var sumX, sumY, count float64
		for index, owner := range p.layer {
			if int(owner) == layer {
				// A quadrant is half as wide as it is tall.
				sumX, sumY, count = sumX+float64(index%wide), sumY+2*float64(index/wide), count+1
			}
		}
		if count == 0 {
			continue
		}
		centerX, centerY := sumX/count, sumY/count
		farthest := 1.0
		for index, owner := range p.layer {
			if int(owner) == layer {
				farthest = max(farthest, math.Hypot(float64(index%wide)-centerX, 2*float64(index/wide)-centerY))
			}
		}
		window := blotWindows[layer-1]
		for index, owner := range p.layer {
			if int(owner) != layer {
				continue
			}
			distance := math.Hypot(float64(index%wide)-centerX, 2*float64(index/wide)-centerY) / farthest
			jitter := float64(mix(uint64(index))%1000)/1000*0.25 - 0.125
			p.reveal[index] = float32(window.start + window.span*min(max(distance+jitter, 0), 1))
		}
	}
}

// Quadrant glyphs by mask: bit 0 is the top left quadrant, bit 1 the top
// right, bit 2 the bottom left, bit 3 the bottom right.
var quadrantGlyphs = [16]string{" ", "▘", "▝", "▀", "▖", "▌", "▞", "▛", "▗", "▚", "▐", "▜", "▄", "▙", "▟", "█"}

type blotQuadrant struct {
	color rgb
	// paper is true for the page, which only the default background can show.
	paper bool
}

// blotColor is the color of a layer. The blot is the black of the page; on a
// dark terminal it is paper instead, as the page would be on a dark
// background.
func (t Theme) blotColor(layer uint8, colors [blotLayers - 1]shapeColor) rgb {
	if layer == 1 {
		if t.shade == DarkBackground {
			return rgb{236, 233, 228}
		}
		return rgb{22, 19, 17}
	}
	switch colors[layer-2] {
	case shapeBlue:
		if t.shade == DarkBackground {
			return rgb{38, 116, 235}
		}
		return markBlue
	case shapeRed:
		// The 256-color cube has no vermilion, and the nearest entry to the
		// brand red is orange. A redder input picks pure red instead.
		if t.depth == ANSI256 {
			return rgb{255, 40, 20}
		}
		return markRed
	case shapeGreen:
		return markGreen
	}
	panic("presentation: unknown shape color")
}

func (t Theme) paperColor() rgb {
	if t.shade == DarkBackground {
		return rgb{27, 27, 27}
	}
	return rgb{255, 255, 255}
}

// Draw paints the picture at a progress from 0 to 1. At 0 nothing shows; at
// 1 the picture is complete and dry. It returns nil below 256 colors.
func (p *BlotPicture) Draw(t Theme, progress float64) []string {
	if t.depth < ANSI256 {
		return nil
	}
	pale := palette[roleHairline].color.on(t.shade)
	paper := t.paperColor()
	wide := 2 * p.Columns
	quadrant := func(column, row int) blotQuadrant {
		index := row*wide + column
		layer := p.layer[index]
		if layer == 0 || progress <= float64(p.reveal[index]) {
			return blotQuadrant{color: paper, paper: true}
		}
		wetness := 1 - min((progress-float64(p.reveal[index]))/blotDrying, 1)
		return blotQuadrant{color: blend(t.blotColor(layer, p.colors), pale, 0.7*wetness)}
	}
	rows := make([]string, 0, p.Rows)
	for row := range p.Rows {
		var out strings.Builder
		// An empty cell with the default background costs no sequence.
		var foreground, background *rgb
		for column := range p.Columns {
			cell := [4]blotQuadrant{
				quadrant(2*column, 2*row), quadrant(2*column+1, 2*row),
				quadrant(2*column, 2*row+1), quadrant(2*column+1, 2*row+1),
			}
			mask, ink, ground := bestSplit(cell)
			glyph := quadrantGlyphs[mask]
			if ground == nil && mask == 0 {
				if background != nil {
					out.WriteString("\x1b[49m")
					background = nil
				}
				out.WriteString(" ")
				continue
			}
			if !sameColor(ground, background) {
				if ground == nil {
					out.WriteString("\x1b[49m")
				} else {
					out.WriteString(t.background(*ground))
				}
				background = ground
			}
			if mask != 0 && !sameColor(ink, foreground) {
				out.WriteString(t.foreground(*ink, ""))
				foreground = ink
			}
			out.WriteString(glyph)
		}
		out.WriteString(reset)
		rows = append(rows, out.String())
	}
	return rows
}

func sameColor(a, b *rgb) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// quadrantGroup is the quadrants of one cell that share a color.
type quadrantGroup struct {
	members [4]blotQuadrant
	count   int
}

func (g *quadrantGroup) add(q blotQuadrant) {
	g.members[g.count] = q
	g.count++
}

// representative is the member closest to the mean of the group. A cell then
// never shows a color that none of its quadrants has, which keeps the edge
// between three colors from turning muddy.
func (g *quadrantGroup) representative() blotQuadrant {
	var mean [3]float64
	for _, q := range g.members[:g.count] {
		mean[0] += float64(q.color.r) / float64(g.count)
		mean[1] += float64(q.color.g) / float64(g.count)
		mean[2] += float64(q.color.b) / float64(g.count)
	}
	best, bestDistance := g.members[0], math.Inf(1)
	for _, q := range g.members[:g.count] {
		red, green, blue := float64(q.color.r)-mean[0], float64(q.color.g)-mean[1], float64(q.color.b)-mean[2]
		distance := red*red + green*green + blue*blue
		if distance < bestDistance {
			best, bestDistance = q, distance
		}
	}
	return best
}

// bestSplit chooses the glyph mask, the color of its filled quadrants, and the
// background, so that the cell is as close as two colors can come to the four
// quadrants. A nil background is the default one, and a mask of 0 has no
// foreground. Paper is only ever drawn as the default background, which is
// the one color of the page that a terminal can promise.
func bestSplit(cell [4]blotQuadrant) (mask int, foreground, background *rgb) {
	bestMask, bestCost, bestGroundSize := 0, math.Inf(1), 0
	var bestInside, bestOutside blotQuadrant
	for candidate := range 15 {
		var inside, outside quadrantGroup
		for index, quadrant := range cell {
			if candidate>>index&1 == 1 {
				inside.add(quadrant)
			} else {
				outside.add(quadrant)
			}
		}
		ground := outside.representative()
		var ink blotQuadrant
		cost := 0.0
		if inside.count > 0 {
			ink = inside.representative()
			// Paper cannot be a foreground, so a split that needs it must lose.
			if ink.paper {
				cost += 1e6
			}
		}
		for index, quadrant := range cell {
			assigned := ground.color
			if candidate>>index&1 == 1 {
				assigned = ink.color
			}
			cost += float64(square(int(quadrant.color.r)-int(assigned.r)) + square(int(quadrant.color.g)-int(assigned.g)) + square(int(quadrant.color.b)-int(assigned.b)))
		}
		// Among equal splits the one with more background wins: a background
		// fills its whole cell, and a block glyph can leave a seam.
		if cost < bestCost-1 || cost <= bestCost+1 && outside.count > bestGroundSize {
			bestMask, bestCost, bestGroundSize = candidate, cost, outside.count
			bestInside, bestOutside = ink, ground
		}
	}
	if !bestOutside.paper {
		ground := bestOutside.color
		background = &ground
	}
	if bestMask != 0 {
		ink := bestInside.color
		foreground = &ink
	}
	return bestMask, foreground, background
}
