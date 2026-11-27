package coverpool

import (
	"context"
	"reflect"
	"testing"
)

func TestStatusForProgress(t *testing.T) {
	ctx := context.Background()
	pool := NewPool(t.TempDir())
	cfg := NewConfig(nil, nil)
	if err := cfg.Set(ctx, Settings{Count: 5, Sizes: [][2]int{{600, 800}, {300, 400}}}); err != nil {
		t.Fatal(err)
	}

	// Non-empty backgrounds must still count (progress aggregates across backgrounds).
	bgA := Spec{Text: "alpha", Width: 600, Height: 800, Background: "a.webp", Format: FormatWebP}
	bgB := Spec{Text: "alpha", Width: 600, Height: 800, Background: "b.webp", Format: FormatWebP}
	if err := pool.Put(bgA, 0, testWebP); err != nil {
		t.Fatal(err)
	}
	if err := pool.Put(bgB, 0, testWebP); err != nil {
		t.Fatal(err)
	}

	got := StatusFor(pool, cfg, []string{"alpha", "beta"})
	want := []KeywordStatus{
		{
			Text: "alpha",
			Sizes: []SizeProgress{
				{Width: 600, Height: 800, Count: 2, Target: 5},
				{Width: 300, Height: 400, Count: 0, Target: 5},
			},
		},
		{
			Text: "beta",
			Sizes: []SizeProgress{
				{Width: 600, Height: 800, Count: 0, Target: 5},
				{Width: 300, Height: 400, Count: 0, Target: 5},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("StatusFor() = %#v, want %#v", got, want)
	}
}

func TestStatusForMissingPoolConfig(t *testing.T) {
	if got := StatusFor(nil, nil, []string{"x"}); !reflect.DeepEqual(got, []KeywordStatus{{Text: "x"}}) {
		t.Fatalf("nil pool+cfg = %#v", got)
	}

	cfg := NewConfig(nil, nil)
	if err := cfg.Set(context.Background(), Settings{Count: 5, Sizes: [][2]int{{600, 800}}}); err != nil {
		t.Fatal(err)
	}
	got := StatusFor(nil, cfg, []string{"x"})
	want := []KeywordStatus{{
		Text: "x",
		Sizes: []SizeProgress{
			{Width: 600, Height: 800, Count: 0, Target: 5},
		},
	}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nil pool = %#v, want %#v", got, want)
	}

	if got := StatusFor(NewPool(t.TempDir()), nil, nil); len(got) != 0 {
		t.Fatalf("empty texts = %#v", got)
	}
}
