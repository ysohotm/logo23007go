package db

import (
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// CoverPoolStore 保存封面池的全局数量配置。
type CoverPoolStore struct {
	Coll *mongo.Collection
}

// Get 返回 1 到 100 的封面池数量；尚无配置时使用默认值 10。
func (s *CoverPoolStore) Get(ctx context.Context) (int, error) {
	var config struct {
		Count int `bson:"count"`
	}
	err := s.Coll.FindOne(ctx, bson.M{"_id": "global"}).Decode(&config)
	if err == mongo.ErrNoDocuments {
		return 10, nil
	}
	if err != nil {
		return 0, err
	}
	if config.Count < 1 || config.Count > 100 {
		return 0, fmt.Errorf("coverpool: invalid count %d", config.Count)
	}
	return config.Count, nil
}

// Set 更新封面池数量，并记录本次配置的 UTC 修改时间。
func (s *CoverPoolStore) Set(ctx context.Context, count int) error {
	if count < 1 || count > 100 {
		return fmt.Errorf("coverpool: invalid count %d", count)
	}
	_, err := s.Coll.UpdateOne(ctx,
		bson.M{"_id": "global"},
		bson.M{"$set": bson.M{
			"count":      count,
			"updated_at": time.Now().UTC(),
		}},
		options.Update().SetUpsert(true),
	)
	return err
}
