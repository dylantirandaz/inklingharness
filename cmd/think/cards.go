package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/agent"
	"github.com/dylantirandaz/inklingharness/internal/anthropic"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
	"github.com/dylantirandaz/inklingharness/internal/tools"
)

// The views of /status and /context are cards like the model cards of the
// Inkling page: a title, a waffle of squares, and rows of label and value.

const (
	// maxCardWidth keeps a card readable on a wide terminal.
	maxCardWidth = 72
	// minCardWidth is the narrowest card that has room for a waffle.
	minCardWidth = 40
	waffleRows   = 3
	// maxWaffleColumns bounds the waffle, whatever the width of the card.
	maxWaffleColumns = 48
	// specLabelWidth holds the longest label of a card.
	specLabelWidth = 13
)

// cardWidth is the width of a card on a screen of screenWidth cells. Cards
// sit two cells in, as the reply text does.
func cardWidth(screenWidth int) int {
	return min(maxCardWidth, screenWidth-3)
}

// cardFits reports whether a screen has room for a card.
func cardFits(theme presentation.Theme, screenWidth int) bool {
	return theme.Soft() && cardWidth(screenWidth) >= minCardWidth
}

// printCard writes rows on a card, indented like the reply text, and the row
// of air that ends it. The block above already ends with its own air, so the
// card adds none before it.
func printCard(output io.Writer, theme presentation.Theme, screenWidth int, rows []string) {
	for _, row := range theme.Card(rows, cardWidth(screenWidth)) {
		fmt.Fprintln(output, "  "+row)
	}
	fmt.Fprintln(output)
}

// innerWidth is the width of the text of a card of screenWidth cells.
func innerWidth(screenWidth int) int {
	return cardWidth(screenWidth) - 2*presentation.CardInset
}

// titleRow puts a title at the left of a card and a note at its right edge.
func titleRow(theme presentation.Theme, title, note string, inner int) string {
	title = presentation.Ellipsize(presentation.Safe(title), inner)
	note = presentation.Ellipsize(presentation.Safe(note), max(inner-presentation.CellWidth(title)-2, 0))
	gap := inner - presentation.CellWidth(title) - presentation.CellWidth(note)
	if note == "" || gap < 2 {
		return theme.Bold(theme.Ink(title))
	}
	return theme.Bold(theme.Ink(title)) + strings.Repeat(" ", gap) + theme.Graphite(note)
}

// specRow is a label in gray and its value in ink, as in the spec lists of
// the page. A value that is too long ends in an ellipsis.
func specRow(theme presentation.Theme, label, value string, inner int) string {
	pad := strings.Repeat(" ", max(specLabelWidth-presentation.CellWidth(label), 0))
	return theme.Graphite(label+pad) + " " + presentation.Ellipsize(presentation.Safe(value), max(inner-specLabelWidth-1, 1))
}

// statusFacts is what /status reports about the session.
type statusFacts struct {
	model, effort, directory, sessionID string
	autoApprove                         bool
	messages, estimate, compactTokens   int
}

// statusCard is the view of /status: the model, the context against the
// point where compaction starts, and the facts of the session.
func statusCard(theme presentation.Theme, screenWidth int, facts statusFacts) []string {
	inner := innerWidth(screenWidth)
	columns := min(inner, maxWaffleColumns)
	capacity := facts.compactTokens
	if capacity <= 0 {
		capacity = max(facts.estimate, 1)
	}
	used := presentation.WaffleCells([]int{facts.estimate}, capacity, columns*waffleRows)
	rows := []string{titleRow(theme, presentation.ModelName(facts.model), "", inner), ""}
	rows = append(rows, theme.Waffle([]presentation.WaffleSegment{{Series: presentation.SeriesAccent, Cells: used[0]}}, columns, waffleRows)...)
	contextText := fmt.Sprintf("about %s tokens in %d messages", shortCount(facts.estimate), facts.messages)
	if facts.compactTokens > 0 {
		contextText = fmt.Sprintf("about %s of %s tokens in %d messages", shortCount(facts.estimate), shortCount(facts.compactTokens), facts.messages)
	}
	return append(rows, "",
		specRow(theme, "Context", contextText, inner),
		specRow(theme, "Effort", facts.effort, inner),
		specRow(theme, "Model", facts.model, inner),
		specRow(theme, "Directory", facts.directory, inner),
		specRow(theme, "Session", facts.sessionID, inner),
		specRow(theme, "Auto-approve", yesNo(facts.autoApprove), inner),
	)
}

// writeContextCard shows what fills the next request, as writeContext does,
// with a waffle that gives each kind of content its own color.
func writeContextCard(output io.Writer, theme presentation.Theme, screenWidth int, config agent.Config, toolSet *tools.Set, messages []anthropic.EncodedMessage, compactTokens int) error {
	parts, err := contextParts(config, toolSet, messages)
	if err != nil {
		return err
	}
	inner := innerWidth(screenWidth)
	total := 0
	tokens := make([]int, len(parts))
	for index, part := range parts {
		tokens[index] = part.bytes / bytesPerToken
		total += part.bytes
	}
	estimate := total / bytesPerToken
	note := "about " + shortCount(estimate) + " tokens"
	if compactTokens > 0 {
		note = fmt.Sprintf("about %s of %s tokens · %d%%", shortCount(estimate), shortCount(compactTokens), estimate*100/compactTokens)
	}
	columns := min(inner, maxWaffleColumns)
	segments := make([]presentation.WaffleSegment, 0, len(parts))
	for index, cells := range presentation.WaffleCells(tokens, compactTokens, columns*waffleRows) {
		segments = append(segments, presentation.WaffleSegment{Series: index, Cells: cells})
	}
	rows := []string{titleRow(theme, "Context", note, inner), ""}
	rows = append(rows, theme.Waffle(segments, columns, waffleRows)...)
	rows = append(rows, "")
	for index, part := range parts {
		if part.bytes == 0 {
			continue
		}
		label := part.name
		if part.count > 0 {
			label = fmt.Sprintf("%s (%d)", part.name, part.count)
		}
		share := 0
		if total > 0 {
			share = part.bytes * 100 / total
		}
		name := presentation.Ellipsize(label, max(inner-14, 1))
		figures := fmt.Sprintf("%6s %3d%%", "~"+shortCount(tokens[index]), share)
		gap := max(inner-2-presentation.CellWidth(name)-len(figures), 1)
		rows = append(rows, theme.SeriesMark(index)+" "+name+strings.Repeat(" ", gap)+theme.Graphite(figures))
	}
	printCard(output, theme, screenWidth, rows)
	return nil
}
