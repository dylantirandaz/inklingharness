package presentation

import (
	"strings"
	"testing"
	"time"
)

const traceFrame = 33 * time.Millisecond

func traceTheme() Theme { return NewTheme(TrueColor, DarkBackground, AccentPlum).OnPaper(20, 22, 26) }

// barCells is the number of cells of a drawn trace that hold a tick.
func barCells(row string) int {
	return strings.Count(row, "┴") + strings.Count(row, "┸")
}

// The trace keeps its width, and without intermediate colors it is a plain
// hairline.
func TestTraceKeepsItsWidth(t *testing.T) {
	trace := NewTrace(48)
	for index := range 200 {
		trace.Mark(TraceReply, index%3)
		trace.Step(traceFrame)
		if got := CellWidth(Safe(trace.Draw(traceTheme()))); got != 48 {
			t.Fatalf("frame %d: trace is %d cells, want 48", index, got)
		}
	}
	if got := Safe(NewTrace(20).Draw(NewTheme(ANSI16, DarkBackground, AccentPlum))); got != strings.Repeat("─", 20) {
		t.Fatalf("trace without blends = %q", got)
	}
}

// The trace is honest about time: a second of streaming leaves bars, and a
// second of silence after it leaves an empty stretch, not more bars.
func TestTraceShowsSilenceAsAnEmptyStretch(t *testing.T) {
	theme := traceTheme()
	trace := NewTrace(48)
	for range 30 {
		trace.Mark(TraceReply, 2)
		trace.Step(traceFrame)
	}
	streamed := barCells(Safe(trace.Draw(theme)))
	for range 30 {
		trace.Step(traceFrame)
	}
	afterSilence := barCells(Safe(trace.Draw(theme)))
	if streamed < 8 || afterSilence > streamed {
		t.Fatalf("bars after a second of streaming %d, after a second of silence %d", streamed, afterSilence)
	}
}

// The head moves while the model is silent, so a wait still looks alive.
func TestTraceHeadMovesInSilence(t *testing.T) {
	theme := traceTheme()
	trace := NewTrace(48)
	first := strings.Index(Safe(trace.Draw(theme)), "╸")
	for range 10 {
		trace.Step(traceFrame)
	}
	if later := strings.Index(Safe(trace.Draw(theme)), "╸"); later <= first {
		t.Fatalf("the head stayed at %d after a third of a second (was %d)", later, first)
	}
}

// Color carries meaning only: reasoning is gray, and red appears only for a
// failure.
func TestTraceUsesRedOnlyForAFailure(t *testing.T) {
	theme := traceTheme()
	red := string(theme.appendColor(nil, palette[roleRed].color.on(theme.shade), false))
	draw := func(kind TraceMark) string {
		trace := NewTrace(48)
		trace.Mark(kind, 3)
		trace.Step(traceFrame)
		trace.Step(traceFrame)
		return trace.Draw(theme)
	}
	for _, kind := range []TraceMark{TraceReasoning, TraceReply, TraceTool} {
		if strings.Contains(draw(kind), red) {
			t.Fatalf("mark %d is drawn in red", kind)
		}
	}
	if !strings.Contains(draw(TraceFailure), red) {
		t.Fatal("a failure is not drawn in red")
	}
}

// One frame of the trace: a step and a draw.
func BenchmarkTraceFrame(b *testing.B) {
	theme := traceTheme()
	trace := NewTrace(48)
	b.ReportAllocs()
	for index := range b.N {
		trace.Mark(TraceMark(index%int(TraceFailure+1)), 1)
		trace.Step(traceFrame)
		_ = trace.Draw(theme)
	}
}
