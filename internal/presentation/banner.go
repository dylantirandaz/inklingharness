package presentation

import (
	"hash/fnv"
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
	bannerArtMaxWidth  = 48
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

// bannerSeed picks one of the page's seeds from the workspace path.
func bannerSeed(directory string) uint32 {
	hash := fnv.New32a()
	hash.Write([]byte(directory))
	return BlotSeeds[int(hash.Sum32()%uint32(len(BlotSeeds)))]
}

func (info BannerInfo) picture(theme Theme) *BlotPicture {
	if theme.depth < ANSI256 || info.Width < bannerMinimumWidth {
		return nil
	}
	rows := min(bannerArtMaxRows, info.Height-bannerArtReserved)
	if rows < bannerArtMinRows {
		return nil
	}
	return NewBlotPicture(NewBlot(bannerSeed(info.Directory)), min(bannerArtMaxWidth, info.Width-8), rows)
}

func bannerText(info BannerInfo, theme Theme) []string {
	return []string{
		theme.Bold(theme.Ink(singleLine(filepath.Base(info.Directory)))),
		theme.Graphite("/help commands") + theme.Hairline(" · ") + theme.Graphite("/status session"),
	}
}

// heroAt draws the opening screen at a progress from 0 to 1, centered like
// the hero of the page: the artwork, and under it a label, as a museum hangs
// one: the workspace name, then the model. Every progress has the same
// number of rows, so the composer below stays in place.
func heroAt(picture *BlotPicture, info BannerInfo, theme Theme, progress float64) string {
	width := max(info.Width-1, 1)
	center := func(plain, styled string) string {
		return strings.Repeat(" ", max((width-CellWidth(plain))/2, 0)) + styled
	}
	var rows []string
	rows = append(rows, "")
	artPad := strings.Repeat(" ", max((width-picture.Columns)/2, 0))
	for _, row := range picture.Draw(theme, min(progress/0.85, 1)) {
		rows = append(rows, artPad+row)
	}
	rows = append(rows, "")
	title := clipCells(singleLine(filepath.Base(info.Directory)), width-4)
	subtitle := clipCells(singleLine(info.Model+"  ·  "+info.Effort+" effort"), width-4)
	switch {
	case progress >= 0.85:
		rows = append(rows, center(title, theme.Bold(theme.Ink(title))), center(subtitle, theme.Graphite(subtitle)))
	case progress >= 0.7:
		rows = append(rows, center(title, theme.Bold(theme.Ink(title))), "")
	default:
		rows = append(rows, "", "")
	}
	rows = append(rows, "")
	return strings.Join(rows, "\n")
}
