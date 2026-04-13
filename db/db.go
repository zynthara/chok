package db

import (
	"context"
	"database/sql"
	"fmt"

	"gorm.io/driver/mysql"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/zynthara/chok/config"
)

// NewMySQL creates a GORM DB connected to MySQL.
func NewMySQL(opts *config.MySQLOptions) (*gorm.DB, error) {
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%d)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		opts.Username, opts.Password, opts.Host, opts.Port, opts.Database)

	gdb, err := gorm.Open(mysql.Open(dsn), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("db: open mysql: %w", err)
	}

	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("db: get underlying sql.DB: %w", err)
	}
	sqlDB.SetMaxOpenConns(opts.MaxOpenConns)
	sqlDB.SetMaxIdleConns(opts.MaxIdleConns)
	sqlDB.SetConnMaxLifetime(opts.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(opts.ConnMaxIdleTime)

	return gdb, nil
}

// NewSQLite creates a GORM DB using SQLite.
func NewSQLite(opts *config.SQLiteOptions) (*gorm.DB, error) {
	gdb, err := gorm.Open(sqlite.Open(opts.Path), &gorm.Config{
		Logger: logger.Discard,
	})
	if err != nil {
		return nil, fmt.Errorf("db: open sqlite: %w", err)
	}
	// Enable WAL mode for concurrency.
	gdb.Exec("PRAGMA journal_mode=WAL")
	return gdb, nil
}

// Close closes the underlying connection pool.
// Intended for use with App.AddCleanup.
func Close(gdb *gorm.DB) error {
	sqlDB, err := gdb.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// Transaction wraps fn in Begin/Commit/Rollback.
// ctx is propagated to all DB operations inside fn.
func Transaction(ctx context.Context, gdb *gorm.DB, fn func(tx *gorm.DB) error) error {
	tx := gdb.WithContext(ctx).Begin(&sql.TxOptions{})
	if tx.Error != nil {
		return tx.Error
	}

	defer func() {
		if r := recover(); r != nil {
			tx.Rollback()
			panic(r)
		}
	}()

	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit().Error
}


