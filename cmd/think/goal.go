package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dylantirandaz/inklingharness/internal/extend"
	"github.com/dylantirandaz/inklingharness/internal/presentation"
)

// maxGoalRounds bounds the prompts that a goal sends after its check fails,
// so a goal that the model cannot meet does not spend tokens without end.
const maxGoalRounds = 5

// goal is a command that must pass before the work is done, and the number
// of follow-up prompts that it has sent. The zero value is no goal.
type goal struct {
	command string
	rounds  int
}

func (g goal) active() bool { return g.command != "" }

// goalStep is what happens after a goal check.
type goalStep uint8

const (
	// goalMet: the check passed; the goal ends.
	goalMet goalStep = iota
	// goalContinue: the check failed and rounds remain; send the prompt.
	goalContinue
	// goalGiveUp: the check failed and no rounds remain; the goal ends.
	goalGiveUp
)

// next runs the check once and decides the next step. On goalContinue it
// returns the prompt for the model and counts the round.
func (g *goal) next(ctx context.Context, workDir string, output io.Writer, theme presentation.Theme) (goalStep, string, error) {
	fmt.Fprintln(output, "  "+theme.Graphite("goal check: ")+presentation.Safe(g.command))
	result, err := extend.CheckGoal(ctx, workDir, g.command)
	if err != nil {
		return goalGiveUp, "", err
	}
	if result.Passed {
		fmt.Fprintln(output, "  "+theme.Green("●")+" goal met: "+presentation.Safe(g.command))
		*g = goal{}
		return goalMet, "", nil
	}
	if g.rounds >= maxGoalRounds {
		fmt.Fprintf(output, "  %s goal not met after %d rounds (%s); the goal is cleared. Tell me how to continue.\n", theme.Red("✕"), maxGoalRounds, result.Status)
		*g = goal{}
		return goalGiveUp, "", nil
	}
	g.rounds++
	fmt.Fprintf(output, "  %s goal check failed (%s); round %d of %d\n", theme.Amber("●"), result.Status, g.rounds, maxGoalRounds)
	return goalContinue, goalPrompt(g.command, result), nil
}

// goalPrompt tells the model why its work is not done. The check output is
// data from a command, so it goes in a fenced block.
func goalPrompt(command string, result extend.GoalResult) string {
	tail := result.Tail
	if tail == "" {
		tail = "(no output)"
	}
	fence := "```"
	for strings.Contains(tail, fence) {
		fence += "`"
	}
	return "The goal check failed, so the work is not done.\n" +
		"Check: " + command + "\n" +
		"Result: " + result.Status + "\n" +
		"End of the output:\n" + fence + "\n" + tail + "\n" + fence + "\n" +
		"Keep working until this check passes. Do not change the check, and do not weaken or delete tests to make it pass. " +
		"If the goal cannot be met, say why."
}
