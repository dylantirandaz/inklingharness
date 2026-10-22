package presentation

import "math"

// BlotPicture is a Blot cut to a box of terminal cells and painted as ink that
// soaks into paper. A cell holds two square pixels, one above the other, and
// each pixel samples the blot at four points, so its edge is smooth.
//
// The opening animation follows wet ink. Each layer lands as a drop at its
// heaviest point and spreads outward with the water. The front of the water
// follows the fibers of the paper, carries pigment that pools along it, and
// leaves a faint stain ahead of it. When the spread stops, the stain dries
// away and the pooled pigment stays as a darker rim, as on dried sumi ink.
type BlotPicture struct {
	Columns, Rows int
	// For each layer, each pixel, row by row:
	//   coverage is the share of the pixel inside the finished shape;
	//   rim is how near the pixel is to the edge of the shape, from 0 at the
	//   inside of the rim band to 1 on the edge, where pigment pools;
	//   reach is how near an uncovered pixel is to the shape, for the stain;
	//   arrival is when the water reaches the pixel, from 0 to about 1.
	coverage, rim, reach, arrival [blotLayers][]float32
	// grain is the uneven take-up of pigment by the paper, from -0.5 to 0.5.
	grain  []float32
	colors [blotLayers - 1]shapeColor
	canvas *inkCanvas
}

// Each layer soaks in during a window of the opening animation: the ink blot
// first, then the shapes, one after another, each while the last is wet. The
// last window and its drying end before the animation does, so the frame
// that goes to scrollback is dry.
var blotWindows = [blotLayers]struct{ start, span float64 }{{0, 0.5}, {0.32, 0.22}, {0.42, 0.22}, {0.52, 0.22}}

const (
	// blotDrying is the share of the animation over which a layer dries
	// after its spread ends.
	blotDrying = 0.25
	// blotFeather is how far, in field units, the edge follows the fibers.
	blotFeather = 0.09
	// blotRimBand and blotStainBand are the widths, in field units, of the
	// pooled rim inside the edge and of the water stain outside it.
	blotRimBand   = 0.14
	blotStainBand = 0.16
	// blotFrontBand is the width, in arrival time, of the wet front.
	blotFrontBand = 0.12
	// blotFrontWander is how far, in arrival time, the fibers bend the front.
	blotFrontWander = 0.22
)

// The four samples of a pixel, as offsets in pixels.
var pixelSamples = [4][2]float64{{0.25, 0.25}, {0.75, 0.25}, {0.25, 0.75}, {0.75, 0.75}}

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
	// A pixel is half a cell wide and half a cell tall: 1/(2*scale) of the
	// grid across and 1/scale down. On screen it is twice as tall as wide,
	// so a distance on screen counts a step across as half a step down.
	pixelsWide, pixelsHigh := 2*columns, 2*rows
	count := pixelsWide * pixelsHigh
	picture := &BlotPicture{Columns: columns, Rows: rows, grain: make([]float32, count), colors: blot.colors, canvas: newInkCanvas(columns, rows)}
	// The fibers of the paper run mostly across, so the noise is stretched
	// along x. Every ink follows the same fibers, as on one sheet of paper.
	// The tone of the ink changes slowly across the shape, as a brush loads
	// unevenly, so the grain is low-frequency noise and not a pixel hash.
	fibers := make([]float64, count)
	for y := range pixelsHigh {
		for x := range pixelsWide {
			index := y*pixelsWide + x
			fibers[index] = fractalNoise(float64(x)*0.11, float64(y)*0.6, 3.7) - 0.5
			picture.grain[index] = float32(fractalNoise(float64(x)*0.09, float64(y)*0.18, 11.3) - 0.5)
		}
	}
	screenDistance := func(x, y, centerX, centerY float64) float64 { return math.Hypot((x-centerX)/2, y-centerY) }
	for layer := range blotLayers {
		coverage, rim, reach := make([]float32, count), make([]float32, count), make([]float32, count)
		var sumX, sumY, weight float64
		for y := range pixelsHigh {
			for x := range pixelsWide {
				index := y*pixelsWide + x
				edge := blotEdge + blotFeather*fibers[index]
				covered, nearest, highest := 0, math.Inf(1), math.Inf(-1)
				for _, sample := range pixelSamples {
					value := blot.fields[layer].at(originX+(float64(x)+sample[0])/(2*scale), originY+(float64(y)+sample[1])/scale) - edge
					if value >= 0 {
						covered++
						nearest = min(nearest, value)
					}
					highest = max(highest, value)
				}
				coverage[index] = float32(covered) / float32(len(pixelSamples))
				if covered > 0 {
					rim[index] = float32(max(1-nearest/blotRimBand, 0))
					sumX, sumY, weight = sumX+float64(x)*float64(covered), sumY+float64(y)*float64(covered), weight+float64(covered)
				} else {
					reach[index] = float32(max(1+highest/blotStainBand, 0))
				}
			}
		}
		arrival := make([]float32, count)
		if weight > 0 {
			// The drop lands at the middle of the shape, and the water
			// reaches each pixel in turn, sooner along a fiber.
			centerX, centerY := sumX/weight, sumY/weight
			farthest := 1.0
			for index, share := range coverage {
				if share > 0 {
					farthest = max(farthest, screenDistance(float64(index%pixelsWide), float64(index/pixelsWide), centerX, centerY))
				}
			}
			for index := range arrival {
				distance := screenDistance(float64(index%pixelsWide), float64(index/pixelsWide), centerX, centerY) / farthest
				arrival[index] = float32(min(max(distance+blotFrontWander*fibers[index], 0), 1.3))
			}
		}
		picture.coverage[layer], picture.rim[layer], picture.reach[layer], picture.arrival[layer] = coverage, rim, reach, arrival
	}
	return picture
}

// blotSpread is the arrival time that the front of a layer has passed at a
// progress from 0 to 1 of the animation, and how wet the layer still is,
// from 1 while it spreads to 0 when it is dry.
func blotSpread(layer int, progress float64) (front, wet float64) {
	window := blotWindows[layer]
	spread := (progress - window.start) / window.span
	if spread <= 0 {
		return math.Inf(-1), 1
	}
	// The water moves fast at first and slows as the paper takes it up.
	eased := 1 - math.Pow(1-min(spread, 1), 1.6)
	front = eased * (1.3 + blotFrontBand)
	wet = 1 - min(max((progress-window.start-window.span)/blotDrying, 0), 1)
	return front, wet
}

// Draw paints the picture at a progress from 0 to 1. At 0 nothing shows; at
// 1 the picture is complete and dry. It returns nil below 256 colors.
func (p *BlotPicture) Draw(t Theme, progress float64) []string {
	if t.depth < ANSI256 {
		return nil
	}
	p.canvas.reset()
	for layer := range blotLayers {
		front, wet := blotSpread(layer, progress)
		if math.IsInf(front, -1) {
			continue
		}
		pigment := t.blotColor(uint8(layer+1), p.colors)
		coverage, rim, reach, arrival := p.coverage[layer], p.rim[layer], p.reach[layer], p.arrival[layer]
		for index, share := range coverage {
			behind := front - float64(arrival[index])
			if behind < 0 {
				continue
			}
			x, y := index%p.canvas.width(), index/p.canvas.width()
			if share == 0 {
				// The stain of the water just outside the shape.
				if wet > 0 && reach[index] > 0 {
					p.canvas.over(x, y, pigment, 0.13*wet*float64(reach[index]))
				}
				continue
			}
			// The body takes up pigment unevenly, the rim holds what the
			// water carried to the edge, and the front is still dark and wet.
			density := 0.82 + 0.2*float64(p.grain[index])
			density += (1 - density) * float64(rim[index])
			if behind < blotFrontBand && wet > 0 {
				density += (1 - density) * (1 - behind/blotFrontBand) * wet
			}
			p.canvas.over(x, y, pigment, density*float64(share))
		}
	}
	return t.drawCanvas(p.canvas)
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

// paperColor is the background of the terminal: the one it reported, or an
// estimate from its shade.
func (t Theme) paperColor() rgb {
	if t.paperKnown {
		return t.paper
	}
	if t.shade == DarkBackground {
		return rgb{27, 27, 27}
	}
	return rgb{255, 255, 255}
}
