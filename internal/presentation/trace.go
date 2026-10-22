package presentation

import (
	"math"
	"strings"
	"time"
)

// TraceMark is the kind of event that the trace records. The marks of a
// step take the colors of its step mark, so the trace and the score of a turn
// speak one language. A later mark outranks an earlier one in a cell.
type TraceMark uint8

const (
	// TraceReasoning is a streamed piece of reasoning.
	TraceReasoning TraceMark = iota
	// TraceReply is a streamed piece of the reply.
	TraceReply
	// TraceRead, TraceChange, and TraceRun are a finished step of that kind.
	TraceRead
	TraceChange
	TraceRun
	// TraceFailure is a step that failed.
	TraceFailure
)

// TraceOf is the mark of a finished step of kind.
func TraceOf(kind StepKind) TraceMark {
	switch kind {
	case StepRead:
		return TraceRead
	case StepChange:
		return TraceChange
	case StepRun:
		return TraceRun
	case StepFailed:
		return TraceFailure
	}
	panic("presentation: unknown step kind")
}

// Trace records the stream of a turn as a ticker tape, on one row of cells.
// A write head moves across at a constant pace and wraps at the end, erasing
// a short gap ahead of itself. Every slice of time in which a piece of the
// stream arrived gets a fine tick on the hairline, so the rhythm of the model
// is the pattern: dense while tokens pour in, open while it waits. A tool is
// a taller tick in the model color, and only a failure is red. Ticks fade
// toward the hairline as the head moves away from them. Nothing is filled,
// and nothing moves but the head.
type Trace struct {
	cells []traceCell
	// head is the position of the write head, in cells, from 0 to the width.
	head    float64
	pending [TraceFailure + 1]int
	line    []byte
}

type traceCell struct {
	marked bool
	mark   TraceMark
	// age is how far, in cells, the head has moved since it wrote the cell.
	age float64
}

const (
	// tracePace is the speed of the head, in cells a second: a cell is a
	// tenth of a second, fine enough to see the rhythm of the stream.
	tracePace = 10.0
	// traceGap is the width of the erased gap ahead of the head.
	traceGap = 2
	// traceFadeSteps is how many tones a tick takes on its way to the
	// hairline.
	traceFadeSteps = 3
)

// NewTrace makes a trace of columns cells.
func NewTrace(columns int) *Trace {
	return &Trace{cells: make([]traceCell, max(columns, 1))}
}

// Columns is the width of the trace in cells.
func (t *Trace) Columns() int { return len(t.cells) }

// Mark records count events of a kind in the cell under the head.
func (t *Trace) Mark(kind TraceMark, count int) {
	t.pending[kind] += count
}

// Step moves the head forward by elapsed time, at most half a second, so a
// long pause leaves an open stretch, not a wiped trace.
func (t *Trace) Step(elapsed time.Duration) {
	distance := min(elapsed.Seconds(), 0.5) * tracePace
	if distance <= 0 {
		return
	}
	for index := range t.cells {
		t.cells[index].age += distance
	}
	// The strongest kind of the pending events names the cell under the head.
	under := &t.cells[int(t.head)%len(t.cells)]
	for kind := TraceReasoning; kind <= TraceFailure; kind++ {
		if t.pending[kind] > 0 {
			if !under.marked || under.age >= 1 {
				*under = traceCell{marked: true, mark: kind}
			}
			under.mark = max(under.mark, kind)
			t.pending[kind] = 0
		}
	}
	t.head = math.Mod(t.head+distance, float64(len(t.cells)))
	for offset := range traceGap {
		t.cells[(int(t.head)+1+offset)%len(t.cells)].marked = false
	}
}

// Draw paints the trace as one row in theme. The head is a short bright tick
// at the leading edge, just ahead of the cell that it writes, so the newest
// mark shows at once and the trace moves while the model is silent. Below 256
// colors it is the hairline.
func (t *Trace) Draw(theme Theme) string {
	if !theme.Blends() {
		return theme.Hairline(strings.Repeat("─", len(t.cells)))
	}
	hairline := palette[roleHairline].color.on(theme.shade)
	span := float64(len(t.cells))
	head := (int(t.head) + 1) % len(t.cells)
	line := t.line[:0]
	var drawn rgb
	haveColor := false
	for index, cell := range t.cells {
		color, glyph := hairline, "─"
		switch {
		case index == head:
			color, glyph = accents[theme.accent].color.on(theme.shade), "╸"
		case cell.marked:
			// The stream leaves slanted strokes, lines of force along the
			// tape; a finished step leaves the wedge of its mark.
			glyph = "╱"
			if cell.mark >= TraceRead {
				glyph = "▶"
			}
			// A tick keeps its tone for a third of the sweep, then fades in
			// a few steps, so neighbors share a color and a frame stays small.
			fade := min(max((cell.age/span-0.33)/0.67, 0), 1)
			color = blend(theme.traceColor(cell.mark), hairline, 0.85*math.Round(fade*traceFadeSteps)/traceFadeSteps)
		}
		if !haveColor || color != drawn {
			line = theme.appendColor(line, color, false)
			drawn, haveColor = color, true
		}
		line = append(line, glyph...)
	}
	line = append(line, reset...)
	t.line = line
	return string(line)
}

// traceColor is the color of a kind of mark: the color of its step's mark.
func (t Theme) traceColor(mark TraceMark) rgb {
	switch mark {
	case TraceReasoning:
		return palette[roleGraphite].color.on(t.shade)
	case TraceReply, TraceRun:
		return palette[roleInk].color.on(t.shade)
	case TraceRead:
		return palette[roleBlue].color.on(t.shade)
	case TraceChange:
		return palette[roleGreen].color.on(t.shade)
	case TraceFailure:
		return palette[roleRed].color.on(t.shade)
	}
	panic("presentation: unknown trace mark")
}
