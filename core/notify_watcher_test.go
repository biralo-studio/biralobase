package core_test

import (
	"context"
	"database/sql"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/tools/store"
	"github.com/pocketbase/dbx"
	"golang.org/x/sync/semaphore"
)

func TestNotifyWatcher_SettingsUpdate(t *testing.T) {
	t.Parallel()

	testEvents := store.New[core.App, int](nil)

	tmpDir, err := os.MkdirTemp("", "pb_notify_test*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	app1 := core.NewBaseApp(core.BaseAppConfig{
		DataDir: tmpDir,
	})
	if err := app1.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app1.ClearBootstrap()

	app2 := core.NewBaseApp(core.BaseAppConfig{
		DataDir: tmpDir,
	})
	if err := app2.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app2.ClearBootstrap()

	timeout := time.After(3 * time.Second)
	done := make(chan struct{}, 1)

	app1.OnSettingsReload().BindFunc(func(e *core.SettingsReloadEvent) error {
		testEvents.SetFunc(app1, func(old int) int {
			return old + 1
		})
		return e.Next()
	})

	app2.OnSettingsReload().BindFunc(func(e *core.SettingsReloadEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		testEvents.SetFunc(app2, func(old int) int {
			defer func() {
				done <- struct{}{}
			}()

			return old + 1
		})
		return nil
	})

	// updating app1 settings should trigger a reload in app2
	app1.Settings().SuperuserIPs = []string{"127.0.0.1"}
	if err := app1.Save(app1.Settings()); err != nil {
		t.Fatal(err)
	}

	// wait for the event
	select {
	case <-timeout:
		t.Fatal("app2 reload event timeout")
	case <-done:
		// ready
	}

	if app1Total := testEvents.Get(app1); app1Total != 1 {
		t.Fatalf("Expected 1 app1 event, got %d", app1Total)
	}

	if app2Total := testEvents.Get(app2); app2Total != 1 {
		t.Fatalf("Expected 1 app2 event, got %d", app2Total)
	}

	app2SuperuserIPs := app2.Settings().SuperuserIPs
	if len(app2SuperuserIPs) != 1 || app2SuperuserIPs[0] != "127.0.0.1" {
		t.Fatalf("Expected exactly 127.0.0.1 superuser IP in app2 settings event, got %v", app2SuperuserIPs)
	}
}

func TestNotifyWatcher_CollectionsUpdate(t *testing.T) {
	t.Parallel()

	tmpDir, err := os.MkdirTemp("", "pb_notify_test*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	app1 := core.NewBaseApp(core.BaseAppConfig{
		DataDir: tmpDir,
	})
	if err := app1.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app1.ClearBootstrap()

	app2 := core.NewBaseApp(core.BaseAppConfig{
		DataDir: tmpDir,
	})
	if err := app2.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app2.ClearBootstrap()

	testQueries := store.New[string, []string](nil)
	app2.ConcurrentDB().(*dbx.DB).QueryLogFunc = func(ctx context.Context, t time.Duration, sql string, rows *sql.Rows, err error) {
		testQueries.SetFunc("concurrent", func(old []string) []string {
			return append(old, sql)
		})
	}
	app2.ConcurrentDB().(*dbx.DB).ExecLogFunc = func(ctx context.Context, t time.Duration, sql string, result sql.Result, err error) {
		testQueries.SetFunc("concurrent", func(old []string) []string {
			return append(old, sql)
		})
	}
	app2.NonconcurrentDB().(*dbx.DB).QueryLogFunc = func(ctx context.Context, t time.Duration, sql string, rows *sql.Rows, err error) {
		testQueries.SetFunc("nonconcurrent", func(old []string) []string {
			return append(old, sql)
		})
	}
	app2.NonconcurrentDB().(*dbx.DB).ExecLogFunc = func(ctx context.Context, t time.Duration, sql string, result sql.Result, err error) {
		testQueries.SetFunc("nonconcurrent", func(old []string) []string {
			return append(old, sql)
		})
	}

	ctx, cancelCtx := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancelCtx()

	sem := semaphore.NewWeighted(1)
	sem.Acquire(ctx, 1)

	// currently there is no hook for the collections cache reload so we pool instead
	done := make(chan bool, 1)
	ticker := time.NewTicker(100 * time.Millisecond)
	go func() {
		for {
			select {
			case <-ticker.C:
				if len(testQueries.Get("concurrent")) > 0 {
					sem.Release(1)
					return
				}
			case <-done:
				return
			}
		}
	}()

	// create/update/delete app1 collections should trigger a reload in app2
	dummyCollection := core.NewBaseCollection("test")
	if err := app1.Save(dummyCollection); err != nil {
		t.Fatal(err)
	}
	dummyCollection.Fields.Add(&core.TextField{Name: "test"})
	if err := app1.Save(dummyCollection); err != nil {
		t.Fatal(err)
	}
	if err := app1.Delete(dummyCollection); err != nil {
		t.Fatal(err)
	}

	// block until released or timeouted
	sem.Acquire(ctx, 1)
	ticker.Stop()
	done <- true

	nonconcurrentQueries := testQueries.Get("nonconcurrent")
	concurrentQueries := testQueries.Get("concurrent")

	if len(nonconcurrentQueries) != 0 {
		t.Fatalf("Expected 0 concurrent queries, got %d (%v)", len(nonconcurrentQueries), nonconcurrentQueries)
	}
	// These mutations can span several debounce windows under race detection
	// or on slow storage. Each reload must use the same read query; requiring
	// exactly one makes the test depend on all writes finishing within 50 ms.
	if len(concurrentQueries) == 0 {
		t.Fatal("Expected a concurrent reload query")
	}

	expectedQuery := "SELECT {{_collections}}.* FROM `_collections` ORDER BY `rowid` ASC"
	for _, query := range concurrentQueries {
		if query != expectedQuery {
			t.Fatalf("Expected query\n%s\ngot\n%s", expectedQuery, query)
		}
	}
}

// A settings change and a collection change from another instance can land in
// the same debounce window. Both caches must be refreshed; remembering only
// the most recent signal silently leaves the other one stale.
func TestNotifyWatcher_SettingsAndCollectionsTogether(t *testing.T) {
	t.Parallel()

	tmpDir, err := os.MkdirTemp("", "pb_notify_test*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	app1 := core.NewBaseApp(core.BaseAppConfig{DataDir: tmpDir})
	if err := app1.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app1.ClearBootstrap()

	app2 := core.NewBaseApp(core.BaseAppConfig{DataDir: tmpDir})
	if err := app2.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app2.ClearBootstrap()

	settingsReloaded := make(chan struct{}, 8)
	app2.OnSettingsReload().BindFunc(func(e *core.SettingsReloadEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		settingsReloaded <- struct{}{}
		return nil
	})

	// The settings signal follows the collection signal within the window.
	if err := app1.Save(core.NewBaseCollection("together")); err != nil {
		t.Fatal(err)
	}
	app1.Settings().SuperuserIPs = []string{"127.0.0.1"}
	if err := app1.Save(app1.Settings()); err != nil {
		t.Fatal(err)
	}

	select {
	case <-settingsReloaded:
	case <-time.After(3 * time.Second):
		t.Fatal("app2 did not reload settings after both changed together")
	}
	// The collection cache is refreshed after the settings in the same pass.
	for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := app2.FindCachedCollectionByNameOrId("together"); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("app2 did not reload cached collections after both changed together")
		}
	}
}

// Reload hooks run application code, which may restart the process. Restart
// runs the terminate hooks on the goroutine that is delivering the
// notification; closing the watcher there must not wait for that goroutine.
// Otherwise the restart never happens and a later shutdown hangs as well.
func TestNotifyWatcher_ReloadHookMayTerminate(t *testing.T) {
	t.Parallel()

	tmpDir, err := os.MkdirTemp("", "pb_notify_test*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	app1 := core.NewBaseApp(core.BaseAppConfig{DataDir: tmpDir})
	if err := app1.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app1.ClearBootstrap()

	app2 := core.NewBaseApp(core.BaseAppConfig{DataDir: tmpDir})
	if err := app2.Bootstrap(); err != nil {
		t.Fatal(err)
	}

	terminated := make(chan error, 1)
	var once sync.Once
	app2.OnSettingsReload().BindFunc(func(e *core.SettingsReloadEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		once.Do(func() {
			// The hooks that Restart triggers, without replacing the test process.
			terminated <- app2.OnTerminate().Trigger(&core.TerminateEvent{App: app2, IsRestart: true})
		})
		return nil
	})

	app1.Settings().SuperuserIPs = []string{"127.0.0.1"}
	if err := app1.Save(app1.Settings()); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-terminated:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		// app2 is deliberately not cleaned up: its watcher is waiting for itself.
		t.Fatal("terminating from a reload hook deadlocked the notification watcher")
	}
	if err := app2.ClearBootstrap(); err != nil {
		t.Fatal(err)
	}
}
