package turso_test

import (
	"database/sql"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/biralo-studio/biralobase/storage/turso"
	_ "modernc.org/sqlite"
)

// Compare durable, indexed inserts with PocketBase's single-writer policy and
// Turso's concurrent pool. This is a storage microbenchmark, not an API SLA.
func BenchmarkIndexedInserts(b *testing.B) {
	for _, name := range []string{"SQLite_single_writer", "Turso_single_writer", "Turso_concurrent_writers"} {
		b.Run(name, func(b *testing.B) {
			var db *sql.DB
			var err error
			e := new(turso.Engine)
			path := filepath.Join(b.TempDir(), "benchmark.db")
			if name == "SQLite_single_writer" {
				db, err = sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
			} else {
				wrapped, openErr := e.Connect(path)
				err = openErr
				if wrapped != nil {
					db = wrapped.DB()
				}
			}
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			if _, err = db.Exec("CREATE TABLE workload(id INTEGER PRIMARY KEY,category TEXT,payload TEXT)"); err != nil {
				b.Fatal(err)
			}
			if _, err = db.Exec("CREATE INDEX category_idx ON workload(category)"); err != nil {
				b.Fatal(err)
			}
			if name == "Turso_concurrent_writers" {
				db.SetMaxOpenConns(16)
			}
			if name != "SQLite_single_writer" {
				e.SetConcurrent(true)
			}
			var next atomic.Int64
			payload := strings.Repeat("x", 512)
			b.ResetTimer()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					id := next.Add(1)
					if _, err := db.Exec("INSERT INTO workload VALUES (?,?,?)", id, "notes", payload); err != nil {
						b.Error(err)
						return
					}
				}
			})
			b.StopTimer()
			var stored int64
			if err = db.QueryRow("SELECT count(*) FROM workload").Scan(&stored); err != nil || stored != int64(b.N) {
				b.Fatalf("stored %d, expected %d: %v", stored, b.N, err)
			}
		})
	}
}
