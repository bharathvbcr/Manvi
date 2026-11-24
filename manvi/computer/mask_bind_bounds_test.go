package computer

import (
	"image"
	"math"
	"testing"
)

// bindMasks must give every distinct set of coordinates its own digest, however
// extreme: a negative or oversized coordinate must not wrap into another.
func TestBindMasksIsInjectiveAtCoordinateExtremes(t *testing.T) {
	var base [32]byte
	vals := []int{math.MinInt, math.MinInt + 1, -65537, -65536, -1, 0, 1, 65535, 65536, math.MaxInt32, math.MaxInt - 1, math.MaxInt}
	seen := map[[32]byte][4]int{}
	for _, a := range vals {
		for _, b := range vals {
			c := [4]int{a, b, b, a}
			d := bindMasks(base, []image.Rectangle{{Min: image.Point{X: c[0], Y: c[1]}, Max: image.Point{X: c[2], Y: c[3]}}})
			if prev, dup := seen[d]; dup {
				t.Fatalf("rectangles %v and %v share a digest", prev, c)
			}
			seen[d] = c
		}
	}
	if got := bindMasks(base, nil); got != base {
		t.Fatal("no masks must leave the digest unchanged")
	}
}
