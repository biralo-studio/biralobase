package turso_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/biralo-studio/biralobase/core"
	_ "github.com/biralo-studio/biralobase/migrations"
	"github.com/biralo-studio/biralobase/storage/turso"
)

func TestSettingsRemainConsistentAfterBackup(t *testing.T) {
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir(), DBConnect: new(turso.Engine).Connect})
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app.ClearBootstrap()
	if err := app.CreateBackup(context.Background(), "settings.zip"); err != nil {
		t.Fatal(err)
	}
	app.Settings().SuperuserIPs = []string{"0.0.0.0"}
	if err := app.Save(app.Settings()); err != nil {
		t.Fatal(err)
	}
	if len(app.Settings().SuperuserIPs) != 1 {
		t.Error("settings lost immediately after save")
	}
	var value string
	if err := app.DB().NewQuery("SELECT value FROM _params WHERE id='settings'").Row(&value); err != nil {
		t.Fatal(err)
	}
	var settings struct {
		SuperuserIPs []string `json:"superuserIPs"`
	}
	if err := json.Unmarshal([]byte(value), &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings.SuperuserIPs) != 1 {
		t.Error("settings lost in stored row")
	}
}

func TestConcurrentWriters(t *testing.T) {
	e := new(turso.Engine)
	db, err := e.Connect(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.DB().SetMaxOpenConns(4)
	if _, err := db.NewQuery("CREATE TABLE test (id INTEGER PRIMARY KEY, value TEXT)").Execute(); err != nil {
		t.Fatal(err)
	}
	e.SetConcurrent(true)
	ctx := context.Background()
	a, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback()
	b, err := db.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Rollback()
	if _, err = a.Exec("INSERT INTO test VALUES (1, 'first')"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.Exec("INSERT INTO test VALUES (2, 'second')"); err != nil {
		t.Fatal(err)
	}
	if err = a.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = b.Commit(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.DB().QueryRow("SELECT count(*) FROM test").Scan(&count); err != nil || count != 2 {
		t.Fatalf("count %d: %v", count, err)
	}
}

func TestPocketBaseBootstrap(t *testing.T) {
	e := new(turso.Engine)
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: t.TempDir(), DBConnect: e.Connect})
	t.Cleanup(func() { app.ClearBootstrap() })
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindCollectionByNameOrId("_superusers"); err != nil {
		t.Fatal(err)
	}
	collection := core.NewBaseCollection("notes")
	collection.Fields.Add(&core.TextField{Name: "title", Required: true})
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	e.SetConcurrent(true)
	record := core.NewRecord(collection)
	record.Set("title", "Turso-backed PocketBase")
	if err := app.RunInTransaction(func(tx core.App) error { return tx.Save(record) }); err != nil {
		t.Fatal(err)
	}
	if _, err := app.FindRecordById("notes", record.Id); err != nil {
		t.Fatal(err)
	}
	if err := app.CreateBackup(context.Background(), "storage-proof.zip"); err != nil {
		t.Fatalf("consistent backup: %v", err)
	}
}

func TestPreparedWritesAndConflictingUpdates(t *testing.T) {
	e := new(turso.Engine)
	db, err := e.Connect(filepath.Join(t.TempDir(), "conflicts.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.DB().Exec("CREATE TABLE values_test(id INTEGER PRIMARY KEY,value INTEGER)"); err != nil {
		t.Fatal(err)
	}
	e.SetConcurrent(true)
	stmt, err := db.DB().Prepare("INSERT\nINTO values_test VALUES (?,?)")
	if err != nil {
		t.Fatal(err)
	}
	defer stmt.Close()
	if _, err = stmt.Exec(1, 0); err != nil {
		t.Fatal(err)
	}
	a, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer a.Rollback()
	b, err := db.DB().Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Rollback()
	var snapshot int
	if err = a.QueryRow("SELECT value FROM values_test WHERE id=1").Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if err = b.QueryRow("SELECT value FROM values_test WHERE id=1").Scan(&snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Exec("UPDATE values_test SET value=1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	_, secondErr := b.Exec("UPDATE values_test SET value=2 WHERE id=1")
	if err = a.Commit(); err != nil {
		t.Fatal(err)
	}
	if secondErr == nil {
		secondErr = b.Commit()
	} else {
		_ = b.Rollback()
	}
	if secondErr == nil {
		t.Fatal("both conflicting updates were accepted")
	}
	var value int
	if err = db.DB().QueryRow("SELECT value FROM values_test WHERE id=1").Scan(&value); err != nil || value != 1 {
		t.Fatalf("lost committed update: %d %v", value, err)
	}
	if _, err = stmt.Exec(2, 3); err != nil {
		t.Fatal("poisoned pool after conflict", err)
	}
}
