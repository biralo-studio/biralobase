package turso

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"reflect"
	"testing"
)

type fakeConn struct {
	driver.Conn
	queries []string
	errors  map[string]error
}

func (c *fakeConn) ExecContext(ctx context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.queries = append(c.queries, q)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return driver.RowsAffected(0), c.errors[q]
}

func TestTransactionLifecycle(t *testing.T) {
	conflict := errors.New("write conflict")
	for _, tc := range []struct {
		name                   string
		concurrent             bool
		commitErr, rollbackErr error
		invalid                bool
	}{
		{name: "exclusive"}, {name: "concurrent", concurrent: true},
		{name: "conflict cleanup", concurrent: true, commitErr: conflict},
		{name: "poisoned connection", commitErr: conflict, rollbackErr: errors.New("rollback failed"), invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := &fakeConn{errors: map[string]error{"COMMIT": tc.commitErr, "ROLLBACK": tc.rollbackErr}}
			conn := &connection{Conn: raw, concurrent: func() bool { return tc.concurrent }}
			tx, err := conn.BeginTx(context.Background(), driver.TxOptions{})
			if err != nil {
				t.Fatal(err)
			}
			err = tx.Commit()
			if !errors.Is(err, tc.commitErr) {
				t.Fatalf("commit: %v", err)
			}
			want := []string{"BEGIN IMMEDIATE", "COMMIT"}
			if tc.concurrent {
				want[0] = "BEGIN CONCURRENT"
			}
			if tc.commitErr != nil {
				want = append(want, "ROLLBACK")
			}
			if !reflect.DeepEqual(raw.queries, want) {
				t.Fatalf("queries %v, want %v", raw.queries, want)
			}
			if conn.IsValid() == tc.invalid {
				t.Fatal("incorrect connection validity")
			}
			if !errors.Is(tx.Rollback(), sql.ErrTxDone) {
				t.Fatal("completed transaction reused")
			}
		})
	}
}

func TestCancelledCommitStillRollsBack(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	raw := &fakeConn{}
	c := &connection{Conn: raw, concurrent: func() bool { return true }}
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if !errors.Is(tx.Commit(), context.Canceled) {
		t.Fatal("cancelled commit must fail")
	}
	if !c.IsValid() || raw.queries[len(raw.queries)-1] != "ROLLBACK" {
		t.Fatal("rollback must run outside cancelled context")
	}
}

func TestUnsupportedTransactionOptions(t *testing.T) {
	c := &connection{Conn: &fakeConn{}, concurrent: func() bool { return true }}
	for _, opts := range []driver.TxOptions{{ReadOnly: true}, {Isolation: driver.IsolationLevel(sql.LevelSerializable)}} {
		if _, err := c.BeginTx(context.Background(), opts); err == nil {
			t.Fatal("must reject unsupported guarantees")
		}
	}
}

func TestPreparedAutocommitDoesNotReplayConflicts(t *testing.T) {
	conflict := errors.New("commit conflict")
	raw := &fakeConn{errors: map[string]error{"COMMIT": conflict}}
	c := &connection{Conn: raw, concurrent: func() bool { return true }}
	s := &statement{conn: c, query: "INSERT\nINTO notes VALUES (?)"}
	if _, err := s.ExecContext(context.Background(), []driver.NamedValue{{Ordinal: 1, Value: "value"}}); !errors.Is(err, conflict) {
		t.Fatal(err)
	}
	want := []string{"BEGIN CONCURRENT", s.query, "COMMIT", "ROLLBACK"}
	if !reflect.DeepEqual(raw.queries, want) {
		t.Fatalf("unexpected replay/transaction policy: %v", raw.queries)
	}
	if c.inTx.Load() || !c.IsValid() {
		t.Fatal("connection was not reset")
	}
}
