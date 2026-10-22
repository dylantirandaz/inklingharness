package terminal

import (
	"math"
	"strings"
	"time"

	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// Style is how a Screen draws.
type Style struct {
	Depth  presentation.ColorDepth
	Accent presentation.Accent
	// Motion animates the banner, streamed text, activity mark, and context
	// count. Pending text and elapsed seconds still update when motion is
	// off.
	Motion bool
}

const (
	// smoothInterval paces short effects that must look fluid: the banner,
	// streamed text and the context count.
	smoothInterval = 33 * time.Millisecond
	// steadyInterval paces the activity mark.
	steadyInterval = 66 * time.Millisecond
	// stillInterval updates the elapsed seconds when motion is off.
	stillInterval = time.Second

	// introDuration lets the ink of the opening artwork soak in and dry. The
	// composer takes input while it plays; a short soak settles the screen
	// sooner and writes half the bytes of a long one.
	introDuration = 1000 * time.Millisecond
	wetDuration   = 240 * time.Millisecond
	gaugeDuration = 450 * time.Millisecond
	expertTick    = 120 * time.Millisecond

	// minimumLiveRows is the room that the banner must leave for the
	// composer while it plays.
	minimumLiveRows = 4
)

// effects is the state of the animations. Every effect is a function of the
// time, not of a frame count, so a late frame never slows an effect down.
type effects struct {
	motion bool
	epoch  time.Time

	// intro draws the opening banner at a progress from 0 to 1 while it
	// plays; nil when no banner plays. Frames are drawn on demand, so no
	// frame is computed or held before the screen shows it.
	intro   func(progress float64) string
	introAt time.Time

	// The streamed preview is the reply prefix and the partial line. wet holds
	// the runs of the line that have not dried yet, oldest first.
	previewPrefix, previewBody string
	wet                        []wetRun

	toolRunning bool

	use       presentation.ContextUse
	haveUse   bool
	gaugeFrom float64
	gaugeAt   time.Time

	// trace records the stream on the working row; nil until the first
	// frame with room for it, and always nil without motion or intermediate
	// colors. traceAt is the time of its last step.
	trace   *presentation.Trace
	traceAt time.Time
}

// wetRun is the part of the preview body from start to the next run, which
// arrived at one time.
type wetRun struct {
	start int
	at    time.Time
}

// Intro plays the opening banner: frame draws it at a progress from 0, empty,
// to 1, final. Every progress must have the same number of rows. The final
// frame goes to scrollback when the animation ends, when other output
// arrives, or at Close. Without motion, or without room for the composer
// under it, the final frame goes to scrollback at once.
func (s *Screen) Intro(frame func(progress float64) string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if frame == nil || s.closed || s.fault != nil {
		return
	}
	final := frame(1)
	if !s.effects.motion || s.rows-strings.Count(final, "\n")-1 < minimumLiveRows {
		s.writeLocked([]byte(final + "\n"))
		return
	}
	s.effects.intro, s.effects.introAt = frame, time.Now()
	s.layoutDirty = true
	s.drawLocked()
	s.wake.signal()
}

func (s *Screen) introFrameLocked(now time.Time) []string {
	if s.effects.intro == nil {
		return nil
	}
	progress := min(float64(now.Sub(s.effects.introAt))/float64(introDuration), 1)
	return strings.Split(s.effects.intro(progress), "\n")
}

// commitIntroLocked moves a banner that still plays to scrollback in its final
// form.
func (s *Screen) commitIntroLocked() {
	frame := s.effects.intro
	if frame == nil {
		return
	}
	s.effects.intro = nil
	s.layoutDirty = true
	s.writeLocked([]byte(frame(1) + "\n"))
}

// StreamReasoning keeps the latest partial line for the next frame. Formatting
// and painting happen on that frame, not on each streamed chunk.
func (s *Screen) StreamReasoning(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasoningText = text
	if !s.reasoningDirty {
		s.reasoningDirty = true
		s.wake.signal()
	}
}

// StreamPreview shows the partial line of a streamed reply after prefix. Text
// added since the last call starts pale and dries to ink. A body that does
// not continue the last one is a new line, all of it fresh.
func (s *Screen) StreamPreview(prefix, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reasoningText, s.reasoningDirty = "", false
	if prefix == s.effects.previewPrefix && body == s.effects.previewBody && s.preview == "" {
		return
	}
	start := len(s.effects.previewBody)
	if s.preview != "" || prefix != s.effects.previewPrefix || !strings.HasPrefix(body, s.effects.previewBody) {
		s.effects.wet = s.effects.wet[:0]
		start = 0
	}
	if len(body) > start && s.effects.motion && s.theme.Blends() {
		s.effects.wet = append(s.effects.wet, wetRun{start: start, at: time.Now()})
	}
	if len(body) > start {
		s.markLocked(presentation.TraceReply, 1)
	}
	s.preview = ""
	s.effects.previewPrefix, s.effects.previewBody = prefix, body
	s.layoutDirty = true
	s.drawLocked()
	s.wake.signal()
}

// previewLocked is the preview row text at now. Runs that have dried are
// dropped, so a long line keeps only its fresh tail as separate runs.
func (s *Screen) previewLocked(now time.Time) string {
	if s.preview != "" || s.effects.previewBody == "" {
		return s.preview
	}
	dried := 0
	for dried < len(s.effects.wet) && now.Sub(s.effects.wet[dried].at) >= wetDuration {
		dried++
	}
	// Compact in place, then read only the compacted slice: the old
	// subslice would now see shifted entries.
	s.effects.wet = append(s.effects.wet[:0], s.effects.wet[dried:]...)
	wet := s.effects.wet
	body := s.effects.previewBody
	if len(wet) == 0 {
		return s.effects.previewPrefix + body
	}
	var out strings.Builder
	out.WriteString(s.effects.previewPrefix)
	out.WriteString(body[:wet[0].start])
	// Neighbors at the same drying level share one color, and few levels
	// leave many frames unchanged, so the diff skips them.
	for index := 0; index < len(wet); {
		level := wetLevel(now.Sub(wet[index].at))
		next := index + 1
		for next < len(wet) && wetLevel(now.Sub(wet[next].at)) == level {
			next++
		}
		end := len(body)
		if next < len(wet) {
			end = wet[next].start
		}
		segment := body[wet[index].start:end]
		if level == 0 {
			out.WriteString(segment)
		} else {
			out.WriteString(s.theme.Wet(segment, float64(level)/wetLevels))
		}
		index = next
	}
	return out.String()
}

// wetLevels is the number of steps from fresh to dry.
const wetLevels = 6

// wetLevel is the drying step of text of age: wetLevels when it just arrived,
// 0 when it is dry. The fade eases out, so most of the change comes early.
func wetLevel(age time.Duration) int {
	remaining := 1 - min(float64(age)/float64(wetDuration), 1)
	return int(math.Ceil(remaining * remaining * wetLevels))
}

func (s *Screen) wetActiveLocked(now time.Time) bool {
	wet := s.effects.wet
	return len(wet) > 0 && now.Sub(wet[len(wet)-1].at) < wetDuration
}

// tickLocked sets the activity mark's phase; without motion it stays still.
func (s *Screen) tickLocked(now time.Time) int {
	if !s.effects.motion {
		return 0
	}
	return int(now.Sub(s.effects.epoch) / expertTick)
}

// SetContext sets what the footer reports. A new context estimate counts to
// its value, and the gauge squares fill one by one.
func (s *Screen) SetContext(use presentation.ContextUse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setContextLocked(use)
}

func (s *Screen) setContextLocked(use presentation.ContextUse) {
	if s.effects.haveUse && use == s.effects.use {
		return
	}
	now := time.Now()
	if s.effects.haveUse && s.effects.motion && use.Tokens != s.effects.use.Tokens {
		s.effects.gaugeFrom, s.effects.gaugeAt = s.shownTokensLocked(now), now
	} else {
		s.effects.gaugeFrom, s.effects.gaugeAt = float64(use.Tokens), time.Time{}
	}
	s.effects.use, s.effects.haveUse = use, true
	s.layoutDirty = true
	s.drawLocked()
	s.wake.signal()
}

// SetContextTokens updates only the context estimate of the footer, for a
// count that arrives between turns while the rest of the report stands.
func (s *Screen) SetContextTokens(tokens int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.effects.haveUse {
		return
	}
	use := s.effects.use
	use.Tokens = tokens
	s.setContextLocked(use)
}

// shownTokensLocked eases from the previous estimate to the current one.
func (s *Screen) shownTokensLocked(now time.Time) float64 {
	target := float64(s.effects.use.Tokens)
	if s.effects.gaugeAt.IsZero() {
		return target
	}
	progress := float64(now.Sub(s.effects.gaugeAt)) / float64(gaugeDuration)
	if progress >= 1 {
		return target
	}
	eased := 1 - math.Pow(1-progress, 3)
	return s.effects.gaugeFrom + (target-s.effects.gaugeFrom)*eased
}

func (s *Screen) gaugeMovingLocked(now time.Time) bool {
	return !s.effects.gaugeAt.IsZero() && now.Sub(s.effects.gaugeAt) < gaugeDuration
}

// footerPartsLocked is the context footer at now, as the model that works
// and the context that it uses.
func (s *Screen) footerPartsLocked(now time.Time) (model, usage string) {
	if !s.effects.haveUse {
		return "", ""
	}
	return presentation.FooterModel(s.effects.use, s.theme),
		presentation.FooterUsage(s.effects.use, int(math.Round(s.shownTokensLocked(now))), s.theme)
}

// footerTextLocked is the two parts of the footer on one line.
func (s *Screen) footerTextLocked(now time.Time) string {
	model, usage := s.footerPartsLocked(now)
	if model == "" {
		return ""
	}
	return model + s.theme.Hairline(" · ") + usage
}

// frameIntervalLocked is the time between pending preview or animation frames,
// or 0 when the screen may sleep until input.
func (s *Screen) frameIntervalLocked(now time.Time) time.Duration {
	if s.view != nil {
		return 0
	}
	if s.reasoningDirty {
		return smoothInterval
	}
	busy := s.mode == modeBusy && s.status != ""
	if !s.effects.motion {
		if busy {
			return stillInterval
		}
		return 0
	}
	switch {
	case s.effects.intro != nil, s.wetActiveLocked(now), s.gaugeMovingLocked(now):
		return smoothInterval
	case busy, s.mode == modePrompt && s.background != "":
		return steadyInterval
	default:
		return 0
	}
}

// introDoneLocked reports a banner that has played to its end; the next draw
// moves it to scrollback.
func (s *Screen) introDoneLocked(now time.Time) bool {
	return s.effects.intro != nil && now.Sub(s.effects.introAt) >= introDuration
}
