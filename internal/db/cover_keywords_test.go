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

	empty := &CoverKeywordStore{}
	if err := empty.UpsertMany(ctx, []string{"a"}); err != nil {
		t.Fatalf("nil Coll UpsertMany: %v", err)
	}
	if got, err := empty.List(ctx); err != nil || got != nil {
		t.Fatalf("nil Coll List = %v, %v", got, err)
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
