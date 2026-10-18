package presentation

import (
	"reflect"
	"testing"
)

// These grids come from the page's own script, run on the same seeds. Each
// character is the layer that the page paints at the middle of one cell of
// its 24 by 16 grid: 0 is the ink blot, 1 to 3 are the shapes, and a dot is
// the bare page.
var blotGolden = map[uint32][blotRows]string{
	3398537396: {
		"........................",
		"........................",
		"........................",
		".....0...........11.....",
		".000000.........1111....",
		"00000000.........11.....",
		"00000000................",
		"000000000...............",
		"00000002200.............",
		"00000022220000000.33..0.",
		"000002222220000003333000",
		"...022222220000003330000",
		"....02222200000000300000",
		"....00000000000000000000",
		"....00000000000000000000",
		".....00000..00000000000.",
	},
	2713983725: {
		".000000....000..........",
		"00000000..00000.........",
		"....000000000000.11.....",
		".....000000000001111....",
		".....000000000001111....",
		".....000000000001111....",
		".....00000000000011.....",
		"......00000000000.......",
		".......000000000........",
		"........000..00.........",
		".......2200........33...",
		"......222200......3333..",
		".....2222000......3333..",
		"......2220000......33...",
		".........000............",
		"..........0.............",
	},
}

// A picture must be the one that the page draws for its seed, or the
// artwork is only an imitation of it.
func TestBlotMatchesThePage(t *testing.T) {
	for seed, want := range blotGolden {
		blot := NewBlot(seed)
		for row, line := range want {
			for column, char := range line {
				layer := -1
				if char != '.' {
					layer = int(char - '0')
				}
				if got := blot.layerAt(float64(column)+0.5, float64(row)+0.5); got != layer {
					t.Fatalf("seed %d, row %d, column %d: layer %d, the page paints %d", seed, row, column, got, layer)
				}
			}
		}
	}
}

// The page treats seed 0 as seed 1, so a caller never gets an empty draw.
func TestBlotSeedZeroIsSeedOne(t *testing.T) {
	if !reflect.DeepEqual(NewBlot(0).fields, NewBlot(1).fields) {
		t.Fatal("seed 0 differs from seed 1")
	}
}

// The reveal only adds ink: a quadrant that has appeared stays, so a frame
// never shows less than the one before it.
func TestBlotRevealOnlyAddsInk(t *testing.T) {
	picture := NewBlotPicture(NewBlot(BlotSeeds[0]), 40, 12)
	previous := 0
	for step := 0; step <= 20; step++ {
		progress := float64(step) / 20
		shown := 0
		for index, layer := range picture.layer {
			if layer != 0 && progress > float64(picture.reveal[index]) {
				shown++
			}
		}
		if shown < previous {
			t.Fatalf("progress %.2f shows %d quadrants after %d", progress, shown, previous)
		}
		previous = shown
	}
	complete := 0
	for _, layer := range picture.layer {
		if layer != 0 {
			complete++
		}
	}
	if previous != complete {
		t.Fatalf("the reveal ends with %d of %d quadrants", previous, complete)
	}
}
