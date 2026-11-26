package db

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/event"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const idChars = "abcdefghijklmnopqrstuvwxyz0123456789"

const (
	defaultMaxPool   = 100
	defaultOpTimeout = 2 * time.Second
	lookupCacheTTL   = 30 * time.Minute
	lookupCacheMax   = 200000
)

type SiteDb struct {
	client *mongo.Client
	db     *mongo.Database
	rng    *rand.Rand
	mu     sync.Mutex

	opTimeout time.Duration

	poolOpen           atomic.Int64
	poolCheckedOut     atomic.Int64
	poolCheckoutFailed atomic.Int64

	md5Cache *lookupCache // md5 -> file string
	keyCache *lookupCache // filekey id -> fileKeyEntry

	md5Uses *useCounter
	keyUses *useCounter
}

type fileKeyEntry struct {
	key  string
	file string
}

type PoolStats struct {
	Open           int64
	CheckedOut     int64
	CheckoutFailed int64
}

func NewSiteDb(ctx context.Context) (*SiteDb, error) {
	return newSiteDb(ctx, true, envMaxPool())
}

// NewSiteDbNoPing mirrors C# MongoClient construction (connect lazily on first use).
func NewSiteDbNoPing(ctx context.Context) (*SiteDb, error) {
	return newSiteDb(ctx, false, envMaxPool())
}

// NewSiteDbWithPool 不 ping，显式指定连接池上限（用于小型控制面 client）。
func NewSiteDbWithPool(ctx context.Context, maxPool uint64) (*SiteDb, error) {
	return newSiteDb(ctx, false, maxPool)
}

func envMaxPool() uint64 {
	if v := os.Getenv("MONGO_MAX_POOL"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return defaultMaxPool
}

func envOpTimeout() time.Duration {
	if v := os.Getenv("DB_OP_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultOpTimeout
}

func newSiteDb(ctx context.Context, ping bool, maxPool uint64) (*SiteDb, error) {
	conn := os.Getenv("DBCONN")
	dbName := os.Getenv("DBNAME")
	if conn == "" {
		return nil, fmt.Errorf("DBCONN is empty")
	}
	if dbName == "" {
		dbName = "cachelogomaker"
	}
	s := &SiteDb{
		rng:       rand.New(rand.NewSource(time.Now().UnixNano())),
		opTimeout: envOpTimeout(),
		md5Cache:  newLookupCache(lookupCacheTTL, lookupCacheMax),
		keyCache:  newLookupCache(lookupCacheTTL, lookupCacheMax),
		md5Uses:   newUseCounter(),
		keyUses:   newUseCounter(),
	}
	opts := options.Client().ApplyURI(conn).
		SetMaxPoolSize(maxPool).
		SetMaxConnIdleTime(5 * time.Minute).
		SetPoolMonitor(&event.PoolMonitor{Event: s.onPoolEvent})
	client, err := mongo.Connect(ctx, opts)
	if err != nil {
		return nil, err
	}
	if ping {
		pctx, cancel := context.WithTimeout(ctx, s.opTimeout)
		err := client.Ping(pctx, nil)
		cancel()
		if err != nil {
			_ = client.Disconnect(context.Background())
			return nil, fmt.Errorf("mongo ping: %w", err)
		}
	}
	s.client = client
	s.db = client.Database(dbName)
	return s, nil
}

func (s *SiteDb) onPoolEvent(e *event.PoolEvent) {
	switch e.Type {
	case event.ConnectionCreated:
		s.poolOpen.Add(1)
	case event.ConnectionClosed:
		s.poolOpen.Add(-1)
	case event.GetSucceeded:
		s.poolCheckedOut.Add(1)
	case event.ConnectionReturned:
		s.poolCheckedOut.Add(-1)
	case event.GetFailed:
		s.poolCheckoutFailed.Add(1)
	}
}

func (s *SiteDb) PoolStats() PoolStats {
	return PoolStats{
		Open:           s.poolOpen.Load(),
		CheckedOut:     s.poolCheckedOut.Load(),
		CheckoutFailed: s.poolCheckoutFailed.Load(),
	}
}

func (s *SiteDb) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, s.opTimeout)
}

func (s *SiteDb) Close(ctx context.Context) error {
	return s.client.Disconnect(ctx)
}

func (s *SiteDb) Collection(name string) *mongo.Collection {
	return s.db.Collection(name)
}

func (s *SiteDb) FilesByMd5() *mongo.Collection {
	return s.db.Collection("filebymd5")
}

func (s *SiteDb) FileKeys() *mongo.Collection {
	return s.db.Collection("filekeys")
}

func (s *SiteDb) CreateIndex(ctx context.Context) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
	}
	_, err := s.FilesByMd5().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "md5", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	if err != nil {
		fmt.Println(err)
	}
	_, err = s.FileKeys().Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys: bson.D{
			{Key: "domain", Value: 1},
			{Key: "key", Value: 1},
		},
		Options: options.Index().SetUnique(false),
	})
	if err != nil {
		fmt.Println(err)
	}
}

func (s *SiteDb) GetNewFileKeyId(ctx context.Context) (string, error) {
	for {
		buf := make([]byte, 8)
		s.mu.Lock()
		for i := range buf {
			buf[i] = idChars[s.rng.Intn(len(idChars))]
		}
		s.mu.Unlock()
		randomID := string(buf)
		octx, cancel := s.opCtx(ctx)
		n, err := s.FileKeys().CountDocuments(octx, bson.M{"_id": randomID})
		cancel()
		if err != nil {
			return "", err
		}
		if n == 0 {
			return randomID, nil
		}
	}
}

func (s *SiteDb) AutoAddFileKey(ctx context.Context, domain, key string) (string, error) {
	var obj bson.M
	octx, cancel := s.opCtx(ctx)
	err := s.FileKeys().FindOne(octx, bson.M{"domain": domain, "key": key}).Decode(&obj)
	cancel()
	if err == mongo.ErrNoDocuments {
		randomID, err := s.GetNewFileKeyId(ctx)
		if err != nil {
			return "", err
		}
		octx, cancel := s.opCtx(ctx)
		defer cancel()
		_, err = s.FileKeys().InsertOne(octx, bson.M{
			"_id":       randomID,
			"domain":    domain,
			"key":       key,
			"use_count": 0,
		})
		return randomID, err
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprint(obj["_id"]), nil
}

func (s *SiteDb) AutoAddFileKeyAllowDouble(ctx context.Context, domain, key string) (string, error) {
	randomID, err := s.GetNewFileKeyId(ctx)
	if err != nil {
		return "", err
	}
	octx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err = s.FileKeys().InsertOne(octx, bson.M{
		"_id":       randomID,
		"domain":    domain,
		"key":       key,
		"use_count": 0,
	})
	return randomID, err
}

func (s *SiteDb) GetFileKey(ctx context.Context, id string) (bson.M, error) {
	if v, ok := s.keyCache.get(id); ok {
		e := v.(fileKeyEntry)
		return bson.M{"_id": id, "key": e.key, "file": e.file}, nil
	}
	var obj bson.M
	octx, cancel := s.opCtx(ctx)
	defer cancel()
	err := s.FileKeys().FindOne(octx, bson.M{"_id": id}).Decode(&obj)
	if err == mongo.ErrNoDocuments {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	k, _ := obj["key"].(string)
	f, _ := obj["file"].(string)
	s.keyCache.set(id, fileKeyEntry{key: k, file: f})
	return obj, nil
}

func (s *SiteDb) UpdateFileKeyFile(ctx context.Context, id, file string) error {
	octx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.FileKeys().UpdateOne(octx,
		bson.M{"_id": id},
		bson.M{"$set": bson.M{"file": file, "use_count": 1}},
	)
	if err != nil {
		return err
	}
	if v, ok := s.keyCache.get(id); ok {
		e := v.(fileKeyEntry)
		e.file = file
		s.keyCache.set(id, e)
	}
	return nil
}

// UpdateFileKeyUseCount 只记内存计数，由 StartUseCountFlusher / FlushUseCounts 批量写回。
func (s *SiteDb) UpdateFileKeyUseCount(ctx context.Context, id string) error {
	s.keyUses.add(id, 1)
	return nil
}

func (s *SiteDb) GetFileByMd5(ctx context.Context, md5 string) (string, error) {
	if v, ok := s.md5Cache.get(md5); ok {
		if f, _ := v.(string); f != "" {
			s.md5Uses.add(md5, 1)
			return f, nil
		}
	}
	opts := options.FindOne().SetProjection(bson.M{"file": 1})
	var obj bson.M
	octx, cancel := s.opCtx(ctx)
	defer cancel()
	err := s.FilesByMd5().FindOne(octx, bson.M{"md5": md5}, opts).Decode(&obj)
	if err == mongo.ErrNoDocuments {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	f, _ := obj["file"].(string)
	if f != "" {
		s.md5Cache.set(md5, f)
		s.md5Uses.add(md5, 1)
	}
	return f, nil
}

func (s *SiteDb) AddFileByMd5(ctx context.Context, md5, file string) error {
	opts := options.Update().SetUpsert(true)
	octx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.FilesByMd5().UpdateOne(octx,
		bson.M{"md5": md5},
		bson.M{"$set": bson.M{"file": file, "md5": md5}},
		opts,
	)
	if err != nil {
		return err
	}
	if file != "" {
		s.md5Cache.set(md5, file)
	} else {
		s.md5Cache.delete(md5)
	}
	return nil
}
