package runtime_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/bundle"
	"github.com/biralo-studio/biralobase/internal/provision"
	server "github.com/biralo-studio/biralobase/internal/runtime"
	"github.com/biralo-studio/biralobase/internal/schema"
)

func TestBackupRestoreKeepsTheDataDirectoryLocked(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "app")
	s, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err = schema.Write(filepath.Join(source, schema.FileName), s); err != nil {
		t.Fatal(err)
	}
	c := provision.Config{AppDir: source, DataDir: filepath.Join(root, "data"), HTTP: "127.0.0.1:0", Dev: true}
	srv, err := server.Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err = srv.App.CreateBackup(context.Background(), "restore-lock.zip"); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(c.DataDir, ".biralobase.lock")
	original, err := os.Stat(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	interrupted := errors.New("interrupt test process restart")
	checked := false
	srv.App.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		if !e.IsRestart {
			return e.Next()
		}
		checked = true
		current, statErr := os.Stat(lockPath)
		if statErr != nil || !os.SameFile(original, current) {
			t.Error("restore replaced or removed the held lock inode")
		}
		f, openErr := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
		if openErr != nil {
			t.Error(openErr)
		} else {
			defer f.Close()
			if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
				syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				t.Error("second process can acquire data-directory ownership during restore")
			}
		}
		return interrupted
	})
	if err = srv.App.RestoreBackup(context.Background(), "restore-lock.zip"); !errors.Is(err, interrupted) {
		t.Fatalf("restore failed before restart probe: %v", err)
	}
	if !checked {
		t.Fatal("restore did not reach restart probe")
	}
}

func TestServeShutdownRunsAsyncTerminateHook(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "app")
	s, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err = schema.Write(filepath.Join(source, schema.FileName), s); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(source, "functions/dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"type":"module"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(source, "functions/dist/index.js"), []byte(`import {writeFile} from 'node:fs/promises'; export default {hooks:[{event:'OnTerminate',handler:async e=>{await writeFile(process.env.BIRALOBASE_FUNCTION_SHUTDOWN_MARKER,'completed');await e.next();}}]};`), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "shutdown")
	t.Setenv("BIRALOBASE_FUNCTION_SHUTDOWN_MARKER", marker)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv, err := server.Open(ctx, provision.Config{AppDir: source, DataDir: filepath.Join(root, "data"), HTTP: "127.0.0.1:0", Dev: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ready := make(chan struct{})
	srv.App.OnServe().BindFunc(func(e *core.ServeEvent) error { e.Listener = listener; err := e.Next(); close(ready); return err })
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("graceful shutdown timed out")
	}
	content, err := os.ReadFile(marker)
	if err != nil || string(content) != "completed" {
		t.Fatal("terminate hook was skipped", err)
	}
}

func TestImmutableStartupMigrationFunctionsProvisioningAndRestart(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	release := filepath.Join(root, "release")
	s, err := schema.Default()
	if err != nil {
		t.Fatal(err)
	}
	s.Collections = append(s.Collections, map[string]any{"id": "notescollection", "name": "notes", "type": "base", "fields": []any{map[string]any{"id": "titlefield", "name": "title", "type": "text"}}, "listRule": "@request.auth.id != ''"})
	if err = schema.Write(filepath.Join(source, schema.FileName), s); err != nil {
		t.Fatal(err)
	}
	if _, err = schema.Generate(source, false); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(source, "functions/dist"), 0755)
	os.WriteFile(filepath.Join(source, "package.json"), []byte(`{"type":"module"}`), 0644)
	os.WriteFile(filepath.Join(source, "functions/dist/index.js"), []byte(`export default {functions:{hello:async ctx=>({body:{authenticated:ctx.auth!==null}})}}`), 0644)
	if _, err = bundle.Build(source, release, "test-revision", server.Version, bundle.Platform()); err != nil {
		t.Fatal(err)
	}
	c := provision.Config{AppDir: release, DataDir: filepath.Join(root, "data"), HTTP: "127.0.0.1:8090", PublicURL: "https://api.example.com", AdminEmail: "admin@example.com", AdminPassword: "test-secret-password", EncryptionKey: strings.Repeat("k", 32)}
	runtime, err := server.Open(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = server.Open(context.Background(), c); err == nil {
		t.Fatal("second process opened the same data directory")
	}
	admin, err := runtime.App.FindAuthRecordByEmail("_superusers", c.AdminEmail)
	if err != nil {
		t.Fatal(err)
	}
	id := admin.Id
	if err = runtime.App.Save(core.NewBaseCollection("drift")); err == nil {
		t.Fatal("production collection mutation allowed")
	}
	routes, err := apis.NewRouter(runtime.App)
	if err != nil {
		t.Fatal(err)
	}
	runtime.Bind(routes)
	handler, err := routes.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("GET", "http://localhost/api/functions/hello", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"authenticated":false`) {
		t.Fatal("HTTP function failed", w.Code, w.Body.String())
	}
	if err = runtime.Close(); err != nil {
		t.Fatal(err)
	}
	c.AdminPassword = "rotated-test-password"
	runtime, err = server.Open(context.Background(), c)
	if err != nil {
		t.Fatal("restart", err)
	}
	defer runtime.Close()
	admin, err = runtime.App.FindAuthRecordByEmail("_superusers", c.AdminEmail)
	if err != nil || admin.Id != id || !admin.ValidatePassword(c.AdminPassword) {
		t.Fatal("managed identity/rotation failed", err)
	}
	var stored string
	if err = runtime.App.DB().NewQuery("SELECT value FROM _params WHERE id='settings'").Row(&stored); err != nil {
		t.Fatal(err)
	}
	if json.Valid([]byte(stored)) {
		t.Fatal("production settings are not encrypted")
	}
}
