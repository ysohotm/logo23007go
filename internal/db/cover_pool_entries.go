package db

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"

	"logo23007go/internal/coverpool"
)

// CoverPoolEntryStore persists on-disk cover pool directory inventory.
type CoverPoolEntryStore struct {
	Coll *mongo.Collection
}

type coverPoolEntryDoc struct {
	ID         string    `bson:"_id"`
	Text       string    `bson:"text"`
	Width      int       `bson:"width"`
	Height     int       `bson:"height"`
	Background string    `bson:"background"`
	Format     string    `bson:"format"`
	Count      int       `bson:"count"`
	Dir        string    `bson:"dir"`
	UpdatedAt  time.Time `bson:"updated_at"`
}

var _ coverpool.EntryRegistry = (*CoverPoolEntryStore)(nil)

// EnsureIndexes creates indexes for size lookup, background filter, and list sort.
func (s *CoverPoolEntryStore) EnsureIndexes(ctx context.Context) error {
	if s == nil || s.Coll == nil {
		return nil
	}
	_, err := s.Coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{
			{Key: "text", Value: 1},
			{Key: "width", Value: 1},
			{Key: "height", Value: 1},
		}},
		{Keys: bson.D{{Key: "background", Value: 1}}},
		{Keys: bson.D{{Key: "updated_at", Value: -1}}},
	})
	if err != nil {
		return fmt.Errorf("cover pool entries indexes: %w", err)
	}
	return nil
}

// Upsert inserts or replaces one pool inventory record by ID.
func (s *CoverPoolEntryStore) Upsert(ctx context.Context, e coverpool.PoolEntry) error {
	if s == nil || s.Coll == nil {
		return nil
	}
	now := time.Now().UTC()
	_, err := s.Coll.UpdateOne(ctx,
		bson.M{"_id": e.ID},
		bson.M{"$set": bson.M{
			"text":       e.Text,
			"width":      e.Width,
			"height":     e.Height,
			"background": e.Background,
			"format":     string(e.Format),
			"count":      e.Count,
			"dir":        e.Dir,
			"updated_at": now,
		}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("cover pool entries upsert: %w", err)
	}
	return nil
}

// UpsertMany inserts or replaces many pool inventory records in one BulkWrite.
func (s *CoverPoolEntryStore) UpsertMany(ctx context.Context, entries []coverpool.PoolEntry) error {
	if s == nil || s.Coll == nil || len(entries) == 0 {
		return nil
	}
	now := time.Now().UTC()
	models := make([]mongo.WriteModel, 0, len(entries))
	for _, e := range entries {
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": e.ID}).
			SetUpdate(bson.M{"$set": bson.M{
				"text":       e.Text,
				"width":      e.Width,
				"height":     e.Height,
				"background": e.Background,
				"format":     string(e.Format),
				"count":      e.Count,
				"dir":        e.Dir,
				"updated_at": now,
			}}).
			SetUpsert(true))
	}
	_, err := s.Coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return fmt.Errorf("cover pool entries upsert many: %w", err)
	}
	return nil
}

// Delete removes one pool inventory record by ID.
func (s *CoverPoolEntryStore) Delete(ctx context.Context, id string) error {
	if s == nil || s.Coll == nil {
		return nil
	}
	_, err := s.Coll.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return fmt.Errorf("cover pool entries delete: %w", err)
	}
	return nil
}

// SumCount sums Count for text+width+height. If emptyBackgroundOnly, only Background=="".
func (s *CoverPoolEntryStore) SumCount(ctx context.Context, text string, width, height int, emptyBackgroundOnly bool) (int, error) {
	if s == nil || s.Coll == nil {
		return 0, nil
	}
	match := bson.M{
		"text":   text,
		"width":  width,
		"height": height,
	}
	if emptyBackgroundOnly {
		match["background"] = ""
	}
	pipeline := mongo.Pipeline{
		{{Key: "$match", Value: match}},
		{{Key: "$group", Value: bson.D{
			{Key: "_id", Value: nil},
			{Key: "total", Value: bson.D{{Key: "$sum", Value: "$count"}}},
		}}},
	}
	cur, err := s.Coll.Aggregate(ctx, pipeline)
	if err != nil {
		return 0, fmt.Errorf("cover pool entries sum: %w", err)
	}
	defer cur.Close(ctx)

	if !cur.Next(ctx) {
		if err := cur.Err(); err != nil {
			return 0, fmt.Errorf("cover pool entries sum: %w", err)
		}
		return 0, nil
	}
	var result struct {
		Total int `bson:"total"`
	}
	if err := cur.Decode(&result); err != nil {
		return 0, fmt.Errorf("cover pool entries sum: %w", err)
	}
	return result.Total, nil
}

// ListBackground lists entries by background emptiness.
// nonempty=true → background!=""; nonempty=false → background=="".
// limit defaults to 1000, max 50000. Ordered by updated_at desc.
func (s *CoverPoolEntryStore) ListBackground(ctx context.Context, nonempty bool, limit int) ([]coverpool.PoolEntry, error) {
	if s == nil || s.Coll == nil {
		return nil, nil
	}
	limit = clampCoverPoolEntryLimit(limit)
	filter := bson.M{"background": ""}
	if nonempty {
		filter["background"] = bson.M{"$ne": ""}
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "updated_at", Value: -1}}).
		SetLimit(int64(limit))
	cur, err := s.Coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("cover pool entries list: %w", err)
	}
	defer cur.Close(ctx)

	var out []coverpool.PoolEntry
	for cur.Next(ctx) {
		var doc coverPoolEntryDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("cover pool entries list: %w", err)
		}
		out = append(out, coverpool.PoolEntry{
			ID:         doc.ID,
			Text:       doc.Text,
			Width:      doc.Width,
			Height:     doc.Height,
			Background: doc.Background,
			Format:     coverpool.ImageFormat(doc.Format),
			Count:      doc.Count,
			Dir:        doc.Dir,
		})
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("cover pool entries list: %w", err)
	}
	return out, nil
}

func clampCoverPoolEntryLimit(limit int) int {
	if limit < 1 {
		return 1000
	}
	if limit > 50000 {
		return 50000
	}
	return limit
}
