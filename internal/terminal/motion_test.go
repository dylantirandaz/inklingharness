package terminal

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// Output that arrives while the banner plays must follow the final banner in
// scrollback; the banner must not stay in the live rows.
func TestIntroCommitsBeforeOtherOutput(t *testing.T) {
	s, take := renderScreen(t, modePrompt, 20, 80)
	s.effects = effects{motion: true, epoch: time.Now()}
	s.Intro([]string{"first frame\nfirst frame", "final banner\nfinal banner"})
	if got := take(); !strings.Contains(got, "first frame") || strings.Contains(got, "final banner") {
		t.Fatalf("intro did not start at its first frame: %q", got)
	}
	if s.frameIntervalLocked(time.Now()) != smoothInterval {
		t.Fatal("a playing intro does not animate")
	}
	if _, err := s.Write([]byte("after\n")); err != nil {
		t.Fatal(err)
	}
	got := take()
	final, after := strings.Index(got, "final banner"), strings.Index(got, "after")
	if final < 0 || after < final {
		t.Fatalf("scrollback order is wrong: %q", got)
	}
	for _, row := range s.live {
		if strings.Contains(row, "banner") || strings.Contains(row, "frame") {
			t.Fatalf("banner stayed in the live rows: %q", s.live)
		}
	}

	still, take := renderScreen(t, modePrompt, 20, 80)
	still.Intro([]string{"first frame", "final banner"})
	if got := take(); strings.Contains(got, "first frame") || !strings.Contains(got, "final banner") {
		t.Fatalf("intro without motion did not go straight to its final frame: %q", got)
	}
}

// New text starts pale and dries to plain ink; text that does not continue
// the line is fresh as a whole.
func TestStreamedPreviewDries(t *testing.T) {
	s, _ := renderScreen(t, modeBusy, 10, 80)
	s.effects = effects{motion: true, epoch: time.Now()}
	s.theme = presentation.NewTheme(presentation.TrueColor, presentation.DarkBackground, presentation.AccentPlum)
	s.StreamPreview("● ", "hello")
	s.StreamPreview("● ", "hello world")
	now := time.Now()
	wet := s.previewLocked(now)
	if presentation.Safe(wet) != "● hello world" || !strings.Contains(wet, "\x1b[38;2;") {
		t.Fatalf("fresh preview = %q", wet)
	}
	if !s.wetActiveLocked(now) || s.frameIntervalLocked(now) != smoothInterval {
		t.Fatal("drying text does not animate")
	}
	if dry := s.previewLocked(now.Add(wetDuration)); dry != "● hello world" {
		t.Fatalf("dry preview = %q", dry)
	}
	if len(s.effects.wet) != 0 {
		t.Fatalf("dry runs were kept: %v", s.effects.wet)
	}
	s.StreamPreview("● ", "next line")
	if len(s.effects.wet) != 1 || s.effects.wet[0].start != 0 {
		t.Fatalf("a new line is not fresh as a whole: %v", s.effects.wet)
	}
	s.SetPreview("")
	if s.previewLocked(time.Now()) != "" || len(s.effects.wet) != 0 {
		t.Fatal("clearing the preview kept streamed text")
	}
}

// When several runs dry between two frames, dropping them must leave the
// fresh runs whole and in order. Before a fix, two dried runs out of five
// made the preview slice out of range.
func TestStreamedPreviewDropsDriedRunsInOrder(t *testing.T) {
	s, _ := renderScreen(t, modeBusy, 10, 120)
	s.effects = effects{motion: true, epoch: time.Now()}
	s.theme = presentation.NewTheme(presentation.TrueColor, presentation.DarkBackground, presentation.AccentPlum)
	now := time.Now()
	for dried := range 5 {
		s.effects.previewPrefix, s.effects.previewBody = "● ", "aaaabbbbccccddddeeee"
		s.effects.wet = s.effects.wet[:0]
		for index := range 5 {
			at := now
			if index < dried {
				at = now.Add(-wetDuration)
			}
			s.effects.wet = append(s.effects.wet, wetRun{start: 4 * index, at: at})
		}
		if got := presentation.Safe(s.previewLocked(now)); got != "● aaaabbbbccccddddeeee" {
			t.Fatalf("%d dried runs: preview = %q", dried, got)
		}
		if len(s.effects.wet) != 5-dried || (len(s.effects.wet) > 0 && s.effects.wet[0].start != 4*dried) {
			t.Fatalf("%d dried runs: kept %v", dried, s.effects.wet)
		}
	}
}

// An approval animates its bar, so entering it must wake a read loop that
// sleeps without a deadline at an idle screen.
func TestApprovalWakesTheReadLoop(t *testing.T) {
	s, _ := renderScreen(t, modeBusy, 10, 80)
	s.effects = effects{motion: true, epoch: time.Now()}
	s.theme = presentation.NewTheme(presentation.TrueColor, presentation.DarkBackground, presentation.AccentPlum)
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { input.Close(); writer.Close() })
	wake, err := newWakePipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wake.close)
	s.input, s.wake, s.ctx = input, wake, context.Background()
	if interval := s.frameIntervalLocked(time.Now()); interval != 0 {
		t.Fatalf("idle screen animates every %v", interval)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Choose(ctx); close(done) }()
	_, woken, err := waitReadable(int(input.Fd()), wake.read, time.Second)
	cancel()
	<-done
	if err != nil || !woken {
		t.Fatalf("approval did not wake the read loop: woken %t, %v", woken, err)
	}
	s.mode = modeChoice
	if interval := s.frameIntervalLocked(time.Now()); interval != steadyInterval {
		t.Fatalf("approval animates every %v", interval)
	}
}

// The end of a turn keeps the activity row for the hand-off, then removes it
// and lets the prompt sleep.
func TestHandoffEndsTheActivityRow(t *testing.T) {
	s, _ := renderScreen(t, modeBusy, 10, 80)
	s.effects = effects{motion: true, epoch: time.Now()}
	s.SetStatus("Thinking")
	s.SetStatus("")
	s.mode = modePrompt
	s.layoutDirty = true
	s.drawLocked()
	if s.activityRow < 0 || !strings.Contains(s.live[s.activityRow], "●") {
		t.Fatalf("hand-off row missing: %q", s.live)
	}
	later := time.Now().Add(handoffDuration)
	if s.frameIntervalLocked(time.Now()) != smoothInterval || s.frameIntervalLocked(later) != 0 {
		t.Fatal("hand-off does not animate and then stop")
	}
	s.effects.handoffAt = s.effects.handoffAt.Add(-handoffDuration)
	s.layoutDirty = true
	s.drawLocked()
	if s.activityRow >= 0 {
		t.Fatalf("activity row stayed after the hand-off: %q", s.live)
	}
}

// A new context estimate counts up to its value; without motion it jumps.
func TestGaugeEasesToNewEstimate(t *testing.T) {
	for _, motion := range []bool{true, false} {
		s, _ := renderScreen(t, modePrompt, 10, 100)
		s.effects = effects{motion: motion, epoch: time.Now()}
		use := presentation.ContextUse{Model: "thinkingmachines/inkling-small", Effort: "high", Tokens: 0, CompactTokens: 200_000}
		s.SetContext(use)
		use.Tokens = 100_000
		s.SetContext(use)
		now := time.Now()
		shown := s.shownTokensLocked(now.Add(gaugeDuration / 3))
		if motion && (shown <= 0 || shown >= 100_000) {
			t.Fatalf("eased value = %v", shown)
		}
		if !motion && shown != 100_000 {
			t.Fatalf("value without motion = %v", shown)
		}
		if got := s.shownTokensLocked(now.Add(gaugeDuration)); got != 100_000 {
			t.Fatalf("final value = %v", got)
		}
		if footer := presentation.Safe(s.footerTextLocked(now.Add(gaugeDuration))); !strings.Contains(footer, "100k / 200k") {
			t.Fatalf("footer = %q", footer)
		}
	}
}
