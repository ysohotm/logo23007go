package db

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	useCountChunk        = 1000
	useCountChunkTimeout = 10 * time.Second
)

// useCounter 在内存中累积 use_count 增量，定期批量写回 Mongo。
type useCounter struct {
	mu sync.Mutex
	m  map[string]int64
}

func newUseCounter() *useCounter {
	return &useCounter{m: make(map[string]int64)}
}

func (u *useCounter) add(id string, n int64) {
	u.mu.Lock()
	u.m[id] += n
	u.mu.Unlock()
}

func (u *useCounter) swap() map[string]int64 {
	u.mu.Lock()
	defer u.mu.Unlock()
	out := u.m
	u.m = make(map[string]int64)
	return out
}

func buildIncModels(counts map[string]int64, filterField string) []mongo.WriteModel {
	models := make([]mongo.WriteModel, 0, len(counts))
	for id, n := range counts {
		if n == 0 {
			continue
		}
		models = append(models, mongo.NewUpdateOneModel().
			SetFilter(bson.M{filterField: id}).
			SetUpdate(bson.M{"$inc": bson.M{"use_count": n}}))
	}
	return models
}

// FlushUseCounts 把内存中累积的计数批量写回；失败的批次计数直接丢弃。
func (s *SiteDb) FlushUseCounts(ctx context.Context) error {
	var errs []error
	if err := s.flushCounter(ctx, s.FilesByMd5(), s.md5Uses, "md5"); err != nil {
		errs = append(errs, err)
	}
	if err := s.flushCounter(ctx, s.FileKeys(), s.keyUses, "_id"); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (s *SiteDb) flushCounter(ctx context.Context, coll *mongo.Collection, u *useCounter, filterField string) error {
	counts := u.swap()
	if len(counts) == 0 {
		return nil
	}
	models := buildIncModels(counts, filterField)
	opts := options.BulkWrite().SetOrdered(false)
	var errs []error
	for start := 0; start < len(models); start += useCountChunk {
		end := min(start+useCountChunk, len(models))
		cctx, cancel := context.WithTimeout(ctx, useCountChunkTimeout)
		_, err := coll.BulkWrite(cctx, models[start:end], opts)
		cancel()
		if err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// StartUseCountFlusher 后台定期 flush；ctx 结束即退出，关停时由调用方再调一次 FlushUseCounts。
func (s *SiteDb) StartUseCountFlusher(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = 10 * time.Second
	}
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = s.FlushUseCounts(context.Background())
			}
		}
	}()
}
