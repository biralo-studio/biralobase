package turso_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/storage/turso"
	"github.com/pocketbase/dbx"
)

func TestCrashRecovery(t *testing.T) {
	if path := os.Getenv("BIRALOBASE_TEST_CRASH_PATH"); path != "" {
		e := new(turso.Engine)
		db, err := e.Connect(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.DB().Exec("CREATE TABLE durable(id INTEGER PRIMARY KEY,value TEXT)"); err != nil {
			t.Fatal(err)
		}
		e.SetConcurrent(true)
		for i := 0; i < 100; i++ {
			if _, err = db.DB().Exec("INSERT INTO durable VALUES (?,?)", i, fmt.Sprintf("value-%d", i)); err != nil {
				t.Fatal(err)
			}
		}
		tx, err := db.DB().Begin()
		if err != nil {
			t.Fatal(err)
		}
		if _, err = tx.Exec("INSERT INTO durable VALUES (100,'must roll back')"); err != nil {
			t.Fatal(err)
		}
		fmt.Println("COMMITTED_AND_PENDING")
		time.Sleep(time.Hour)
		return
	}
	path := filepath.Join(t.TempDir(), "crash.db")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCrashRecovery$")
	cmd.Env = append(os.Environ(), "BIRALOBASE_TEST_CRASH_PATH="+path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := false
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if scanner.Text() == "COMMITTED_AND_PENDING" {
			ready = true
			break
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if !ready {
		t.Fatalf("child did not reach crash point: %s", stderr.String())
	}
	e := new(turso.Engine)
	db, err := e.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.DB().Query("SELECT id,value FROM durable ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int
		var value string
		if err = rows.Scan(&id, &value); err != nil {
			t.Fatal(err)
		}
		if id != count || value != fmt.Sprintf("value-%d", count) {
			t.Fatal("committed content changed during recovery")
		}
		count++
	}
	if rows.Err() != nil || count != 100 {
		t.Fatalf("recovered %d rows: %v", count, rows.Err())
	}
}

// Opt-in, continuous storage workload with disjoint concurrent writers, read
// verification and explicit rollback. No errors are hidden by retries. Run the
// API/SDK and crash suites alongside this gate.
//
// Two databases are exercised for the whole duration. The "reopened" lane
// closes and reopens its database after every round, verifying persistent
// contents each time. The "long-lived" lane keeps one handle open like a
// server process, takes a backup under load every few rounds, and requires
// checkpoints to keep the MVCC log bounded. A lane that only reopens cannot
// observe state that accumulates in a running process.
func TestStorageSoak(t *testing.T) {
	raw := os.Getenv("BIRALOBASE_SOAK_DURATION")
	if raw == "" {
		t.Skip("set BIRALOBASE_SOAK_DURATION for the continuous soak gate")
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		t.Fatal("invalid soak duration")
	}
	dir := t.TempDir()
	deadline := time.Now().Add(duration)
	var lanes sync.WaitGroup
	for _, lane := range []struct {
		name      string
		longLived bool
	}{{"reopened", false}, {"long-lived", true}} {
		lanes.Add(1)
		go func() {
			defer lanes.Done()
			soakLane(t, lane.name, filepath.Join(dir, lane.name+".db"), deadline, lane.longLived)
		}()
	}
	lanes.Wait()
}

// Checkpoints start once the log passes roughly 4 MiB. A log this large means
// they have stopped and committed rows are accumulating in memory.
const soakLogLimit = 64 << 20

func soakLane(t *testing.T, name, path string, deadline time.Time, longLived bool) {
	nextProgress := time.Now().Add(time.Minute)
	var count atomic.Int64
	var retained, backups int64
	var e *turso.Engine
	var db *dbx.DB
	open := func() bool {
		var err error
		e = new(turso.Engine)
		if db, err = e.Connect(path); err != nil {
			t.Errorf("%s: %v", name, err)
			return false
		}
		if _, err = db.DB().Exec("CREATE TABLE IF NOT EXISTS workload(id TEXT PRIMARY KEY,value INTEGER)"); err != nil {
			t.Errorf("%s: %v", name, err)
			return false
		}
		e.SetConcurrent(true)
		db.DB().SetMaxOpenConns(8)
		return true
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	round := 0
	for ; time.Now().Before(deadline) && !t.Failed(); round++ {
		if db == nil && !open() {
			return
		}
		var wg sync.WaitGroup
		failures := make(chan error, 9)
		// A backup taken while the writers below are committing must be a
		// consistent copy holding every row acknowledged before it started.
		before := count.Load() - retained
		if longLived && round%20 == 19 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				copyPath := path + ".backup"
				defer os.Remove(copyPath)
				if _, err := db.DB().Exec("VACUUM INTO '" + copyPath + "'"); err != nil {
					failures <- fmt.Errorf("backup: %w", err)
					return
				}
				snapshot, err := new(turso.Engine).Connect(copyPath)
				if err != nil {
					failures <- fmt.Errorf("open backup: %w", err)
					return
				}
				defer snapshot.Close()
				var rows int64
				if err = snapshot.DB().QueryRow("SELECT count(*) FROM workload").Scan(&rows); err != nil {
					failures <- fmt.Errorf("read backup: %w", err)
				} else if rows < before || rows > before+800 {
					failures <- fmt.Errorf("backup holds %d rows; %d were acknowledged before it and at most 800 during it", rows, before)
				}
				backups++
			}()
		}
		for worker := 0; worker < 8; worker++ {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := 0; i < 100 && time.Now().Before(deadline); i++ {
					id := fmt.Sprintf("%d-%d-%d", round, worker, i)
					if _, err := db.DB().Exec("INSERT INTO workload VALUES (?,?)", id, i); err != nil {
						failures <- err
						return
					}
					tx, err := db.DB().Begin()
					if err != nil {
						failures <- err
						return
					}
					_, err = tx.Exec("UPDATE workload SET value=-1 WHERE id=?", id)
					rollbackErr := tx.Rollback()
					if err != nil {
						failures <- err
						return
					}
					if rollbackErr != nil {
						failures <- rollbackErr
						return
					}
					var got int
					if err = db.DB().QueryRow("SELECT value FROM workload WHERE id=?", id).Scan(&got); err != nil {
						failures <- err
						return
					}
					if got != i {
						failures <- fmt.Errorf("rollback/read mismatch: %s", id)
						return
					}
					count.Add(1)
				}
			}(worker)
		}
		wg.Wait()
		close(failures)
		for err := range failures {
			t.Errorf("%s: %v", name, err)
		}
		var stored int64
		if err := db.DB().QueryRow("SELECT count(*) FROM workload").Scan(&stored); err != nil {
			t.Errorf("%s: %v", name, err)
		} else if stored != count.Load()-retained {
			t.Errorf("%s: stored %d, acknowledged since pruning %d", name, stored, count.Load()-retained)
		}
		// Keep a bounded working set during 6/24-hour gates. Every row is
		// verified above before deletion. This also exercises deletion and
		// space reuse instead of benchmarking only an ever-growing table.
		if stored >= 10000 {
			if _, err := db.DB().Exec("DELETE FROM workload"); err != nil {
				t.Errorf("%s: %v", name, err)
			} else {
				retained = count.Load()
			}
		}
		logSize := int64(0)
		if info, err := os.Stat(path + "-log"); err == nil {
			logSize = info.Size()
		}
		if logSize > soakLogLimit {
			t.Errorf("%s: MVCC log reached %d bytes; checkpoints have stopped", name, logSize)
		}
		if !longLived {
			if err := db.Close(); err != nil {
				t.Errorf("%s: %v", name, err)
			}
			db = nil
		}
		if time.Now().After(nextProgress) {
			t.Logf("progress[%s]: writes=%d rounds=%d backups=%d log=%dKiB peak_rss=%dMiB remaining=%s", name, count.Load(), round+1, backups, logSize>>10, peakRSS()>>20, time.Until(deadline).Round(time.Second))
			nextProgress = time.Now().Add(time.Minute)
		}
	}
	if t.Failed() {
		return
	}
	// Whatever a long-lived process acknowledged must survive a clean reopen.
	if db != nil {
		if err := db.Close(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		db = nil
	}
	if !open() {
		return
	}
	var stored int64
	if err := db.DB().QueryRow("SELECT count(*) FROM workload").Scan(&stored); err != nil {
		t.Errorf("%s: %v", name, err)
	} else if stored != count.Load()-retained {
		t.Errorf("%s: reopened with %d rows, acknowledged since pruning %d", name, stored, count.Load()-retained)
	}
	t.Logf("verified[%s] %d concurrent writes and rollbacks, %d backups, across %d rounds", name, count.Load(), backups, round)
}

// peakRSS reports the process high-water mark in bytes.
func peakRSS() int64 {
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) != nil {
		return 0
	}
	if runtime.GOOS == "darwin" {
		return int64(usage.Maxrss)
	}
	return int64(usage.Maxrss) << 10
}
