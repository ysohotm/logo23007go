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

// CoverPoolStore 保存封面池的全局配置（数量 + 尺寸白名单）。
type CoverPoolStore struct {
	Coll *mongo.Collection
}

type coverPoolDoc struct {
	Count     int       `bson:"count"`
	Workers   int       `bson:"workers,omitempty"`
	Sizes     []sizeDoc `bson:"sizes,omitempty"`
	UpdatedAt time.Time `bson:"updated_at,omitempty"`
}

type sizeDoc struct {
	Width  int `bson:"w"`
	Height int `bson:"h"`
}

// Get 返回封面池配置；尚无文档时使用默认值。旧文档只有 count 时补默认尺寸。
func (s *CoverPoolStore) Get(ctx context.Context) (coverpool.Settings, error) {
	var doc coverPoolDoc
	err := s.Coll.FindOne(ctx, bson.M{"_id": "global"}).Decode(&doc)
	if err == mongo.ErrNoDocuments {
		return coverpool.DefaultSettings(), nil
	}
	if err != nil {
		return coverpool.Settings{}, err
	}
	settings := coverpool.Settings{Count: doc.Count, Workers: doc.Workers, Sizes: sizesFromDoc(doc.Sizes)}
	if settings.Count == 0 {
		settings.Count = 10
	}
	// workers missing/0 → NormalizeSettings defaults to 2
	return coverpool.NormalizeSettings(settings)
}

// Set 校验并 upsert 封面池配置。
func (s *CoverPoolStore) Set(ctx context.Context, settings coverpool.Settings) error {
	normalized, err := coverpool.NormalizeSettings(settings)
	if err != nil {
		return err
	}
	_, err = s.Coll.UpdateOne(ctx,
		bson.M{"_id": "global"},
		bson.M{"$set": bson.M{
			"count":      normalized.Count,
			"workers":    normalized.Workers,
			"sizes":      sizesToDoc(normalized.Sizes),
			"updated_at": time.Now().UTC(),
		}},
		options.Update().SetUpsert(true),
	)
	if err != nil {
		return fmt.Errorf("coverpool store: %w", err)
	}
	return nil
}

func sizesFromDoc(in []sizeDoc) [][2]int {
	if len(in) == 0 {
		return nil
	}
	out := make([][2]int, 0, len(in))
	for _, s := range in {
		out = append(out, [2]int{s.Width, s.Height})
	}
	return out
}

func sizesToDoc(in [][2]int) []sizeDoc {
	out := make([]sizeDoc, 0, len(in))
	for _, s := range in {
		out = append(out, sizeDoc{Width: s[0], Height: s[1]})
	}
	return out
}
