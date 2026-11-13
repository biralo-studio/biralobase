//go:build linux || darwin

package turso_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/storage/turso"
)

func TestFailedBackupDoesNotStopCheckpoints(t *testing.T) {
	for _, destination := range []string{"existing", "missing-directory"} {
		t.Run(destination, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "data.db")
			e := new(turso.Engine)
			db, err := e.Connect(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.DB().SetMaxOpenConns(1)
			db.DB().SetMaxIdleConns(1)
			if _, err = db.DB().Exec("CREATE TABLE workload(id INTEGER PRIMARY KEY,payload TEXT)"); err != nil {
				t.Fatal(err)
			}
			e.SetConcurrent(true)
			if _, err = db.DB().Exec("INSERT INTO workload VALUES (1,'before backup')"); err != nil {
				t.Fatal(err)
			}
			copyPath := filepath.Join(dir, "missing", "copy.db")
			if destination == "existing" {
				copyPath = filepath.Join(dir, "copy.db")
				if err = os.WriteFile(copyPath, []byte("occupied"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = db.DB().Exec("VACUUM INTO '" + copyPath + "'"); err == nil {
				t.Fatal("invalid backup destination was accepted")
			}
			if _, err = db.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
				t.Fatal("checkpoint after failed backup:", err)
			}
			before := size(t, path)
			payload := strings.Repeat("x", 4096)
			for range 2500 {
				if _, err = db.DB().Exec("INSERT INTO workload(payload) VALUES (?)", payload); err != nil {
					t.Fatal("write after failed backup:", err)
				}
			}
			if grown, log := size(t, path)-before, size(t, path+"-log"); grown < 8<<20 || log > 8<<20 {
				t.Fatalf("after a failed backup, 10 MiB of rows grew the database by %d bytes and left a %d byte log: checkpoints are blocked", grown, log)
			}
			var rows int
			if err = db.DB().QueryRow("SELECT count(*) FROM workload").Scan(&rows); err != nil || rows != 2501 {
				t.Fatalf("committed data after failed backup: %d %v", rows, err)
			}
		})
	}
}

func processCPU(t *testing.T) time.Duration {
	t.Helper()
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// A committer waiting for another transaction must not occupy a CPU. Before
// commits were ordered in Go, sixteen writers used 9-30 times the CPU of one
// writer for every committed row, on every platform measured; ordered commits
// stay within 1.5 times. Wall-clock time is deliberately not asserted.
func TestWaitingCommittersDoNotConsumeCPU(t *testing.T) {
	const commits = 3000
	measure := func(writers int) time.Duration {
		e := new(turso.Engine)
		db, err := e.Connect(filepath.Join(t.TempDir(), "data.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		db.DB().SetMaxOpenConns(writers)
		db.DB().SetMaxIdleConns(writers)
		if _, err = db.DB().Exec("CREATE TABLE workload(id INTEGER PRIMARY KEY,payload TEXT)"); err != nil {
			t.Fatal(err)
		}
		e.SetConcurrent(true)
		payload := strings.Repeat("x", 256)
		var next atomic.Int64
		var group sync.WaitGroup
		started := processCPU(t)
		for range writers {
			group.Add(1)
			go func() {
				defer group.Done()
				for {
					id := next.Add(1)
					if id > commits {
						return
					}
					if _, err := db.DB().ExecContext(context.Background(), "INSERT INTO workload VALUES (?,?)", id, payload); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		group.Wait()
		used := processCPU(t) - started
		var stored int
		if err = db.DB().QueryRow("SELECT count(*) FROM workload").Scan(&stored); err != nil || stored != commits {
			t.Fatalf("stored %d of %d acknowledged writes: %v", stored, commits, err)
		}
		return used / commits
	}
	single := measure(1)
	contended := measure(16)
	t.Logf("CPU per committed write: %v with one writer, %v with sixteen", single, contended)
	if contended > 4*single {
		t.Fatalf("sixteen writers used %v of CPU per write, %.1f times one writer (%v): waiting committers are spinning", contended, float64(contended)/float64(single), single)
	}
}

func size(t *testing.T, path string) int64 {
	t.Helper()
	info, err := filepath.Glob(path)
	if err != nil || len(info) != 1 {
		t.Fatalf("missing %s", path)
	}
	var stat syscall.Stat_t
	if err = syscall.Stat(path, &stat); err != nil {
		t.Fatal(err)
	}
	return stat.Size
}

// The engine keeps committed rows in its log and in memory until a checkpoint
// moves them into the database file. VACUUM INTO used to leave a snapshot that
// made every later checkpoint fail as busy for the life of the process: after
// one backup the log grew without bound while the database file stayed empty.
func TestBackupDoesNotStopCheckpoints(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	e := new(turso.Engine)
	db, err := e.Connect(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.DB().Exec("CREATE TABLE workload(id INTEGER PRIMARY KEY,payload TEXT)"); err != nil {
		t.Fatal(err)
	}
	e.SetConcurrent(true)
	payload := strings.Repeat("x", 4096)
	write := func(rows int) {
		t.Helper()
		for range rows {
			if _, err := db.DB().Exec("INSERT INTO workload(payload) VALUES (?)", payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	write(100)
	// Take the backup while other writers are committing, as a live server does.
	stop, writing := make(chan struct{}), make(chan struct{})
	var background atomic.Int64
	go func() {
		defer close(writing)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := db.DB().Exec("INSERT INTO workload(payload) VALUES ('concurrent')"); err != nil {
				t.Error(err)
				return
			}
			background.Add(1)
		}
	}()
	copyPath := filepath.Join(dir, "copy.db")
	_, backupErr := db.DB().Exec("VACUUM INTO '" + copyPath + "'")
	close(stop)
	<-writing
	if backupErr != nil {
		t.Fatal(backupErr)
	}
	started := time.Now()
	if _, err = db.DB().Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatalf("checkpoint after backup failed after %v: %v", time.Since(started).Round(time.Millisecond), err)
	}
	// Ten megabytes is several times the engine's automatic checkpoint
	// threshold, so the rows must reach the database file and leave the log.
	before := size(t, path)
	write(2500)
	if grown, log := size(t, path)-before, size(t, path+"-log"); grown < 8<<20 || log > 8<<20 {
		t.Fatalf("after a backup, 10 MiB of rows grew the database file by %d bytes and left a %d byte log: checkpoints are blocked", grown, log)
	}
	// The snapshot itself must be a complete, independent database.
	snapshot, err := new(turso.Engine).Connect(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	var rows int64
	if err = snapshot.DB().QueryRow("SELECT count(*) FROM workload").Scan(&rows); err != nil || rows < 100 || rows > 100+background.Load() {
		t.Fatalf("backup holds %d rows, expected between 100 and %d: %v", rows, 100+background.Load(), err)
	}
}
