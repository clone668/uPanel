package auth

import (
	"context"
	"database/sql"
	"errors"
)

// InTransaction 在一个事务里执行 fn，供接口层把“业务变更 + 审计”合成一笔事务。
//
// fn 内部调用本 Service 的写方法时，它们通过 store.WithTx 的事务传播加入同一事务，
// 因此：fn 返回错误 -> 业务与审计一起回滚；fn 返回 nil -> 一起提交。
//
// 为什么放在这里而不是直接暴露 *store.DB：接口层只需要“在这个服务的事务里执行一件事”，
// 不需要拿到数据库句柄，也避免有人在事务外顺手写库。
func (s *Service) InTransaction(ctx context.Context, fn func(ctx context.Context) error) error {
	if fn == nil {
		return errors.New("auth: InTransaction 需要非空函数")
	}
	return s.db.WithTx(ctx, func(ctx context.Context, _ *sql.Tx) error {
		return fn(ctx)
	})
}
