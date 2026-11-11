package runtime_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/bundle"
	"github.com/biralo-studio/biralobase/internal/provision"
	server "github.com/biralo-studio/biralobase/internal/runtime"
	"github.com/biralo-studio/biralobase/internal/schema"
	"github.com/pocketbase/dbx"
	_ "modernc.org/sqlite"
)

// benchmarkTarget is one fully assembled HTTP handler and the pools behind it.
type benchmarkTarget struct {
	handler http.Handler
	token   string
	pools   map[string]*sql.DB
	close   func()
}

func benchmarkPools(app core.App) map[string]*sql.DB {
	return map[string]*sql.DB{
		"data-read":  app.ConcurrentDB().(*dbx.DB).DB(),
		"data-write": app.NonconcurrentDB().(*dbx.DB).DB(),
		"aux-read":   app.AuxConcurrentDB().(*dbx.DB).DB(),
		"aux-write":  app.AuxNonconcurrentDB().(*dbx.DB).DB(),
	}
}

// openBenchmarkRuntime starts the production assembly: immutable bundle,
// generated migration, managed administrator and concurrent Turso writes.
func openBenchmarkRuntime(b *testing.B) benchmarkTarget {
	b.Helper()
	root := b.TempDir()
	source, release := filepath.Join(root, "source"), filepath.Join(root, "release")
	s, err := schema.Default()
	if err != nil {
		b.Fatal(err)
	}
	s.Collections = append(s.Collections, map[string]any{"id": "bbreviewitems001", "name": "bb_review_items", "type": "base", "fields": []any{
		map[string]any{"id": "bbkeyfield001", "name": "key", "type": "text"},
		map[string]any{"id": "bbpayload001", "name": "payload", "type": "json"},
		map[string]any{"id": "bbvaluefield1", "name": "value", "type": "number"},
	}})
	if err = schema.Write(filepath.Join(source, schema.FileName), s); err != nil {
		b.Fatal(err)
	}
	if _, err = schema.Generate(source, false); err != nil {
		b.Fatal(err)
	}
	if _, err = bundle.Build(source, release, "benchmark", server.Version, bundle.Platform()); err != nil {
		b.Fatal(err)
	}
	c := provision.Config{AppDir: release, DataDir: filepath.Join(root, "data"), HTTP: "127.0.0.1:0", PublicURL: "https://benchmark.example.test", AdminEmail: "admin@example.com", AdminPassword: "benchmark-secret-password", EncryptionKey: strings.Repeat("k", 32)}
	srv, err := server.Open(context.Background(), c)
	if err != nil {
		b.Fatal(err)
	}
	routes, err := apis.NewRouter(srv.App)
	if err != nil {
		b.Fatal(err)
	}
	srv.Bind(routes)
	handler, err := routes.BuildMux()
	if err != nil {
		b.Fatal(err)
	}
	admin, err := srv.App.FindAuthRecordByEmail(core.CollectionNameSuperusers, c.AdminEmail)
	if err != nil {
		b.Fatal(err)
	}
	token, err := admin.NewAuthToken()
	if err != nil {
		b.Fatal(err)
	}
	return benchmarkTarget{handler: handler, token: token, pools: benchmarkPools(srv.App), close: func() { _ = srv.Close() }}
}

// openBenchmarkReference starts unmodified PocketBase request handling on the
// SQLite driver. synchronous selects the durability level being compared.
func openBenchmarkReference(b *testing.B, synchronous string) benchmarkTarget {
	b.Helper()
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: filepath.Join(b.TempDir(), "data"), DBConnect: func(path string) (*dbx.DB, error) {
		return dbx.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(200000000)&_pragma=synchronous("+synchronous+")&_pragma=foreign_keys(ON)&_pragma=temp_store(MEMORY)&_pragma=cache_size(-32000)&_defensive=1")
	}})
	if err := app.Bootstrap(); err != nil {
		b.Fatal(err)
	}
	collection := core.NewBaseCollection("bb_review_items")
	collection.Fields.Add(&core.TextField{Name: "key"}, &core.JSONField{Name: "payload"}, &core.NumberField{Name: "value"})
	if err := app.Save(collection); err != nil {
		b.Fatal(err)
	}
	superusers, err := app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		b.Fatal(err)
	}
	admin := core.NewRecord(superusers)
	admin.SetEmail("admin@example.com")
	admin.SetPassword("benchmark-secret-password")
	if err = app.Save(admin); err != nil {
		b.Fatal(err)
	}
	token, err := admin.NewAuthToken()
	if err != nil {
		b.Fatal(err)
	}
	routes, err := apis.NewRouter(app)
	if err != nil {
		b.Fatal(err)
	}
	handler, err := routes.BuildMux()
	if err != nil {
		b.Fatal(err)
	}
	return benchmarkTarget{handler: handler, token: token, pools: benchmarkPools(app), close: func() {
		_ = app.OnTerminate().Trigger(&core.TerminateEvent{App: app}, func(e *core.TerminateEvent) error { return app.ClearBootstrap() })
	}}
}

// BenchmarkHTTPRecordCRUD reproduces the qualification workload in process:
// every iteration creates a record with a JSON value, reads it, updates it and
// reads it again, verifying both reads. Requests are never retried. Each
// iteration is four HTTP requests; requests/s and latency percentiles are
// reported per request, together with connection-pool churn.
//
//	go test ./internal/runtime -run '^$' -bench BenchmarkHTTPRecordCRUD -benchtime 2000x
//
// BIRALOBASE_BENCH_TARGETS selects a comma-separated subset of targets.
func BenchmarkHTTPRecordCRUD(b *testing.B) {
	targets := []struct {
		name string
		open func(*testing.B) benchmarkTarget
	}{
		{"Biralobase", openBenchmarkRuntime},
		{"SQLite_FULL", func(b *testing.B) benchmarkTarget { return openBenchmarkReference(b, "FULL") }},
		{"SQLite_NORMAL", func(b *testing.B) benchmarkTarget { return openBenchmarkReference(b, "NORMAL") }},
	}
	selected := os.Getenv("BIRALOBASE_BENCH_TARGETS")
	for _, target := range targets {
		if selected != "" && !strings.Contains(","+selected+",", ","+target.name+",") {
			continue
		}
		for _, clients := range []int{1, 8, 16} {
			b.Run(fmt.Sprintf("%s/clients=%d", target.name, clients), func(b *testing.B) {
				t := target.open(b)
				defer t.close()
				runRecordCRUD(b, t, clients)
			})
		}
	}
}

func runRecordCRUD(b *testing.B, t benchmarkTarget, clients int) {
	const path = "/api/collections/bb_review_items/records"
	var failures atomic.Int64
	call := func(latencies *[]time.Duration, method, url string, body any, expected int) map[string]any {
		var reader *bytes.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			reader = bytes.NewReader(raw)
		} else {
			reader = bytes.NewReader(nil)
		}
		request := httptest.NewRequest(method, "http://localhost"+url, reader)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", t.token)
		recorder := httptest.NewRecorder()
		start := time.Now()
		t.handler.ServeHTTP(recorder, request)
		*latencies = append(*latencies, time.Since(start))
		if recorder.Code != expected {
			if failures.Add(1) == 1 {
				b.Errorf("%s %s: HTTP %d: %s", method, url, recorder.Code, recorder.Body.String())
			}
			return nil
		}
		var result map[string]any
		if json.Unmarshal(recorder.Body.Bytes(), &result) != nil {
			failures.Add(1)
			return nil
		}
		return result
	}
	before := map[string]sql.DBStats{}
	for name, pool := range t.pools {
		before[name] = pool.Stats()
	}
	var next atomic.Int64
	all := make([][]time.Duration, clients)
	var group sync.WaitGroup
	b.ResetTimer()
	start := time.Now()
	for worker := range clients {
		group.Add(1)
		go func() {
			defer group.Done()
			latencies := make([]time.Duration, 0, 4*b.N/clients+4)
			for {
				i := next.Add(1)
				if i > int64(b.N) {
					break
				}
				nested := []any{float64(worker), float64(i), map[string]any{"value": "synthetic"}}
				record := call(&latencies, "POST", path, map[string]any{"key": "load-" + strconv.FormatInt(i, 10), "payload": map[string]any{"nested": nested}, "value": i}, 200)
				if record == nil {
					continue
				}
				url := path + "/" + record["id"].(string)
				if read := call(&latencies, "GET", url, nil, 200); read != nil {
					got, _ := json.Marshal(read["payload"])
					want, _ := json.Marshal(map[string]any{"nested": nested})
					if !bytes.Equal(got, want) {
						failures.Add(1)
					}
				}
				if call(&latencies, "PATCH", url, map[string]any{"value": i + 1}, 200) != nil {
					if read := call(&latencies, "GET", url, nil, 200); read != nil && read["value"] != float64(i+1) {
						failures.Add(1)
					}
				}
			}
			all[worker] = latencies
		}()
	}
	group.Wait()
	elapsed := time.Since(start)
	b.StopTimer()
	var latencies []time.Duration
	for _, part := range all {
		latencies = append(latencies, part...)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if failed := failures.Load(); failed != 0 {
		b.Errorf("%d failed or incorrect requests", failed)
	}
	if len(latencies) == 0 {
		return
	}
	percentile := func(p float64) float64 {
		return float64(latencies[min(len(latencies)-1, int(float64(len(latencies))*p))]) / float64(time.Millisecond)
	}
	b.ReportMetric(float64(len(latencies))/elapsed.Seconds(), "requests/s")
	b.ReportMetric(percentile(.50), "p50-ms")
	b.ReportMetric(percentile(.95), "p95-ms")
	b.ReportMetric(percentile(.99), "p99-ms")
	// Connections opened beyond the idle limit are closed when returned and
	// reopened by the next request; a nonzero rate means the pool is churning.
	var closed, waits int64
	for name, pool := range t.pools {
		after := pool.Stats()
		closed += after.MaxIdleClosed - before[name].MaxIdleClosed
		waits += after.WaitCount - before[name].WaitCount
	}
	b.ReportMetric(float64(closed)/float64(len(latencies)), "idle-closed/request")
	b.ReportMetric(float64(waits)/float64(len(latencies)), "pool-waits/request")
}
