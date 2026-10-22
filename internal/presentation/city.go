package presentation

import (
	"hash/fnv"
	"math"
)

// CityPicture is the opening artwork: a city of the Futurists, after the
// drawings of Antonio Sant'Elia for La Città Nuova (1914). Towers step back as
// they rise, a lift shaft runs up outside the main tower, a bridge joins two
// towers, and the lines of force of Boccioni and Balla cut the sky behind
// them as planes of light. Each project has its own city, from a hash of its
// path. In the opening the city rises from the ground, the far towers first,
// with the echo of each rising roof above it, and the planes sweep in.
type CityPicture struct {
	Columns, Rows int
	// Back to front: the planes of force, the distant skyline, the far
	// towers, the bridge, the main tower, and its lift shaft.
	forces   []cityForce
	distant  []cityTower
	far      []cityTower
	main     cityTower
	shaft    cityShaft
	bridge   cityBlock
	canvas   *inkCanvas
	groundY  int
	diagonal float64
	// originX and originY are the point, in pixels, that the planes of force
	// come from: above and outside one top corner.
	originX, originY float64
}

// cityBlock is a rectangle in pixels: x from left to right-1 and y from top
// to bottom-1, with y growing down.
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

// cityForce is a plane of force: a wedge from a point above the picture,
// between two angles, in the color of its tone.
type cityForce struct {
	fromAngle, toAngle float64
	tone               cityTone
	sweep              cityWindow
}

// cityTone is the color of a plane of force.
type cityTone uint8

const (
	toneBlue cityTone = iota
	toneRed
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

// The point that the planes of force come from, as a share of the picture:
// above and outside one top corner.
const (
	forceOriginX = -0.12
	forceOriginY = -0.6
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
	picture := &CityPicture{
		Columns: columns, Rows: rows, canvas: newInkCanvas(columns, rows), groundY: ground,
		diagonal: math.Hypot(float64(width), float64(height)) * 1.6,
	}

	// The main tower stands off the middle and nearly reaches the top. Its
	// tiers step back on both sides as it rises, and a mast crowns it.
	center := width * random.between(40, 56) / 100
	baseWidth := max(width*random.between(18, 22)/100, 10)
	picture.main = stackTower(random, center, baseWidth, ground, height-random.between(4, 6), random.between(3, 4))
	top := picture.main.tiers[len(picture.main.tiers)-1]
	mastX := (top.left + top.right) / 2
	picture.main.mast = cityBlock{left: mastX, right: mastX + 1, top: max(top.top-random.between(3, 5), 0), bottom: top.top}
	picture.main.rise = cityWindow{start: 0.25, span: 0.5}
	picture.main.shadowWidth = 2
	base := picture.main.tiers[0]

	// A distant skyline of low blocks runs across the whole width, so the
	// city has depth and the picture has a ground.
	for x := 0; x < width; {
		blockWidth := random.between(4, 9)
		tower := stackTower(random, x+blockWidth/2, blockWidth, ground, height*random.between(16, 38)/100, random.between(1, 2))
		tower.rise = cityWindow{start: 0.02 * float64(x) / float64(width), span: 0.3}
		picture.distant = append(picture.distant, tower)
		x += blockWidth + random.between(0, 2)
	}

	// Far towers stand on both sides of the main tower, spread to the edges
	// of the picture, lower than it, and rise before it.
	leftRoom, rightRoom := base.left-2, width-base.right-2
	for side, room := range [2]int{leftRoom, rightRoom} {
		count := max(min(room/12, 3), 1)
		for index := range count {
			farWidth := max(width*random.between(8, 12)/100, 6)
			share := (float64(index) + 0.5) / float64(count)
			farCenter := base.left - 2 - int(share*float64(room))
			if side == 1 {
				farCenter = base.right + 2 + int(share*float64(room))
			}
			farCenter = min(max(farCenter, farWidth/2), width-farWidth/2-1)
			// Towers nearer the main tower are taller, so the skyline peaks
			// at the middle like a crowd of buildings.
			farHeight := height * (random.between(40, 52) + int(20*(1-share))) / 100
			tower := stackTower(random, farCenter, farWidth, ground, farHeight, random.between(1, 3))
			tower.rise = cityWindow{start: 0.08 + 0.05*float64(len(picture.far)), span: 0.4}
			tower.shadowWidth = 1
			picture.far = append(picture.far, tower)
		}
	}

	// The lift shaft runs up one side of the main tower, past its second
	// tier, with walkways to each tier that it passes.
	leftSide := random.between(0, 1) == 0
	shaftTop := picture.main.tiers[min(1, len(picture.main.tiers)-1)].top - random.between(3, 5)
	shaftX := base.right + 2
	if leftSide {
		shaftX = base.left - 4
	}
	picture.shaft.column = cityBlock{left: shaftX, right: shaftX + 2, top: shaftTop, bottom: ground + 1}
	picture.shaft.house = cityBlock{left: shaftX - 1, right: shaftX + 3, top: shaftTop - 2, bottom: shaftTop}
	for _, tier := range picture.main.tiers {
		if tier.top <= shaftTop {
			break
		}
		y := tier.top + 1
		walkway := cityBlock{left: tier.right, right: shaftX, top: y, bottom: y + 1}
		if leftSide {
			walkway = cityBlock{left: shaftX + 2, right: tier.left, top: y, bottom: y + 1}
		}
		picture.shaft.walkways = append(picture.shaft.walkways, walkway)
	}
	picture.shaft.rise = cityWindow{start: 0.3, span: 0.45}

	// A bridge joins the main tower to the nearest far tower on the side
	// away from the shaft, below the roof of both.
	for _, tower := range picture.far {
		farBase := tower.tiers[0]
		farTop := tower.tiers[len(tower.tiers)-1].top
		awayRight := leftSide && farBase.left > base.right
		awayLeft := !leftSide && farBase.right < base.left
		if !awayRight && !awayLeft {
			continue
		}
		y := max(farTop, base.top) + random.between(2, 4)
		if y+2 >= ground {
			continue
		}
		picture.bridge = cityBlock{left: base.right, right: farBase.left + 1, top: y, bottom: y + 2}
		if awayLeft {
			picture.bridge = cityBlock{left: farBase.right - 1, right: base.left, top: y, bottom: y + 2}
		}
		break
	}

	// Planes of force fan out from a point above one corner and cut across
	// the sky, in alternate blue and red, each a few degrees wide.
	picture.originX, picture.originY = forceOriginX*float64(width), forceOriginY*float64(height)
	if random.between(0, 1) == 0 {
		picture.originX = (1 - forceOriginX) * float64(width)
	}
	// Each plane lands on its own stretch of the ground, so the planes cross
	// the whole sky and not only the corner they come from.
	planes := random.between(3, 4)
	for index := range planes {
		share := (float64(index) + 0.25 + 0.5*float64(random.between(0, 100))/100) / float64(planes)
		landing := share * float64(width)
		half := float64(width) * float64(random.between(3, 6)) / 100
		dy := float64(ground) - picture.originY
		first := math.Atan2(dy, landing+half-picture.originX)
		second := math.Atan2(dy, landing-half-picture.originX)
		tone := toneBlue
		if index%2 == 1 {
			tone = toneRed
		}
		picture.forces = append(picture.forces, cityForce{
			fromAngle: min(first, second), toAngle: max(first, second), tone: tone,
			sweep: cityWindow{start: 0.06 * float64(index), span: 0.4},
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
	ink, shadow := t.cityInk(0.62)
	_, farShadow := t.cityInk(0.42)
	_, distant := t.cityInk(0.2)
	p.drawForces(t, paper, progress)
	for _, tower := range p.distant {
		p.drawTower(tower, distant, distant, progress)
	}
	for _, tower := range p.far {
		p.drawTower(tower, blend(paper, ink, 0.78), farShadow, progress)
	}
	if p.bridge.right > p.bridge.left && progress >= 0.72 {
		p.fill(p.bridge, blue.on(t.shade))
	}
	p.drawTower(p.main, ink, shadow, progress)
	p.drawShaft(ink, progress)
	return t.drawCanvas(p.canvas)
}

// cityInk is the ink of the towers and the tone of their shaded sides,
// which is depth of the way from the page to the ink.
func (t Theme) cityInk(depth float64) (ink, shade rgb) {
	ink = rgb{236, 233, 228}
	if t.shade == LightBackground {
		ink = rgb{22, 19, 17}
	}
	return ink, blend(t.paperColor(), ink, depth)
}

// drawForces paints the planes of force as far as they have swept from
// their point, as faint wedges of color.
func (p *CityPicture) drawForces(t Theme, paper rgb, progress float64) {
	width, height := p.canvas.width(), p.canvas.height()
	originX, originY := p.originX, p.originY
	for _, force := range p.forces {
		reach := force.sweep.at(progress) * p.diagonal
		if reach <= 0 {
			continue
		}
		color := blend(paper, blue.on(t.shade), 0.3)
		if force.tone == toneRed {
			color = blend(paper, palette[roleRed].color.on(t.shade), 0.26)
		}
		for y := range height {
			for x := range width {
				dx, dy := float64(x)+0.5-originX, float64(y)+0.5-originY
				if angle := math.Atan2(dy, dx); angle < force.fromAngle || angle > force.toAngle || math.Hypot(dx, dy) > reach {
					continue
				}
				p.canvas.over(x, y, color, 1)
			}
		}
	}
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
			for x := block.left; x < block.right; x++ {
				color := front
				if shaded && x >= block.right-tower.shadowWidth {
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

// fill paints a block in one color.
func (p *CityPicture) fill(block cityBlock, color rgb) {
	for y := block.top; y < block.bottom; y++ {
		for x := block.left; x < block.right; x++ {
			p.canvas.over(x, y, color, 1)
		}
	}
}
