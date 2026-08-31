// Package database 封装控制面 PostgreSQL 连接与健康检查。
package database

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Validate 只校验 PostgreSQL 连接串格式，不建立连接或执行 DDL。
func Validate(databaseURL string) error {
	if _, err := pgxpool.ParseConfig(databaseURL); err != nil {
		return errors.New("数据库连接配置无效")
	}
	return nil
}

// Open 创建惰性连接池；依赖可用性由就绪探针持续判断。
func Open(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, errors.New("数据库连接配置无效")
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, errors.New("数据库连接池初始化失败")
	}
	return pool, nil
}

// Checker 使用 PostgreSQL Ping 判断依赖是否满足就绪条件。
type Checker struct {
	pool *pgxpool.Pool
}

// NewChecker 创建只暴露健康语义而不暴露连接池的检查器。
func NewChecker(pool *pgxpool.Pool) *Checker {
	return &Checker{pool: pool}
}

// Check 执行一次受调用方上下文约束的数据库探测。
func (checker *Checker) Check(ctx context.Context) error {
	return checker.pool.Ping(ctx)
}
