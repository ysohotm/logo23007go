package db

import (
	"context"
	"fmt"
	"regexp"
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

// KeywordListResult is a page of cover keyword texts.
type KeywordListResult struct {
	Texts    []string
	Total    int64
	Page     int
	PageSize int
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

// ListPage returns a page of keyword texts. page>=1, pageSize 1..200 (default 50).
// q trimmed; non-empty filters text with case-sensitive substring via quoted bson regex.
// Sorted by updated_at desc. Texts is never nil.
func (s *CoverKeywordStore) ListPage(ctx context.Context, page, pageSize int, q string) (KeywordListResult, error) {
	page, pageSize = clampKeywordListPage(page, pageSize)
	out := KeywordListResult{Texts: []string{}, Page: page, PageSize: pageSize}
	if s == nil || s.Coll == nil {
		return out, nil
	}
	filter := bson.M{}
	q = strings.TrimSpace(q)
	if q != "" {
		filter["text"] = bson.M{"$regex": regexp.QuoteMeta(q)}
	}
	total, err := s.Coll.CountDocuments(ctx, filter)
	if err != nil {
		return out, fmt.Errorf("cover keywords count: %w", err)
	}
	out.Total = total
	skip := int64(page-1) * int64(pageSize)
	opts := options.Find().
		SetSort(bson.D{{Key: "updated_at", Value: -1}}).
		SetSkip(skip).
		SetLimit(int64(pageSize))
	cur, err := s.Coll.Find(ctx, filter, opts)
	if err != nil {
		return out, fmt.Errorf("cover keywords list page: %w", err)
	}
	defer cur.Close(ctx)

	texts := make([]string, 0, pageSize)
	for cur.Next(ctx) {
		var doc coverKeywordDoc
		if err := cur.Decode(&doc); err != nil {
			return out, fmt.Errorf("cover keywords list page: %w", err)
		}
		text := doc.Text
		if text == "" {
			text = doc.ID
		}
		texts = append(texts, text)
	}
	if err := cur.Err(); err != nil {
		return out, fmt.Errorf("cover keywords list page: %w", err)
	}
	out.Texts = texts
	return out, nil
}

// EnsureIndexes creates an index on updated_at for list sorting.
func (s *CoverKeywordStore) EnsureIndexes(ctx context.Context) error {
	if s == nil || s.Coll == nil {
		return nil
	}
	_, err := s.Coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{{Key: "updated_at", Value: -1}},
	})
	if err != nil {
		return fmt.Errorf("cover keywords indexes: %w", err)
	}
	return nil
}

func clampKeywordListPage(page, pageSize int) (int, int) {
	if page < 1 {
		page = 1
	}
	if pageSize < 1 {
		pageSize = 50
	} else if pageSize > 200 {
		pageSize = 200
	}
	return page, pageSize
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
