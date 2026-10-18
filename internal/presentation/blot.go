package presentation

import (
	"math"
)

// This file draws the artwork of thinkingmachines.ai/inkling. The page grows
// a black blot on a grid of 24 by 16 cells and adds three colored shapes, all
// from one seed. Each layer is blurred and cut at one half, so its edge is
// smooth. The steps and the numbers follow the page's own script, so a seed
// gives the picture that the page shows for it.

// BlotSeeds are the seeds that the page rotates through. They are the ones
// that make a good picture, so a caller should pick from them.
var BlotSeeds = [...]uint32{
	4080338587, 2713983725, 38430317, 1770525004, 3398537396, 930963614,
	338165897, 2228226371, 1966101379, 3760791918, 4151914198,
}

const (
	blotColumns   = 24
	blotRows      = 16
	blotCell      = 10 // field pixels in one grid cell
	blotRadius    = 11 // box blur radius in field pixels
	blotPasses    = 3
	blotMargin    = blotRadius + 3
	blotFieldW    = blotColumns*blotCell + 2*blotMargin
	blotFieldH    = blotRows*blotCell + 2*blotMargin
	blotDeclutter = 2
	// blotEdge is the field value at which a layer ends.
	blotEdge = 0.5
)

// blotLayers is the ink blot and the three shapes painted over it.
const blotLayers = 4

// shapeColor is the color of one of the three shapes.
type shapeColor uint8

const (
	shapeBlue shapeColor = iota
	shapeRed
	shapeGreen
)

type shapeKind uint8

const (
	shapeMedium shapeKind = iota
	shapeSmall
	shapeTiny
)

type blotCells [blotRows * blotColumns]bool

// blotField holds one blurred layer. The index is x*blotFieldH+y.
type blotField []float32

// Blot is one picture of the artwork: the ink blot first, then the shapes.
type Blot struct {
	fields [blotLayers]blotField
	// colors names the color of layer 1 to 3; layer 0 is ink.
	colors [blotLayers - 1]shapeColor
}

// blotRandom is mulberry32, the generator of the page's script.
type blotRandom struct{ state uint32 }

func (r *blotRandom) float() float64 {
	r.state += 0x6D2B79F5
	t := (r.state ^ r.state>>15) * (1 | r.state)
	t = (t + (t^t>>7)*(61|t)) ^ t
	return float64(t^t>>14) / 4294967296
}

func (r *blotRandom) between(low, high float64) float64 { return low + r.float()*(high-low) }

func (r *blotRandom) below(n int) int { return int(math.Floor(r.float() * float64(n))) }

func shuffle[T any](r *blotRandom, items []T) {
	for last := len(items) - 1; last > 0; last-- {
		other := r.below(last + 1)
		items[last], items[other] = items[other], items[last]
	}
}

// NewBlot draws the artwork for a seed. Seed 0 gives the picture of seed 1,
// as on the page.
func NewBlot(seed uint32) *Blot {
	if seed == 0 {
		seed = 1
	}
	random := blotRandom{state: seed}
	var layers [blotLayers]blotCells
	layers[0] = growBlot(&random)
	colors := [...]shapeColor{shapeBlue, shapeRed, shapeGreen}
	shuffle(&random, colors[:])
	kinds := [...]shapeKind{shapeMedium, shapeSmall, shapeTiny}
	shuffle(&random, kinds[:])
	// Each shape sits near one of three places on the grid.
	places := [...][2]float64{{blotColumns * 0.72, blotRows * 0.22}, {blotColumns * 0.28, blotRows * 0.72}, {blotColumns * 0.8, blotRows * 0.7}}
	for index, place := range places {
		layers[index+1] = growShape(&random, place[0], place[1], kinds[index])
	}
	blot := &Blot{colors: colors}
	// One scratch field serves every pass of every layer.
	scratch := make(blotField, blotFieldW*blotFieldH)
	for index := range layers {
		declutter(&layers[index], blotDeclutter)
		blot.fields[index] = blurredField(&layers[index], scratch)
	}
	return blot
}

// growBlot walks at random from the middle of the grid and fills the cells
// that it visits, until about a third of the grid is full.
func growBlot(random *blotRandom) blotCells {
	var cells blotCells
	x, y := blotColumns/2, blotRows/2
	cells[y*blotColumns+x] = true
	filled := 1
	target := int(math.Floor(blotColumns * blotRows * random.between(0.32, 0.44)))
	steps := [...]int{-1, 0, 1}
	for attempt := 0; filled < target && attempt < 5000; attempt++ {
		x += steps[random.below(3)]
		y += steps[random.below(3)]
		x = min(max(x, 0), blotColumns-1)
		y = min(max(y, 0), blotRows-1)
		if !cells[y*blotColumns+x] {
			cells[y*blotColumns+x] = true
			filled++
		}
	}
	return cells
}

// growShape fills the cells inside a few overlapping circles near a place.
func growShape(random *blotRandom, x, y float64, kind shapeKind) blotCells {
	var cells blotCells
	circle := func(centerX, centerY, radius float64) {
		for row := range blotRows {
			for column := range blotColumns {
				if math.Hypot(float64(column)-centerX, float64(row)-centerY) <= radius {
					cells[row*blotColumns+column] = true
				}
			}
		}
	}
	if kind == shapeTiny {
		centerX, centerY := math.Floor(x+0.5), math.Floor(y+0.5)
		firstRadius := random.between(2, 2.3)
		offsetX := float64(random.below(3) - 1)
		offsetY := float64(random.below(3) - 1)
		secondRadius := random.between(1.5, 1.8)
		circle(centerX, centerY, firstRadius)
		circle(centerX+offsetX, centerY+offsetY, secondRadius)
		return cells
	}
	// The page draws the count once, after it knows the size of the shape.
	fewest, spread, smallest, largest := 2, 1.3, 1.6, 2.2
	if kind == shapeMedium {
		fewest, spread, smallest, largest = 3, 1.9, 1.9, 2.7
	}
	count := fewest + random.below(2)
	for range count {
		centerX := x + random.between(-spread, spread)
		centerY := y + random.between(-spread, spread)
		circle(centerX, centerY, random.between(smallest, largest))
	}
	return cells
}

// declutter removes cells with at most one neighbor and fills gaps with at
// least three, which rounds the walk's edge. It stops early when nothing
// changes.
func declutter(cells *blotCells, rounds int) {
	for range rounds {
		next := *cells
		changed := false
		for row := range blotRows {
			for column := range blotColumns {
				index := row*blotColumns + column
				neighbors := 0
				if column > 0 && cells[index-1] {
					neighbors++
				}
				if column < blotColumns-1 && cells[index+1] {
					neighbors++
				}
				if row > 0 && cells[index-blotColumns] {
					neighbors++
				}
				if row < blotRows-1 && cells[index+blotColumns] {
					neighbors++
				}
				switch {
				case cells[index] && neighbors <= 1:
					next[index], changed = false, true
				case !cells[index] && neighbors >= 3:
					next[index], changed = true, true
				}
			}
		}
		*cells = next
		if !changed {
			return
		}
	}
}

// blurredField paints each cell as a square of field pixels and blurs the
// result with three passes of a box filter in each direction. The scratch
// field holds the pass in progress and is left in an unspecified state.
func blurredField(cells *blotCells, scratch blotField) blotField {
	field := make(blotField, blotFieldW*blotFieldH)
	for row := range blotRows {
		for column := range blotColumns {
			if !cells[row*blotColumns+column] {
				continue
			}
			left, top := blotMargin+column*blotCell, blotMargin+row*blotCell
			for x := left; x < left+blotCell; x++ {
				for y := top; y < top+blotCell; y++ {
					field[x*blotFieldH+y] = 1
				}
			}
		}
	}
	for range blotPasses {
		blurAlongX(scratch, field)
		blurAlongY(field, scratch)
	}
	return field
}

// The filters divide by the whole window, so the field fades to zero at its
// edge, as in the page's script. Each writes its result to blurred, which
// must not be source.
func blurAlongX(blurred, source blotField) {
	window := float64(2*blotRadius + 1)
	for y := range blotFieldH {
		sum := 0.0
		for x := 0; x <= blotRadius && x < blotFieldW; x++ {
			sum += float64(source[x*blotFieldH+y])
		}
		for x := range blotFieldW {
			blurred[x*blotFieldH+y] = float32(sum / window)
			if leaving := x - blotRadius; leaving >= 0 {
				sum -= float64(source[leaving*blotFieldH+y])
			}
			if entering := x + blotRadius + 1; entering < blotFieldW {
				sum += float64(source[entering*blotFieldH+y])
			}
		}
	}
}

func blurAlongY(blurred, source blotField) {
	window := float64(2*blotRadius + 1)
	for x := range blotFieldW {
		column := source[x*blotFieldH : (x+1)*blotFieldH]
		out := blurred[x*blotFieldH : (x+1)*blotFieldH]
		sum := 0.0
		for y := 0; y <= blotRadius && y < blotFieldH; y++ {
			sum += float64(column[y])
		}
		for y := range blotFieldH {
			out[y] = float32(sum / window)
			if leaving := y - blotRadius; leaving >= 0 {
				sum -= float64(column[leaving])
			}
			if entering := y + blotRadius + 1; entering < blotFieldH {
				sum += float64(column[entering])
			}
		}
	}
}

// at reads the field at a point of the grid, in cells, with linear blending
// between field pixels. Outside the field it is zero.
func (f blotField) at(gridX, gridY float64) float64 {
	fieldX := blotMargin + gridX*blotCell
	fieldY := blotMargin + gridY*blotCell
	x, y := int(math.Floor(fieldX)), int(math.Floor(fieldY))
	shareX, shareY := fieldX-float64(x), fieldY-float64(y)
	pixel := func(px, py int) float64 {
		if px < 0 || py < 0 || px >= blotFieldW || py >= blotFieldH {
			return 0
		}
		return float64(f[px*blotFieldH+py])
	}
	top := pixel(x, y)*(1-shareX) + pixel(x+1, y)*shareX
	bottom := pixel(x, y+1)*(1-shareX) + pixel(x+1, y+1)*shareX
	return top*(1-shareY) + bottom*shareY
}

// layerAt is the layer painted at a point of the grid: the last layer that
// reaches the point, or -1 for the page.
func (b *Blot) layerAt(gridX, gridY float64) int {
	for layer := blotLayers - 1; layer >= 0; layer-- {
		if b.fields[layer].at(gridX, gridY) >= blotEdge {
			return layer
		}
	}
	return -1
}

// bounds is the smallest box of the grid, in cells, that holds every layer.
func (b *Blot) bounds() (left, top, right, bottom float64) {
	minX, minY, maxX, maxY := blotFieldW, blotFieldH, -1, -1
	for _, field := range b.fields {
		for x := range blotFieldW {
			for y := range blotFieldH {
				if field[x*blotFieldH+y] < blotEdge {
					continue
				}
				minX, maxX = min(minX, x), max(maxX, x)
				minY, maxY = min(minY, y), max(maxY, y)
			}
		}
	}
	toGrid := func(pixel int) float64 { return float64(pixel-blotMargin) / blotCell }
	return toGrid(minX - 1), toGrid(minY - 1), toGrid(maxX + 2), toGrid(maxY + 2)
}
