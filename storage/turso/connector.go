// Package turso adapts the Turso Rust engine to PocketBase's database/sql contract.
package turso

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/pocketbase/dbx"
	engine "turso.tech/database/tursogo"
)

// Engine keeps transaction policy shared across all connections. Startup and
// migrations use exclusive transactions; callers enable concurrent transactions
// only after schema initialization is complete and before serving requests.
type Engine struct{ concurrent atomic.Bool }

func (e *Engine) SetConcurrent(enabled bool) { e.concurrent.Store(enabled) }

func (e *Engine) Connect(path string) (*dbx.DB, error) {
	if strings.ContainsAny(path, "?#") {
		return nil, errors.New("database path cannot contain ? or #")
	}
	params := url.Values{"experimental": {"mvcc,vacuum"}, "_busy_timeout": {"5000"}}
	base, err := engine.NewConnector(path + "?" + params.Encode())
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(&connector{base: base, concurrent: e.concurrent.Load})
	// dbx's sqlite dialect is independent of the underlying SQL driver.
	return dbx.NewFromDB(db, "sqlite"), nil
}

type connector struct {
	base       driver.Connector
	concurrent func() bool
}

func (c *connector) Driver() driver.Driver { return c.base.Driver() }
func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	raw, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	conn := &connection{Conn: raw, concurrent: c.concurrent}
	for _, query := range []string{"PRAGMA journal_mode=mvcc", "PRAGMA synchronous=FULL", "PRAGMA foreign_keys=ON"} {
		if _, err := conn.ExecContext(ctx, query, nil); err != nil {
			raw.Close()
			return nil, fmt.Errorf("initialize Turso (%s): %w", query, err)
		}
	}
	return conn, nil
}

type connection struct {
	driver.Conn
	concurrent func() bool
	invalid    atomic.Bool
	inTx       atomic.Bool
}

func (c *connection) IsValid() bool { return !c.invalid.Load() && !c.inTx.Load() }
func (c *connection) PrepareContext(ctx context.Context, q string) (driver.Stmt, error) {
	raw, err := c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, q)
	if err != nil {
		return nil, err
	}
	return &statement{Stmt: raw, conn: c, query: q}, nil
}
func (c *connection) Prepare(q string) (driver.Stmt, error) {
	return c.PrepareContext(context.Background(), q)
}

// Keep prepared writes under the same transaction policy as direct Exec.
type statement struct {
	driver.Stmt
	conn  *connection
	query string
}

func (s *statement) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	return s.conn.ExecContext(ctx, s.query, args)
}
func (s *statement) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	return s.conn.QueryContext(ctx, s.query, args)
}
func namedValues(values []driver.Value) []driver.NamedValue {
	args := make([]driver.NamedValue, len(values))
	for i, v := range values {
		args[i] = driver.NamedValue{Ordinal: i + 1, Value: v}
	}
	return args
}
func (s *statement) Exec(values []driver.Value) (driver.Result, error) {
	return s.ExecContext(context.Background(), namedValues(values))
}
func (s *statement) Query(values []driver.Value) (driver.Rows, error) {
	return s.QueryContext(context.Background(), namedValues(values))
}
func (c *connection) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	command := strings.ToUpper(strings.TrimSpace(q))
	first := ""
	if words := strings.Fields(command); len(words) > 0 {
		first = words[0]
	}
	// Core record writes also use autocommit statements. Give those the same
	// concurrent transaction mode as explicit application transactions.
	auto := c.concurrent() && !c.inTx.Load() && (first == "INSERT" || first == "UPDATE" || first == "DELETE" || first == "REPLACE" || first == "WITH")
	if auto {
		if _, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, "BEGIN CONCURRENT", nil); err != nil {
			return nil, err
		}
		c.inTx.Store(true)
	}
	if strings.EqualFold(strings.TrimSpace(q), "VACUUM") {
		if _, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)", nil); err != nil {
			return nil, err
		}
	}
	result, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	if err == nil && (strings.HasPrefix(command, "BEGIN ") || command == "BEGIN") {
		c.inTx.Store(true)
	}
	if err == nil && (command == "COMMIT" || command == "ROLLBACK") {
		c.inTx.Store(false)
	}
	if auto {
		if err == nil {
			_, err = c.Conn.(driver.ExecerContext).ExecContext(ctx, "COMMIT", nil)
		}
		if err != nil {
			if _, rollbackErr := c.Conn.(driver.ExecerContext).ExecContext(context.Background(), "ROLLBACK", nil); rollbackErr != nil {
				c.invalid.Store(true)
				err = errors.Join(err, rollbackErr)
			}
		}
		c.inTx.Store(false)
	}
	if err == nil && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(q)), "VACUUM INTO ") {
		// The v0.8.3 engine leaves the snapshot connection pinned after VACUUM
		// INTO. Discard it rather than returning stale reads to the pool.
		c.invalid.Store(true)
	}
	return result, err
}
func (c *connection) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}
func (c *connection) Ping(ctx context.Context) error {
	rows, err := c.QueryContext(ctx, "SELECT 1", nil)
	if err != nil {
		return err
	}
	return rows.Close()
}
func (c *connection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *connection) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if opts.Isolation != driver.IsolationLevel(sql.LevelDefault) || opts.ReadOnly {
		return nil, errors.New("Turso adapter does not implement explicit isolation or read-only transactions")
	}
	query := "BEGIN IMMEDIATE"
	if c.concurrent() {
		query = "BEGIN CONCURRENT"
	}
	if _, err := c.ExecContext(ctx, query, nil); err != nil {
		return nil, err
	}
	return &transaction{conn: c, ctx: ctx}, nil
}

type transaction struct {
	conn *connection
	ctx  context.Context
	done bool
}

func (t *transaction) Commit() error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	_, err := t.conn.ExecContext(t.ctx, "COMMIT", nil)
	if err != nil {
		// database/sql will not call Rollback after Commit returns. Clean up even
		// on cancellation, and discard a connection if its state is uncertain.
		_, rollbackErr := t.conn.ExecContext(context.Background(), "ROLLBACK", nil)
		if rollbackErr != nil {
			t.conn.invalid.Store(true)
		}
		return errors.Join(err, rollbackErr)
	}
	return nil
}
func (t *transaction) Rollback() error {
	if t.done {
		return sql.ErrTxDone
	}
	t.done = true
	_, err := t.conn.ExecContext(context.Background(), "ROLLBACK", nil)
	if err != nil {
		t.conn.invalid.Store(true)
	}
	return err
}
