package runtime_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/bundle"
	"github.com/biralo-studio/biralobase/internal/provision"
	server "github.com/biralo-studio/biralobase/internal/runtime"
	"github.com/biralo-studio/biralobase/internal/schema"
)

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
