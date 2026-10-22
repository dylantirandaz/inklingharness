package presentation

import (
	"math"
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

// The spread only adds ink: the front of each layer never moves back, so a
// frame never shows less than the one before it. At the end the water has
// reached every pixel of every shape, and the ink is dry.
func TestBlotSpreadOnlyAddsInkAndEndsDry(t *testing.T) {
	picture := NewBlotPicture(NewBlot(BlotSeeds[0]), 40, 12)
	for layer := range blotLayers {
		previous := math.Inf(-1)
		for step := 0; step <= 40; step++ {
			front, _ := blotSpread(layer, float64(step)/40)
			if front < previous {
				t.Fatalf("layer %d: the front moves back from %.3f to %.3f at step %d", layer, previous, front, step)
			}
			previous = front
		}
		for index, share := range picture.coverage[layer] {
			if share > 0 && float64(picture.arrival[layer][index]) > previous {
				t.Fatalf("layer %d: pixel %d of the shape is never reached", layer, index)
			}
		}
		if _, wet := blotSpread(layer, 1); wet != 0 {
			t.Fatalf("layer %d is still wet at the end: %.3f", layer, wet)
		}
	}
}
