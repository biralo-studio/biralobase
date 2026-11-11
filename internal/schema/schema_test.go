package schema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/storage/turso"
)

func newSchemaProject(t *testing.T) (string, Schema) {
	t.Helper()
	root := t.TempDir()
	s, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	s.Collections = append(s.Collections, map[string]any{"id": "notescollection", "name": "notes", "type": "base", "fields": []any{map[string]any{"id": "titlefield", "name": "title", "type": "text"}}, "listRule": "@request.auth.id != ''", "viewRule": "@request.auth.id != ''"})
	if err = Write(filepath.Join(root, FileName), s); err != nil {
		t.Fatal(err)
	}
	if _, err = Generate(root, false); err != nil {
		t.Fatal(err)
	}
	s, err = Read(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	return root, s
}

func TestGeneratedMigrationsPreserveDataAndRejectDrift(t *testing.T) {
	for _, engine := range []string{"sqlite", "turso"} {
		t.Run(engine, func(t *testing.T) {
			root, s := newSchemaProject(t)
			config := core.BaseAppConfig{DataDir: t.TempDir()}
			if engine == "turso" {
				config.DBConnect = new(turso.Engine).Connect
			}
			app := core.NewBaseApp(config)
			if err := app.Bootstrap(); err != nil {
				t.Fatal(err)
			}
			defer app.ClearBootstrap()
			ms, err := ReadMigrations(filepath.Join(root, "migrations"))
			if err != nil {
				t.Fatal(err)
			}
			if err = Apply(app, s, ms); err != nil {
				t.Fatal(err)
			}
			c, err := app.FindCollectionByNameOrId("notes")
			if err != nil {
				t.Fatal(err)
			}
			r := core.NewRecord(c)
			r.Set("title", "retained content")
			if err = app.Save(r); err != nil {
				t.Fatal(err)
			}
			for _, c := range s.Collections {
				if c["name"] == "notes" {
					for _, v := range c["fields"].([]any) {
						f := v.(map[string]any)
						if f["id"] == "titlefield" {
							f["name"] = "headline"
						}
					}
				}
			}
			if err = Write(filepath.Join(root, FileName), s); err != nil {
				t.Fatal(err)
			}
			if _, err = Generate(root, false); err != nil {
				t.Fatal(err)
			}
			s, _ = Read(filepath.Join(root, FileName))
			ms, _ = ReadMigrations(filepath.Join(root, "migrations"))
			if err = Apply(app, s, ms); err != nil {
				t.Fatal(err)
			}
			if err = Apply(app, s, ms); err != nil {
				t.Fatalf("restart: %v", err)
			}
			r, err = app.FindRecordById("notes", r.Id)
			if err != nil || r.GetString("headline") != "retained content" {
				t.Fatal("rename lost data")
			}
			c, _ = app.FindCollectionByNameOrId("notes")
			c.ListRule = nil
			if err = app.Save(c); err != nil {
				t.Fatal(err)
			}
			if err = Apply(app, s, ms); err == nil || !strings.Contains(err.Error(), "drift") {
				t.Fatalf("drift accepted: %v", err)
			}
		})
	}
}

func TestMigrationHistoryAndDestructiveGuard(t *testing.T) {
	root, s := newSchemaProject(t)
	if err := Check(root); err != nil {
		t.Fatal(err)
	}
	s.Collections = s.Collections[:1]
	if err := Write(filepath.Join(root, FileName), s); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, false); err == nil {
		t.Fatal("destructive change accepted without explicit intent")
	}
	if _, err := Generate(root, true); err != nil {
		t.Fatal(err)
	}
	if err := Check(root); err != nil {
		t.Fatal(err)
	}
	ms, _ := ReadMigrations(filepath.Join(root, "migrations"))
	ms[0].From = "tampered"
	if err := Write(filepath.Join(root, "migrations", "000001_schema.json"), ms[0]); err != nil {
		t.Fatal(err)
	}
	if err := Check(root); err == nil {
		t.Fatal("broken history accepted")
	}
}

func TestFailedMigrationRollsBackDDLDataAndHistory(t *testing.T) {
	for _, engine := range []string{"sqlite", "turso"} {
		t.Run(engine, func(t *testing.T) {
			root, previous := newSchemaProject(t)
			config := core.BaseAppConfig{DataDir: t.TempDir()}
			if engine == "turso" {
				config.DBConnect = new(turso.Engine).Connect
			}
			app := core.NewBaseApp(config)
			if err := app.Bootstrap(); err != nil {
				t.Fatal(err)
			}
			defer app.ClearBootstrap()
			initial, err := ReadMigrations(filepath.Join(root, "migrations"))
			if err != nil {
				t.Fatal(err)
			}
			if err = Apply(app, previous, initial); err != nil {
				t.Fatal(err)
			}
			c, err := app.FindCollectionByNameOrId("notes")
			if err != nil {
				t.Fatal(err)
			}
			r := core.NewRecord(c)
			r.Set("title", "original")
			if err = app.Save(r); err != nil {
				t.Fatal(err)
			}
			// Read an independent copy so the pre-migration state is unchanged.
			next, err := Read(filepath.Join(root, FileName))
			if err != nil {
				t.Fatal(err)
			}
			for _, collection := range next.Collections {
				if collection["name"] == "notes" {
					for _, field := range collection["fields"].([]any) {
						f := field.(map[string]any)
						if f["id"] == "titlefield" {
							f["name"] = "headline"
						}
					}
				}
			}
			if err = Write(filepath.Join(root, FileName), next); err != nil {
				t.Fatal(err)
			}
			if _, err = Generate(root, false); err != nil {
				t.Fatal(err)
			}
			next, _ = Read(filepath.Join(root, FileName))
			ms, err := ReadMigrations(filepath.Join(root, "migrations"))
			if err != nil {
				t.Fatal(err)
			}
			ms[1].Before = []string{"UPDATE notes SET title='must roll back'"}
			ms[1].After = []string{"INSERT INTO nonexistent_table VALUES ('force failure after DDL')"}
			if err = Apply(app, next, ms); err == nil {
				t.Fatal("injected failure was accepted")
			}
			if err = Apply(app, previous, initial); err != nil {
				t.Fatalf("history or physical schema changed after rollback: %v", err)
			}
			stored, err := app.FindRecordById("notes", r.Id)
			if err != nil || stored.GetString("title") != "original" {
				t.Fatal("failed migration changed data", err)
			}
			c, err = app.FindCollectionByNameOrId("notes")
			if err != nil || c.Fields.GetByName("headline") != nil {
				t.Fatal("failed migration changed cached schema", err)
			}
			ms[1].Before = nil
			ms[1].After = []string{"UPDATE notes SET headline=upper(headline)"}
			if err = Apply(app, next, ms); err != nil {
				t.Fatalf("corrected release could not recover: %v", err)
			}
			stored, err = app.FindRecordById("notes", r.Id)
			if err != nil || stored.GetString("headline") != "ORIGINAL" {
				t.Fatal("recovered migration lost data", err)
			}
		})
	}
}

func TestSchemaExportDoesNotContainAuthSecrets(t *testing.T) {
	s, err := Default()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	if strings.Contains(string(b), `"secret"`) || strings.Contains(string(b), `"clientSecret"`) {
		t.Fatal("schema contains secrets")
	}
}

func TestNarrowingMultiValueFieldRequiresDestructiveAcknowledgement(t *testing.T) {
	root, s := newSchemaProject(t)
	for _, c := range s.Collections {
		if c["name"] == "notes" {
			c["fields"] = append(c["fields"].([]any), map[string]any{"id": "tagsfield", "name": "tags", "type": "select", "values": []any{"a", "b", "c"}, "maxSelect": 3})
		}
	}
	if err := Write(filepath.Join(root, FileName), s); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(root, false); err != nil {
		t.Fatal(err)
	}
	s, err := Read(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.Collections {
		if c["name"] == "notes" {
			for _, v := range c["fields"].([]any) {
				f := v.(map[string]any)
				if f["id"] == "tagsfield" {
					f["maxSelect"] = 1
				}
			}
		}
	}
	if err = Write(filepath.Join(root, FileName), s); err != nil {
		t.Fatal(err)
	}
	if _, err = Generate(root, false); err == nil {
		t.Fatal("multi-value to single-value conversion accepted without destructive acknowledgement")
	}
	if _, err = Generate(root, true); err != nil {
		t.Fatal(err)
	}
}

func TestProductionSchemaGuard(t *testing.T) {
	app, cleanup, err := Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	LockProductionSchema(app)
	if app.Save(core.NewBaseCollection("forbidden")) == nil {
		t.Fatal("runtime collection mutation accepted")
	}
}

func TestMCPAuthCollectionIsValidatedBeforeDeployment(t *testing.T) {
	_, s := newSchemaProject(t)
	s.MCP = map[string]Exposure{"notescollection": {Name: "notes", Description: "Notes", Actions: []string{"list"}, Fields: []string{"id", "title"}}}
	for _, name := range []string{"missing", "notes", "_superusers"} {
		s.AuthCollection = name
		if err := s.Validate(); err == nil {
			t.Fatalf("accepted invalid MCP auth collection %s", name)
		}
	}
	s.AuthCollection = "users"
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDashboardExportGenerationAndPhysicalDrift(t *testing.T) {
	root, s := newSchemaProject(t)
	app, cleanup, err := Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	ms, _ := ReadMigrations(filepath.Join(root, "migrations"))
	if err = Apply(app, s, ms); err != nil {
		t.Fatal(err)
	}
	c, _ := app.FindCollectionByNameOrId("notes")
	c.Fields.Add(&core.TextField{Id: "newfield", Name: "subtitle"})
	if err = app.Save(c); err != nil {
		t.Fatal(err)
	}
	s.Collections, err = Snapshot(app)
	if err != nil {
		t.Fatal(err)
	}
	if err = Write(filepath.Join(root, FileName), s); err != nil {
		t.Fatal(err)
	}
	if _, err = Generate(root, false); err != nil {
		t.Fatal(err)
	}
	s, _ = Read(filepath.Join(root, FileName))
	ms, _ = ReadMigrations(filepath.Join(root, "migrations"))
	if err = Apply(app, s, ms); err == nil {
		t.Fatal("production adopted drift")
	}
	if err = ReconcileDevelopment(app, s, ms); err != nil {
		t.Fatal(err)
	}
	if err = Apply(app, s, ms); err != nil {
		t.Fatal(err)
	}
	if _, err = app.DB().NewQuery("ALTER TABLE notes ADD COLUMN undocumented TEXT").Execute(); err != nil {
		t.Fatal(err)
	}
	if err = Apply(app, s, ms); err == nil || !strings.Contains(err.Error(), "physical") {
		t.Fatalf("physical drift: %v", err)
	}
}

func FuzzDecodeSchema(f *testing.F) {
	f.Add([]byte(`{"version":1,"collections":[]}`))
	f.Add([]byte(`{"version":1,"collections":null} {}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		var s Schema
		if Decode(data, &s) == nil {
			_ = s.Validate()
		}
	})
}

func TestWriteNoTemporaryFiles(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "file.json"), map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("temporary file leaked")
	}
}
