package handlers

import "testing"

func TestNormalizeSize(t *testing.T) {
	cases := []struct {
		w, h, wantW, wantH int
	}{
		{1200, 1520, 600, 800},
		{240, 300, 240, 300},
		{600, 800, 600, 800},
		{0, 0, 0, 0},
		{1200, 1600, 1200, 1600},
	}
	for _, c := range cases {
		gotW, gotH := normalizeSize(c.w, c.h)
		if gotW != c.wantW || gotH != c.wantH {
			t.Errorf("normalizeSize(%d,%d) = %d,%d; want %d,%d", c.w, c.h, gotW, gotH, c.wantW, c.wantH)
		}
	}
}
