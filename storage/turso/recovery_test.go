package turso_test

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/storage/turso"
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
// verification, explicit rollback, and periodic clean reopen. No errors are
// hidden by retries. Run the API/SDK and crash suites alongside this gate.
func TestStorageSoak(t *testing.T) {
	raw := os.Getenv("BIRALOBASE_SOAK_DURATION")
	if raw == "" {
		t.Skip("set BIRALOBASE_SOAK_DURATION for the continuous soak gate")
	}
	duration, err := time.ParseDuration(raw)
	if err != nil || duration <= 0 {
		t.Fatal("invalid soak duration")
	}
	path := filepath.Join(t.TempDir(), "soak.db")
	deadline := time.Now().Add(duration)
	var count atomic.Int64
	for round := 0; time.Now().Before(deadline); round++ {
		e := new(turso.Engine)
		db, err := e.Connect(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.DB().Exec("CREATE TABLE IF NOT EXISTS workload(id TEXT PRIMARY KEY,value INTEGER)"); err != nil {
			db.Close()
			t.Fatal(err)
		}
		e.SetConcurrent(true)
		db.DB().SetMaxOpenConns(8)
		var wg sync.WaitGroup
		failures := make(chan error, 8)
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
			t.Error(err)
		}
		var stored int64
		if err = db.DB().QueryRow("SELECT count(*) FROM workload").Scan(&stored); err != nil {
			t.Error(err)
		} else if stored != count.Load() {
			t.Errorf("stored %d, acknowledged %d", stored, count.Load())
		}
		if err = db.Close(); err != nil {
			t.Error(err)
		}
		if t.Failed() {
			return
		}
	}
	t.Logf("verified %d concurrent writes and rollbacks across %s", count.Load(), duration)
}
