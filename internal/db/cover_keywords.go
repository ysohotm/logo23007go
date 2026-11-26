package db

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CoverKeywordStore registers manually added cover keywords (one doc per text).
type CoverKeywordStore struct {
	Coll *mongo.Collection
}

type coverKeywordDoc struct {
	ID        string    `bson:"_id"`
	Text      string    `bson:"text"`
	CreatedAt time.Time `bson:"created_at"`
	UpdatedAt time.Time `bson:"updated_at"`
}

// UpsertMany inserts/updates normalized non-empty unique texts. Empty input is no-op.
// On insert set created_at; always set updated_at=now UTC. _id and text = the keyword string.
func (s *CoverKeywordStore) UpsertMany(ctx context.Context, texts []string) error {
	if s == nil || s.Coll == nil {
		return nil
	}
	normalized := normalizeCoverKeywords(texts)
	if len(normalized) == 0 {
		return nil
	}
	now := time.Now().UTC()
	models := make([]mongo.WriteModel, 0, len(normalized))
	for _, text := range normalized {
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{"_id": text}).
			SetUpdate(bson.M{
				"$set": bson.M{
					"text":       text,
					"updated_at": now,
				},
				"$setOnInsert": bson.M{
					"created_at": now,
				},
			}).
			SetUpsert(true))
	}
	_, err := s.Coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	if err != nil {
		return fmt.Errorf("cover keywords upsert: %w", err)
	}
	return nil
}

// List returns texts ordered by updated_at descending (newest first). Empty store → nil slice.
func (s *CoverKeywordStore) List(ctx context.Context) ([]string, error) {
	if s == nil || s.Coll == nil {
		return nil, nil
	}
	opts := options.Find().SetSort(bson.D{{Key: "updated_at", Value: -1}})
	cur, err := s.Coll.Find(ctx, bson.M{}, opts)
	if err != nil {
		return nil, fmt.Errorf("cover keywords list: %w", err)
	}
	defer cur.Close(ctx)

	var out []string
	for cur.Next(ctx) {
		var doc coverKeywordDoc
		if err := cur.Decode(&doc); err != nil {
			return nil, fmt.Errorf("cover keywords list: %w", err)
		}
		text := doc.Text
		if text == "" {
			text = doc.ID
		}
		out = append(out, text)
	}
	if err := cur.Err(); err != nil {
		return nil, fmt.Errorf("cover keywords list: %w", err)
	}
	return out, nil
}

func normalizeCoverKeywords(texts []string) []string {
	seen := make(map[string]struct{}, len(texts))
	out := make([]string, 0, len(texts))
	for _, s := range texts {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
