package db

import (
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

func TestUseCounterAddSwap(t *testing.T) {
	u := newUseCounter()
	u.add("a", 1)
	u.add("a", 2)
	u.add("b", 1)
	got := u.swap()
	if got["a"] != 3 || got["b"] != 1 || len(got) != 2 {
		t.Fatalf("swap = %v", got)
	}
	if again := u.swap(); len(again) != 0 {
		t.Fatalf("swap after reset = %v", again)
	}
	u.add("c", 5)
	if got := u.swap(); got["c"] != 5 || len(got) != 1 {
		t.Fatalf("swap after re-add = %v", got)
	}
}

func TestBuildIncModels(t *testing.T) {
	for _, field := range []string{"md5", "_id"} {
		counts := map[string]int64{"x": 3, "y": 7}
		models := buildIncModels(counts, field)
		if len(models) != 2 {
			t.Fatalf("%s: len=%d", field, len(models))
		}
		seen := map[string]int64{}
		for _, m := range models {
			um, ok := m.(*mongo.UpdateOneModel)
			if !ok {
				t.Fatalf("%s: model type %T", field, m)
			}
			f, ok := um.Filter.(bson.M)
			if !ok || len(f) != 1 {
				t.Fatalf("%s: filter %v", field, um.Filter)
			}
			id, _ := f[field].(string)
			upd, ok := um.Update.(bson.M)
			if !ok {
				t.Fatalf("%s: update %v", field, um.Update)
			}
			inc, ok := upd["$inc"].(bson.M)
			if !ok {
				t.Fatalf("%s: $inc %v", field, upd)
			}
			n, _ := inc["use_count"].(int64)
			seen[id] = n
		}
		if seen["x"] != 3 || seen["y"] != 7 {
			t.Fatalf("%s: seen=%v", field, seen)
		}
	}
}

func TestBuildIncModelsEmpty(t *testing.T) {
	if got := buildIncModels(map[string]int64{}, "md5"); len(got) != 0 {
		t.Fatalf("len=%d", len(got))
	}
}
