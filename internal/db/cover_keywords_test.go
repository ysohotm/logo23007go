package db

import (
	"context"
	"reflect"
	"testing"
)

func TestCoverKeywordStoreNilSafe(t *testing.T) {
	ctx := context.Background()
	var nilStore *CoverKeywordStore
	if err := nilStore.UpsertMany(ctx, []string{"a"}); err != nil {
		t.Fatalf("nil store UpsertMany: %v", err)
	}
	if got, err := nilStore.List(ctx); err != nil || got != nil {
		t.Fatalf("nil store List = %v, %v", got, err)
	}
	page, err := nilStore.ListPage(ctx, 0, 0, "q")
	if err != nil {
		t.Fatalf("nil store ListPage: %v", err)
	}
	if page.Texts == nil || len(page.Texts) != 0 || page.Page != 1 || page.PageSize != 50 || page.Total != 0 {
		t.Fatalf("nil store ListPage = %+v", page)
	}
	if err := nilStore.EnsureIndexes(ctx); err != nil {
		t.Fatalf("nil store EnsureIndexes: %v", err)
	}

	empty := &CoverKeywordStore{}
	if err := empty.UpsertMany(ctx, []string{"a"}); err != nil {
		t.Fatalf("nil Coll UpsertMany: %v", err)
	}
	if got, err := empty.List(ctx); err != nil || got != nil {
		t.Fatalf("nil Coll List = %v, %v", got, err)
	}
	page, err = empty.ListPage(ctx, 2, 100, "")
	if err != nil {
		t.Fatalf("nil Coll ListPage: %v", err)
	}
	if page.Texts == nil || len(page.Texts) != 0 || page.Page != 2 || page.PageSize != 100 {
		t.Fatalf("nil Coll ListPage = %+v", page)
	}
}

func TestClampKeywordListPage(t *testing.T) {
	tests := []struct {
		page, size     int
		wantPage, want int
	}{
		{0, 0, 1, 50},
		{-1, -5, 1, 50},
		{1, 50, 1, 50},
		{3, 100, 3, 100},
		{2, 200, 2, 200},
		{1, 201, 1, 200},
		{1, 999, 1, 200},
	}
	for _, tt := range tests {
		gotPage, gotSize := clampKeywordListPage(tt.page, tt.size)
		if gotPage != tt.wantPage || gotSize != tt.want {
			t.Fatalf("clampKeywordListPage(%d,%d)=(%d,%d), want (%d,%d)",
				tt.page, tt.size, gotPage, gotSize, tt.wantPage, tt.want)
		}
	}
}

func TestNormalizeCoverKeywords(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{
			name: "trim empty dedupe",
			in:   []string{"  alpha ", "", "beta", "alpha", "  beta  "},
			want: []string{"alpha", "beta"},
		},
		{
			name: "all empty",
			in:   []string{"", "  ", "\t"},
			want: []string{},
		},
		{
			name: "nil input",
			in:   nil,
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := normalizeCoverKeywords(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("normalizeCoverKeywords() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestCoverKeywordStoreUpsertManyEmptyNoOp(t *testing.T) {
	// Even with a non-nil Coll pointer shape, empty/normalized-empty must not
	// touch Mongo; nil Coll already covered. Empty texts is a pure no-op.
	store := &CoverKeywordStore{}
	if err := store.UpsertMany(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertMany(context.Background(), []string{"", "  "}); err != nil {
		t.Fatal(err)
	}
}
