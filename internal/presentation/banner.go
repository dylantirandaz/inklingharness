package presentation

import (
	"math"
	"path/filepath"
	"strings"
)

// BannerInfo is what the opening screen shows.
type BannerInfo struct {
	// Directory is the workspace. Its name is the title, and it picks the
	// picture, so a project keeps its artwork.
	Directory string
	// Model and Effort are the display names that the subtitle shows.
	Model, Effort string
	// Width and Height are the terminal size in cells; 0 for no terminal.
	Width, Height int
}

// The hero needs this much of the terminal: the page's 12 rows of artwork
// need 6 rows of text and margins, and the composer needs the rest.
const (
	bannerMinimumWidth = 64
	bannerArtMaxRows   = 12
	bannerArtMaxWidth  = 60
	bannerArtReserved  = 18
	bannerArtMinRows   = 5
)

// Banner is the opening screen without animation.
func Banner(info BannerInfo, theme Theme) string {
	if picture := info.picture(theme); picture != nil {
		return heroAt(picture, info, theme, 1)
	}
	return "\n  " + strings.Join(bannerText(info, theme), "\n\n  ") + "\n"
}

// BannerAnimation draws the opening screen at a progress from 0 to 1: the
// ink soaks in, then the workspace and the model appear, and the composer
// below never moves. It is nil when the terminal has no room or color support
// for the artwork. It draws each frame when called, from one picture, so a
// screen computes only the frames that it shows. It is not safe for
// concurrent calls.
func BannerAnimation(info BannerInfo, theme Theme) func(progress float64) string {
	picture := info.picture(theme)
	if picture == nil {
		return nil
	}
	return func(progress float64) string {
		return heroAt(picture, info, theme, min(max(progress, 0), 1))
	}
}

func (info BannerInfo) picture(theme Theme) *CityPicture {
	if theme.depth < ANSI256 || info.Width < bannerMinimumWidth {
		return nil
	}
	rows := min(bannerArtMaxRows, info.Height-bannerArtReserved)
	if rows < bannerArtMinRows {
		return nil
	}
	return NewCityPicture(info.Directory, min(bannerArtMaxWidth, info.Width-8), rows)
}

func bannerText(info BannerInfo, theme Theme) []string {
	return []string{
		theme.Bold(theme.Ink(singleLine(filepath.Base(info.Directory)))),
		theme.Graphite("/help commands") + theme.Hairline(" · ") + theme.Graphite("/status session"),
	}
}

// heroAt draws the opening screen at a progress from 0 to 1: the city, and
// under it one line of force as wide as the city, set as Marinetti set words
// in freedom: the workspace name in bold, a heavy rule that runs out toward
// the model, and the model and effort. Every progress has the same number of
// rows, so the composer below stays in place.
func heroAt(picture *CityPicture, info BannerInfo, theme Theme, progress float64) string {
	width := max(info.Width-1, 1)
	pad := strings.Repeat(" ", max((width-picture.Columns)/2, 0))
	var rows []string
	rows = append(rows, "")
	for _, row := range picture.Draw(theme, min(progress/0.85, 1)) {
		rows = append(rows, pad+row)
	}
	rows = append(rows, "", pad+theme.forceLine(info, picture.Columns, progress), "")
	return strings.Join(rows, "\n")
}

// forceLine is the title under the city. The name shows first, the rule
// runs out from it, and the model lands at its end.
func (t Theme) forceLine(info BannerInfo, columns int, progress float64) string {
	if progress < 0.7 {
		return ""
	}
	name := clipCells(singleLine(filepath.Base(info.Directory)), max(columns/2, 4))
	model := singleLine(info.Model + " · " + info.Effort)
	room := columns - CellWidth(name) - CellWidth(model) - 3
	if room < 4 {
		model, room = "", columns-CellWidth(name)-2
	}
	run := int(math.Round(float64(max(room, 1)) * min((progress-0.7)/0.15, 1)))
	line := t.Bold(t.Ink(name)) + " " + t.forceRule(run, room)
	if progress >= 0.85 && model != "" {
		line += " " + t.Graphite(model)
	}
	return line
}
