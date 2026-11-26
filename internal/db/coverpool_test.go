package db

import (
	"testing"

	"logo23007go/internal/coverpool"
)

func TestCoverPoolStoreImplementsInterface(t *testing.T) {
	var _ coverpool.Store = (*CoverPoolStore)(nil)
}

func TestCoverPoolStoreSetRejectsInvalidSettings(t *testing.T) {
	store := &CoverPoolStore{}
	if err := store.Set(nil, coverpool.Settings{Count: 0, Sizes: coverpool.DefaultSizes()}); err == nil {
		t.Fatal("expected invalid count error")
	}
	if err := store.Set(nil, coverpool.Settings{Count: 10, Sizes: [][2]int{{-1, 100}}}); err == nil {
		t.Fatal("expected invalid size error")
	}
}

func TestSizesDocRoundTrip(t *testing.T) {
	in := [][2]int{{600, 800}, {300, 400}}
	got := sizesFromDoc(sizesToDoc(in))
	if len(got) != 2 || got[0] != in[0] || got[1] != in[1] {
		t.Fatalf("roundtrip = %v", got)
	}
}
