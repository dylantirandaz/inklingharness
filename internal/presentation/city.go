package presentation

import (
	"hash/fnv"
	"math"
)

// CityPicture is the opening artwork: a panorama of the city of the
// Futurists, after the drawings of Antonio Sant'Elia for La Città Nuova
// (1914), seen from below as he drew it. The walls of every tower lean
// toward one point high above the picture, so the city soars. Towers step
// back as they rise, a red lift shaft runs up outside the main tower, and a
// bridge joins it to a neighbor. Planes of force, as Boccioni and Balla
// painted them, cut through the city from a corner: they are translucent,
// so a tower takes their color where a plane crosses it. Each project has
// its own city, from a hash of its path. In the opening the city rises as a
// wave from left to right, with the echo of each rising roof above it, and
// the planes sweep open.
type CityPicture struct {
	Columns, Rows int
	// Back to front: the distant skyline, the crowd of towers, the bridge,
	// the main tower, and its lift shaft; the planes cross all of them.
	distant []cityTower
	crowd   []cityTower
	main    cityTower
	shaft   cityShaft
	bridge  cityBlock
	planes  []cityPlane
	canvas  *inkCanvas
	groundY int
	// vanishX and vanishY are the point, in pixels, that every wall leans
	// toward: above the picture, so the city is seen from the street.
	vanishX, vanishY float64
	// originX and originY are the corner below the picture that the planes
	// of force start from, and reach is how far they run.
	originX, originY, reach float64
}

// cityBlock is a rectangle in pixels at the foot of the perspective: x from
// left to right-1 and y from top to bottom-1, with y growing down. Drawn, its
// sides lean toward the vanishing point.
type cityBlock struct{ left, right, top, bottom int }

// cityTower is a stack of tiers from the ground up, the mast on its roof (an
// empty block for none), and the window of the opening in which it rises.
type cityTower struct {
	tiers       []cityBlock
	mast        cityBlock
	rise        cityWindow
	shadowWidth int
}

// cityShaft is a lift shaft outside the main tower: a red column with a
// machine house on top, joined to the tower by walkways.
type cityShaft struct {
	column, house cityBlock
	walkways      []cityBlock
	rise          cityWindow
}

// cityPlane is a plane of force: a band from the origin along an angle on
// screen, halfWidth screen units to each side at its far end and half that
// at the origin, in one tone, that runs out during its window of the
// opening.
type cityPlane struct {
	angle, halfWidth float64
	tone             cityTone
	sweep            cityWindow
}

// pixelAspect is how much taller than wide a pixel of the canvas is on
// screen: a cell is about twice as tall as it is wide, and a pixel is a
// quarter of a cell. The planes measure in screen units, so a steep plane
// and a shallow one look equally thick.
const pixelAspect = 2.1

// cityTone is the color of a plane of force.
type cityTone uint8

const (
	toneRed cityTone = iota
	toneAmber
	toneBlue
)

// cityWindow is a part of the opening, as a start and a span of progress.
type cityWindow struct{ start, span float64 }

// at is how far the window has run at progress, from 0 to 1, eased so that
// a motion starts fast and settles, as a machine brakes.
func (w cityWindow) at(progress float64) float64 {
	share := min(max((progress-w.start)/w.span, 0), 1)
	return 1 - math.Pow(1-share, 3)
}

// cityRandom is a small deterministic generator, so a project always gets
// the same city.
type cityRandom struct{ state uint64 }

func (r *cityRandom) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	return mix(r.state)
}

// between is a whole number from low to high, both included.
func (r *cityRandom) between(low, high int) int {
	if high <= low {
		return low
	}
	return low + int(r.next()%uint64(high-low+1))
}

// A plane of force is nearly solid in the open sky, where its color must
// carry, and translucent where it crosses a tower, so the tower takes its
// color and still shows: the two forms pass through each other.
const (
	planeSkyAlpha   = 0.78
	planeCrossAlpha = 0.4
)

// NewCityPicture builds the city of directory for a box of columns by rows
// cells. It returns nil when the box is too small to hold a city.
func NewCityPicture(directory string, columns, rows int) *CityPicture {
	if columns < 24 || rows < 5 {
		return nil
	}
	hash := fnv.New64a()
	hash.Write([]byte(directory))
	random := &cityRandom{state: hash.Sum64()}
	width, height := 2*columns, 2*rows
	ground := height - 1
	picture := &CityPicture{Columns: columns, Rows: rows, canvas: newInkCanvas(columns, rows), groundY: ground}

	// The main tower stands near the middle and nearly reaches the top, with
	// tiers that step back and a mast. The walls lean toward a point high
	// above it.
	center := width * random.between(42, 58) / 100
	picture.vanishX, picture.vanishY = float64(center), -5*float64(height)
	baseWidth := max(width*random.between(10, 13)/100, 12)
	picture.main = stackTower(random, center, baseWidth, ground, height-random.between(3, 5), random.between(3, 4))
	top := picture.main.tiers[len(picture.main.tiers)-1]
	mastX := (top.left + top.right) / 2
	picture.main.mast = cityBlock{left: mastX, right: mastX + 1, top: max(top.top-random.between(3, 5), 0), bottom: top.top}
	picture.main.shadowWidth = 3
	base := picture.main.tiers[0]

	// The city rises as a wave from left to right, the way the work moves.
	wave := func(x int) cityWindow {
		return cityWindow{start: 0.05 + 0.35*float64(x)/float64(width), span: 0.35}
	}
	picture.main.rise = cityWindow{start: wave(center).start, span: 0.45}

	// A distant skyline of low blocks runs across the whole width.
	for x := 0; x < width; {
		blockWidth := random.between(4, 9)
		tower := stackTower(random, x+blockWidth/2, blockWidth, ground, height*random.between(16, 34)/100, random.between(1, 2))
		tower.rise = cityWindow{start: 0.02, span: 0.3}
		picture.distant = append(picture.distant, tower)
		x += blockWidth + random.between(0, 2)
	}

	// A crowd of towers fills the panorama on both sides of the main tower,
	// tallest near it, so the skyline peaks at the middle.
	for x := random.between(0, 3); x < width; {
		towerWidth := random.between(7, 14)
		middle := x + towerWidth/2
		if middle+towerWidth/2 >= base.left-3 && middle-towerWidth/2 <= base.right+6 {
			x = base.right + 7
			continue
		}
		distance := float64(middle-center) / float64(width)
		profile := 0.3 + 0.42*math.Exp(-distance*distance/(0.28*0.28))
		towerHeight := int(float64(height) * profile * float64(random.between(80, 110)) / 100)
		tower := stackTower(random, middle, towerWidth, ground, towerHeight, random.between(1, 3))
		tower.shadowWidth = 1
		tower.rise = wave(middle)
		picture.crowd = append(picture.crowd, tower)
		x += towerWidth + random.between(1, 4)
	}

	// The lift shaft runs up the right of the main tower, past its second
	// tier, with walkways to each tier that it passes.
	shaftTop := picture.main.tiers[min(1, len(picture.main.tiers)-1)].top - random.between(3, 5)
	shaftX := base.right + 2
	picture.shaft.column = cityBlock{left: shaftX, right: shaftX + 2, top: shaftTop, bottom: ground + 1}
	picture.shaft.house = cityBlock{left: shaftX - 1, right: shaftX + 3, top: shaftTop - 2, bottom: shaftTop}
	for _, tier := range picture.main.tiers {
		if tier.top <= shaftTop {
			break
		}
		y := tier.top + 1
		picture.shaft.walkways = append(picture.shaft.walkways, cityBlock{left: tier.right, right: shaftX, top: y, bottom: y + 1})
	}
	picture.shaft.rise = picture.main.rise

	// A bridge joins the main tower to the nearest tower on its left, below
	// the roof of both.
	for index := len(picture.crowd) - 1; index >= 0; index-- {
		neighbor := picture.crowd[index]
		if neighbor.tiers[0].right > base.left {
			continue
		}
		y := max(neighbor.tiers[len(neighbor.tiers)-1].top, base.top) + random.between(2, 4)
		if y+2 < ground {
			picture.bridge = cityBlock{left: neighbor.tiers[0].right - 1, right: base.left + 1, top: y, bottom: y + 2}
		}
		break
	}

	// The planes of force start below the left corner and fan up across the
	// whole city, each aimed at its own stretch of the top edge. A plane is a
	// band that widens a little as it runs, as a beam does, so it reads as a
	// line of force and not as a slab.
	picture.originX, picture.originY = -0.1*float64(width), 1.6*float64(height)
	picture.reach = math.Hypot(1.3*float64(width), 2.6*float64(height)*pixelAspect)
	tones := [...]cityTone{toneRed, toneBlue, toneAmber, toneRed, toneBlue}
	planes := random.between(4, 5)
	for index := range planes {
		share := (float64(index) + 0.3 + 0.4*float64(random.between(0, 100))/100) / float64(planes)
		landing := (0.1 + 1.1*share) * float64(width)
		picture.planes = append(picture.planes, cityPlane{
			angle:     math.Atan2(-picture.originY*pixelAspect, landing-picture.originX),
			halfWidth: float64(random.between(20, 34)) / 10,
			tone:      tones[index],
			sweep:     cityWindow{start: 0.04 * float64(index), span: 0.5},
		})
	}
	return picture
}

// stackTower builds a tower of tiers that step back on both sides, centered
// at center, with the base width, from ground up to height pixels.
func stackTower(random *cityRandom, center, baseWidth, ground, height, tierCount int) cityTower {
	// The lower tiers are taller, as the base of a building carries the rest.
	weights := make([]int, tierCount)
	total := 0
	for index := range weights {
		weights[index] = (tierCount-index)*3 + random.between(1, 3)
		total += weights[index]
	}
	tower := cityTower{}
	bottom, tierWidth := ground+1, baseWidth
	for index, weight := range weights {
		tierHeight := max(height*weight/total, 2)
		if index == tierCount-1 {
			tierHeight = max(bottom-(ground+1-height), 2)
		}
		half := tierWidth / 2
		tower.tiers = append(tower.tiers, cityBlock{left: center - half, right: center - half + tierWidth, top: bottom - tierHeight, bottom: bottom})
		bottom -= tierHeight
		tierWidth = max(tierWidth*random.between(60, 76)/100, 4)
	}
	return tower
}

// Draw paints the city at a progress from 0 to 1. At 0 the sky is empty; at
// 1 the city stands complete, in flat colors. It returns nil below 256
// colors.
func (p *CityPicture) Draw(t Theme, progress float64) []string {
	if t.depth < ANSI256 {
		return nil
	}
	p.canvas.reset()
	paper := t.paperColor()
	ink, _ := t.cityInk(0)
	_, distant := t.cityInk(0.2)
	_, crowdShadow := t.cityInk(0.45)
	_, mainShadow := t.cityInk(0.6)
	for _, tower := range p.distant {
		p.drawTower(tower, distant, distant, progress)
	}
	crowdFront := blend(paper, ink, 0.8)
	for _, tower := range p.crowd {
		p.drawTower(tower, crowdFront, crowdShadow, progress)
	}
	if p.bridge.right > p.bridge.left && progress >= 0.7 {
		p.fill(p.bridge, blue.on(t.shade))
	}
	p.drawTower(p.main, ink, mainShadow, progress)
	p.drawShaft(ink, progress)
	p.drawPlanes(t, progress)
	return t.drawCanvas(p.canvas)
}

// cityInk is the ink of the towers and a tone that lies depth of the way
// from the page to the ink.
func (t Theme) cityInk(depth float64) (ink, tone rgb) {
	ink = rgb{236, 233, 228}
	if t.shade == LightBackground {
		ink = rgb{22, 19, 17}
	}
	return ink, blend(t.paperColor(), ink, depth)
}

// lean is where x, at the foot of the perspective, lies at height y: walls
// lean toward the vanishing point as they rise.
func (p *CityPicture) lean(x float64, y int) float64 {
	return x + (p.vanishX-x)*float64(p.groundY+1-y)/(float64(p.groundY+1)-p.vanishY)
}

// span is the pixels that block covers at height y, after the lean.
func (p *CityPicture) span(block cityBlock, y int) (left, right int) {
	return int(math.Round(p.lean(float64(block.left), y))), int(math.Round(p.lean(float64(block.right), y)))
}

// drawTower paints a tower that has risen as far as progress allows. Above a
// rising roof, its echo fades, as Balla painted speed: a form repeated where
// it just was.
func (p *CityPicture) drawTower(tower cityTower, front, side rgb, progress float64) {
	risen := tower.rise.at(progress)
	if risen <= 0 {
		return
	}
	highest := tower.tiers[len(tower.tiers)-1].top
	if tower.mast.bottom > tower.mast.top {
		highest = tower.mast.top
	}
	roof := p.groundY + 1 - int(math.Round(risen*float64(p.groundY+1-highest)))
	const echo = 4
	paint := func(block cityBlock, shaded bool) {
		for y := max(block.top, roof-echo); y < block.bottom; y++ {
			alpha := 1.0
			if y < roof {
				if risen >= 1 {
					continue
				}
				alpha = 0.45 * float64(echo-(roof-y)) / echo
			}
			left, right := p.span(block, y)
			for x := left; x < right; x++ {
				color := front
				if shaded && x >= right-tower.shadowWidth {
					color = side
				}
				p.canvas.over(x, y, color, alpha)
			}
		}
	}
	for _, tier := range tower.tiers {
		paint(tier, true)
	}
	paint(tower.mast, false)
}

// drawShaft paints the lift shaft in Futurist red, rising with its tower, and
// its walkways once it stands.
func (p *CityPicture) drawShaft(ink rgb, progress float64) {
	risen := p.shaft.rise.at(progress)
	if risen <= 0 {
		return
	}
	roof := p.groundY + 1 - int(math.Round(risen*float64(p.groundY+1-p.shaft.house.top)))
	for _, block := range []cityBlock{p.shaft.column, p.shaft.house} {
		block.top = max(block.top, roof)
		p.fill(block, shaftRed)
	}
	if risen >= 1 {
		for _, walkway := range p.shaft.walkways {
			p.fill(walkway, ink)
		}
	}
}

// drawPlanes lays the planes of force over the city as far as they have run
// from their corner.
func (p *CityPicture) drawPlanes(t Theme, progress float64) {
	width, height := p.canvas.width(), p.canvas.height()
	for _, plane := range p.planes {
		reach := plane.sweep.at(progress) * p.reach
		if reach <= 0 {
			continue
		}
		var color rgb
		switch plane.tone {
		case toneRed:
			color = palette[roleRed].color.on(t.shade)
		case toneAmber:
			color = palette[roleAmber].color.on(t.shade)
		case toneBlue:
			color = blue.on(t.shade)
		}
		cos, sin := math.Cos(plane.angle), math.Sin(plane.angle)
		for y := range height {
			for x := range width {
				// The pixel in screen units, from the origin: how far along
				// the plane it lies, and how far to one side.
				dx, dy := float64(x)+0.5-p.originX, (float64(y)+0.5-p.originY)*pixelAspect
				along, across := dx*cos+dy*sin, math.Abs(dy*cos-dx*sin)
				if along < 0 || along > reach || across > plane.halfWidth*(0.5+0.5*along/p.reach) {
					continue
				}
				alpha := planeSkyAlpha
				if p.canvas.pixels[y*width+x].alpha > 0 {
					alpha = planeCrossAlpha
				}
				p.canvas.over(x, y, color, alpha)
			}
		}
	}
}

// fill paints a block in one color, leaning with the perspective.
func (p *CityPicture) fill(block cityBlock, color rgb) {
	for y := block.top; y < block.bottom; y++ {
		left, right := p.span(block, y)
		for x := left; x < right; x++ {
			p.canvas.over(x, y, color, 1)
		}
	}
}
