package ipguard

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const configDocID = "default"

// MongoStore 把规则存为集合中的单文档 _id=default。
type MongoStore struct {
	Coll *mongo.Collection
}

func (m MongoStore) Load(ctx context.Context) (Config, bool, error) {
	var c Config
	err := m.Coll.FindOne(ctx, bson.M{"_id": configDocID}).Decode(&c)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	return c, true, nil
}

func (m MongoStore) Save(ctx context.Context, c Config) error {
	_, err := m.Coll.ReplaceOne(ctx, bson.M{"_id": configDocID}, c, options.Replace().SetUpsert(true))
	return err
}
