package turso

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

// engineConn records how many COMMITs are inside the (fake) native engine at
// once. Each COMMIT stays there until the test lets it finish.
type engineConn struct {
	driver.Conn
	inside, peak *atomic.Int32
	entered      chan<- struct{}
	finish       <-chan struct{}
	queries      []string
}

func (c *engineConn) ExecContext(_ context.Context, q string, _ []driver.NamedValue) (driver.Result, error) {
	c.queries = append(c.queries, q)
	if q == "COMMIT" {
		now := c.inside.Add(1)
		for {
			peak := c.peak.Load()
			if now <= peak || c.peak.CompareAndSwap(peak, now) {
				break
			}
		}
		c.entered <- struct{}{}
		<-c.finish
		c.inside.Add(-1)
	}
	return driver.RowsAffected(0), nil
}

func TestContendedCommitsWaitOutsideTheEngine(t *testing.T) {
	const writers = 8
	gate := commitGate(filepath.Join(t.TempDir(), "data.db"))
	var inside, peak atomic.Int32
	entered, finish := make(chan struct{}, writers), make(chan struct{})
	var group sync.WaitGroup
	// Explicit transactions and autocommit writes must share the same turn.
	for writer := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			c := &connection{Conn: &engineConn{inside: &inside, peak: &peak, entered: entered, finish: finish}, concurrent: func() bool { return true }, commits: gate}
			if writer%2 == 0 {
				tx, err := c.BeginTx(context.Background(), driver.TxOptions{})
				if err == nil {
					err = tx.Commit()
				}
				if err != nil {
					t.Error(err)
				}
			} else if _, err := c.ExecContext(context.Background(), "INSERT INTO notes VALUES (1)", nil); err != nil {
				t.Error(err)
			}
		}()
	}
	for range writers {
		<-entered
		// Give a broken gate time to admit a second committer before this one
		// leaves. A delay here can only hide a failure, never cause one.
		time.Sleep(5 * time.Millisecond)
		finish <- struct{}{}
	}
	group.Wait()
	if peak.Load() != 1 {
		t.Fatalf("%d commits were inside the engine at once; waiting committers spin there", peak.Load())
	}
}

func TestCommitAbandonedWhileWaitingRollsBack(t *testing.T) {
	gate := commitGate(filepath.Join(t.TempDir(), "data.db"))
	gate <- struct{}{} // another transaction is committing
	ctx, cancel := context.WithCancel(context.Background())
	raw := &fakeConn{}
	c := &connection{Conn: raw, concurrent: func() bool { return true }, commits: gate}
	tx, err := c.BeginTx(ctx, driver.TxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- tx.Commit() }()
	select {
	case err = <-done:
		t.Fatalf("commit did not wait for its turn: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	if err = <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned commit: %v", err)
	}
	if want := []string{"BEGIN CONCURRENT", "ROLLBACK"}; !reflect.DeepEqual(raw.queries, want) {
		t.Fatalf("queries %v, want %v: an abandoned commit must not reach the engine", raw.queries, want)
	}
	if !c.IsValid() || len(gate) != 1 {
		t.Fatal("abandoned commit changed the connection or released another transaction's turn")
	}
}

func TestPoolsOfOneDatabaseShareACommitGate(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	if commitGate("data.db") != commitGate(filepath.Join(dir, "sub", "..", "data.db")) {
		t.Fatal("two spellings of one database path commit independently")
	}
	if commitGate("data.db") == commitGate("auxiliary.db") {
		t.Fatal("separate databases must not wait for each other")
	}
}

func TestBackupSnapshotIsReleasedBeforeTheConnectionIsDiscarded(t *testing.T) {
	const backup = "VACUUM INTO '/tmp/copy.db'"
	raw := &fakeConn{}
	c := &connection{Conn: raw, concurrent: func() bool { return true }}
	if _, err := c.ExecContext(context.Background(), backup, nil); err != nil {
		t.Fatal(err)
	}
	if want := []string{backup, "BEGIN", "ROLLBACK"}; !reflect.DeepEqual(raw.queries, want) {
		t.Fatalf("queries %v, want %v", raw.queries, want)
	}
	if c.IsValid() {
		t.Fatal("snapshot connection returned to the pool")
	}
	// A snapshot that cannot be released blocks every later checkpoint, so the
	// backup must not be reported as a success.
	stuck := errors.New("engine refused")
	raw = &fakeConn{errors: map[string]error{"ROLLBACK": stuck}}
	c = &connection{Conn: raw, concurrent: func() bool { return true }}
	if _, err := c.ExecContext(context.Background(), backup, nil); !errors.Is(err, stuck) || c.IsValid() {
		t.Fatalf("unreleased snapshot was hidden: %v", err)
	}
}
