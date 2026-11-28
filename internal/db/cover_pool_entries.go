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
	ID            string    `bson:"_id"`
	Text          string    `bson:"text"`
	Width         int       `bson:"width"`
	Height        int       `bson:"height"`
	Background    string    `bson:"background"`
	Format        string    `bson:"format"`
	Count         int       `bson:"count"`
	Dir           string    `bson:"dir"`
	UpdatedAt     time.Time `bson:"updated_at"`
	LastRotatedAt time.Time `bson:"last_rotated_at,omitempty"`
}

var _ coverpool.EntryRegistry = (*CoverPoolEntryStore)(nil)

// EnsureIndexes creates indexes for size lookup, background filter, list, and rotate.
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
		{Keys: bson.D{
			{Key: "background", Value: 1},
			{Key: "count", Value: 1},
			{Key: "last_rotated_at", Value: 1},
		}},
		{Keys: bson.D{
			{Key: "background", Value: 1},
			{Key: "count", Value: 1},
			{Key: "updated_at", Value: 1},
		}},
	})
	if err != nil {
		return fmt.Errorf("cover pool entries indexes: %w", err)
	}
	return nil
}

func entrySetDoc(e coverpool.PoolEntry, now time.Time) bson.M {
	set := bson.M{
		"text":       e.Text,
		"width":      e.Width,
		"height":     e.Height,
		"background": e.Background,
		"format":     string(e.Format),
		"count":      e.Count,
		"dir":        e.Dir,
		"updated_at": now,
	}
	// Zero LastRotatedAt means "leave existing Mongo value" (Put/reindex must not wipe it).
	if !e.LastRotatedAt.IsZero() {
		set["last_rotated_at"] = e.LastRotatedAt.UTC()
	}
	return set
}

func entryFromDoc(doc coverPoolEntryDoc) coverpool.PoolEntry {
	return coverpool.PoolEntry{
		ID:            doc.ID,
		Text:          doc.Text,
		Width:         doc.Width,
		Height:        doc.Height,
		Background:    doc.Background,
		Format:        coverpool.ImageFormat(doc.Format),
		Count:         doc.Count,
		Dir:           doc.Dir,
		LastRotatedAt: doc.LastRotatedAt,
	}
}

// Upsert inserts or replaces one pool inventory record by ID.
func (s *CoverPoolEntryStore) Upsert(ctx context.Context, e coverpool.PoolEntry) error {
	if s == nil || s.Coll == nil {
		return nil
	}
	now := time.Now().UTC()
	_, err := s.Coll.UpdateOne(ctx,
		bson.M{"_id": e.ID},
		bson.M{"$set": entrySetDoc(e, now)},
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
			SetUpdate(bson.M{"$set": entrySetDoc(e, now)}).
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
		out = append(out, entryFromDoc(doc))
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("cover pool entries list: %w", err)
	}
	return out, nil
}

// ListDueRotate lists empty-background pools due for rotation.
// Never-rotated (missing/zero last_rotated_at) are always eligible; otherwise last_rotated_at<=olderThan.
// limit <1 defaults to 50; max 50000. Sorted oldest-first.
func (s *CoverPoolEntryStore) ListDueRotate(ctx context.Context, minCount int, olderThan time.Time, limit int) ([]coverpool.PoolEntry, error) {
	if s == nil || s.Coll == nil {
		return nil, nil
	}
	if minCount < 1 {
		minCount = 1
	}
	if limit < 1 {
		limit = 50
	}
	if limit > 50000 {
		limit = 50000
	}
	zero := time.Time{}.UTC()
	older := olderThan.UTC()
	filter := bson.M{
		"background": "",
		"count":      bson.M{"$gte": minCount},
		"$or": []bson.M{
			{"last_rotated_at": bson.M{"$gt": zero, "$lte": older}},
			{"last_rotated_at": bson.M{"$exists": false}},
			{"last_rotated_at": nil},
			{"last_rotated_at": zero},
		},
	}
	opts := options.Find().
		SetSort(bson.D{
			{Key: "last_rotated_at", Value: 1},
			{Key: "updated_at", Value: 1},
		}).
		SetLimit(int64(limit))
	cur, err := s.Coll.Find(ctx, filter, opts)
	if err != nil {
		return nil, fmt.Errorf("cover pool entries due rotate: %w", err)
	}
	defer cur.Close(ctx)

	var out []coverpool.PoolEntry
	for cur.Next(ctx) {
		var doc coverPoolEntryDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("cover pool entries due rotate: %w", err)
		}
		out = append(out, entryFromDoc(doc))
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("cover pool entries due rotate: %w", err)
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
