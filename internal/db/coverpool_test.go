package db

import (
	"context"
	"testing"
)

// 配置服务可直接持有 *CoverPoolStore，无需为 Mongo 额外抽象接口。
type coverPoolConfigStore interface {
	Get(context.Context) (int, error)
	Set(context.Context, int) error
}

var _ coverPoolConfigStore = (*CoverPoolStore)(nil)

func TestCoverPoolStoreContract(t *testing.T) {
	// nil receiver 可验证方法集契约；实际读写与错误传递由 Mongo 集成测试覆盖。
	var store coverPoolConfigStore = (*CoverPoolStore)(nil)
	if store == nil {
		t.Fatal("CoverPoolStore does not satisfy coverPoolConfigStore")
	}
}

func TestCoverPoolStoreSetRejectsInvalidCount(t *testing.T) {
	store := &CoverPoolStore{}
	for _, count := range []int{-1, 0, 101} {
		if err := store.Set(context.Background(), count); err == nil {
			t.Errorf("Set(%d) returned nil error", count)
		}
	}
}
