package schema

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/tools/types"
	"github.com/pocketbase/dbx"
)

type Migration struct {
	Version     int      `json:"version"`
	Sequence    int      `json:"sequence"`
	From        string   `json:"from"`
	To          Schema   `json:"to"`
	Destructive bool     `json:"destructive"`
	Before      []string `json:"before,omitempty"`
	After       []string `json:"after,omitempty"`
}
type Applied struct {
	Sequence int    `json:"sequence"`
	Checksum string `json:"checksum"`
}
type State struct {
	Schema          Schema            `json:"schema"`
	History         []Applied         `json:"history"`
	Physical        map[string]string `json:"physical,omitempty"`
	PhysicalVersion int               `json:"physicalVersion,omitempty"`
}

const stateKey = "biralobase_schema_state"

func ReadMigrations(dir string) ([]Migration, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	result := make([]Migration, 0, len(files))
	var previous string
	for i, path := range files {
		var m Migration
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if len(b) > 16<<20 {
			return nil, errors.New("migration exceeds 16 MiB")
		}
		if err = Decode(b, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if m.Version != 1 || m.Sequence != i+1 {
			return nil, errors.New("migration sequence must start at 1 and be contiguous")
		}
		if err = m.To.Validate(); err != nil {
			return nil, err
		}
		if i > 0 && m.From != previous {
			return nil, errors.New("broken migration hash chain")
		}
		previous = Hash(m.To)
		result = append(result, m)
	}
	return result, nil
}

func Generate(root string, allowDestructive bool) (string, error) {
	s, err := Read(filepath.Join(root, FileName))
	if err != nil {
		return "", err
	}
	s, err = Normalize(s)
	if err != nil {
		return "", err
	}
	migrations, err := ReadMigrations(filepath.Join(root, "migrations"))
	if err != nil {
		return "", err
	}
	previous, err := Default()
	if err != nil {
		return "", err
	}
	if len(migrations) > 0 {
		previous = migrations[len(migrations)-1].To
	}
	if Hash(previous) == Hash(s) {
		return "", Write(filepath.Join(root, FileName), s)
	}
	destructive := isDestructive(previous, s)
	if destructive && !allowDestructive {
		return "", errors.New("schema removes fields/collections, changes field types, or narrows selections/relations; review intent and rerun with --allow-destructive")
	}
	m := Migration{Version: 1, Sequence: len(migrations) + 1, From: Hash(previous), To: s, Destructive: destructive}
	app, cleanup, err := Sandbox()
	if err != nil {
		return "", err
	}
	defer cleanup()
	for _, prior := range migrations {
		if err = applyOne(app, prior); err != nil {
			return "", err
		}
	}
	if err = applyOne(app, m); err != nil {
		return "", err
	}
	path := filepath.Join(root, "migrations", fmt.Sprintf("%06d_schema.json", m.Sequence))
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("migration destination already exists")
	}
	if err = Write(path, m); err != nil {
		return "", err
	}
	return path, Write(filepath.Join(root, FileName), s)
}

func Check(root string) error {
	s, err := Read(filepath.Join(root, FileName))
	if err != nil {
		return err
	}
	migrations, err := ReadMigrations(filepath.Join(root, "migrations"))
	if err != nil {
		return err
	}
	app, cleanup, err := Sandbox()
	if err != nil {
		return err
	}
	defer cleanup()
	return Apply(app, s, migrations)
}

// Apply checks history and live schema before changing anything. Startup calls
// this before accepting traffic and after creating the deployment backup.
func Apply(app core.App, desired Schema, migrations []Migration) error {
	state, p, err := plan(app, desired, migrations, false)
	if err != nil {
		return err
	}
	return applyPending(app, state, p, migrations)
}

// Pending validates the complete release before creating a deployment backup.
func Pending(app core.App, desired Schema, migrations []Migration) (bool, error) {
	state, _, err := plan(app, desired, migrations, false)
	return len(state.History) < len(migrations), err
}

func plan(app core.App, desired Schema, migrations []Migration, development bool) (state State, p *core.Param, err error) {
	p = &core.Param{}
	err = app.ModelQuery(p).Model(stateKey, p)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return state, p, err
	}
	if p.Id == "" {
		state.Schema, err = Default()
		if err != nil {
			return state, p, err
		}
		state.History = []Applied{}
		p.Id = stateKey
	} else if err = json.Unmarshal(p.Value, &state); err != nil {
		return state, p, errors.New("invalid schema migration state")
	}
	if len(state.History) > len(migrations) {
		return state, p, errors.New("bundle is older than applied migration history")
	}
	for i, applied := range state.History {
		if applied.Sequence != i+1 || Hash(migrations[i]) != applied.Checksum {
			return state, p, fmt.Errorf("applied migration %d was changed", i+1)
		}
	}
	current, err := Snapshot(app)
	if err != nil {
		return state, p, err
	}
	if !development && Hash(current) != Hash(state.Schema.Collections) {
		return state, p, errors.New("database schema drift detected; export and reconcile before deployment")
	}
	if !development && state.Physical != nil {
		if state.PhysicalVersion < 0 || state.PhysicalVersion > 1 {
			return state, p, errors.New("unsupported physical schema fingerprint version")
		}
		physical, physicalErr := physicalSchemaVersion(app, state.Schema, state.PhysicalVersion)
		if physicalErr != nil {
			return state, p, physicalErr
		}
		if Hash(physical) != Hash(state.Physical) {
			return state, p, errors.New("physical database schema drift detected")
		}
	}
	expected := state.Schema
	for _, m := range migrations[len(state.History):] {
		if m.From != Hash(expected) {
			return state, p, errors.New("migration does not extend current schema")
		}
		expected = m.To
	}
	if Hash(expected) != Hash(desired) {
		return state, p, errors.New("schema changes have no matching generated migration")
	}
	return state, p, nil
}

// ReconcileDevelopment records dashboard edits that already exactly match the
// generated release. Production never calls this. Data SQL cannot be adopted:
// its effects cannot be inferred from a schema snapshot.
func ReconcileDevelopment(app core.App, desired Schema, migrations []Migration) error {
	state, p, err := plan(app, desired, migrations, true)
	if err != nil {
		return err
	}
	current, err := Snapshot(app)
	if err != nil {
		return err
	}
	if Hash(current) == Hash(state.Schema.Collections) {
		return nil
	}
	if Hash(current) != Hash(desired.Collections) {
		return errors.New("development schema drift: stop, schema export, then schema generate before restarting")
	}
	for _, m := range migrations[len(state.History):] {
		if len(m.Before) > 0 || len(m.After) > 0 {
			return errors.New("cannot adopt dashboard changes with pending data transformations; use a clean development database")
		}
		if isDestructive(state.Schema, m.To) && !m.Destructive {
			return errors.New("destructive migration is not explicitly marked")
		}
		state.History = append(state.History, Applied{Sequence: m.Sequence, Checksum: Hash(m)})
		state.Schema = m.To
	}
	state.Schema = desired
	state.Physical, err = physicalSchema(app, desired)
	state.PhysicalVersion = 1
	if err != nil {
		return err
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	p.Value = types.JSONRaw(b)
	return app.Save(p)
}

func applyPending(app core.App, state State, p *core.Param, migrations []Migration) error {
	// Upgrade legacy fingerprints only after plan verified their exact hashes.
	// A restored legacy backup with a mismatched hash still fails closed.
	if (p.IsNew() && len(migrations) == 0) || (!p.IsNew() && state.PhysicalVersion == 0) {
		var err error
		state.Physical, err = physicalSchema(app, state.Schema)
		state.PhysicalVersion = 1
		if err != nil {
			return err
		}
		b, err := json.Marshal(state)
		if err != nil {
			return err
		}
		p.Value = types.JSONRaw(b)
		if err = app.Save(p); err != nil {
			return err
		}
	}
	for _, m := range migrations[len(state.History):] {
		if isDestructive(state.Schema, m.To) && !m.Destructive {
			return errors.New("destructive migration is not explicitly marked")
		}
		err := app.RunInTransaction(func(tx core.App) error {
			if err := applyOne(tx, m); err != nil {
				return err
			}
			actual, err := Snapshot(tx)
			if err != nil {
				return err
			}
			if Hash(actual) != Hash(m.To.Collections) {
				return errors.New("migration result differs from committed schema")
			}
			next := State{Schema: m.To, PhysicalVersion: 1, History: append(append([]Applied{}, state.History...), Applied{Sequence: m.Sequence, Checksum: Hash(m)})}
			next.Physical, err = physicalSchema(tx, m.To)
			if err != nil {
				return err
			}
			b, err := json.Marshal(next)
			if err != nil {
				return err
			}
			p.Value = types.JSONRaw(b)
			if err = tx.Save(p); err != nil {
				return err
			}
			state = next
			return nil
		})
		if err != nil {
			_ = app.ReloadCachedCollections()
			return fmt.Errorf("migration %d: %w", m.Sequence, err)
		}
	}
	return app.ReloadCachedCollections()
}

func physicalSchema(app core.App, s Schema) (map[string]string, error) {
	return physicalSchemaVersion(app, s, 1)
}

func physicalSchemaVersion(app core.App, s Schema, version int) (map[string]string, error) {
	result := map[string]string{}
	for _, c := range s.Collections {
		var rows []struct {
			Name string
			Type string
			SQL  sql.NullString
		}
		err := app.DB().NewQuery("SELECT name,type,sql FROM sqlite_schema WHERE lower(tbl_name)=lower({:name}) AND type IN ('table','view','index') ORDER BY type,name").Bind(dbx.Params{"name": c["name"]}).All(&rows)
		if err != nil {
			return nil, err
		}
		if len(rows) == 0 {
			return nil, fmt.Errorf("physical database schema drift: missing %s", c["name"])
		}
		if version == 1 {
			for i := range rows {
				// Turso also folds catalog index names during VACUUM. SQLite
				// object names are case insensitive, unlike string literals.
				rows[i].Name = strings.ToLower(rows[i].Name)
				if rows[i].SQL.Valid {
					rows[i].SQL.String = Hash(sqlTokens(rows[i].SQL.String))
				}
			}
			sort.Slice(rows, func(i, j int) bool {
				if rows[i].Type != rows[j].Type {
					return rows[i].Type < rows[j].Type
				}
				return rows[i].Name < rows[j].Name
			})
		}
		result[c["id"].(string)] = Hash(rows)
	}
	return result, nil
}

func applyOne(app core.App, m Migration) error {
	return app.RunInTransaction(func(tx core.App) error {
		if err := runDataSQL(tx, m.Before); err != nil {
			return err
		}
		if err := Import(tx, m.To.Collections); err != nil {
			return err
		}
		return runDataSQL(tx, m.After)
	})
}

func runDataSQL(app core.App, statements []string) error {
	for _, query := range statements {
		words := strings.Fields(query)
		if len(words) == 0 || strings.Contains(query, ";") {
			return errors.New("data transformations require single statements without semicolons")
		}
		switch strings.ToUpper(words[0]) {
		case "UPDATE", "INSERT", "DELETE":
		default:
			return errors.New("data transformations accept only INSERT, UPDATE, and DELETE")
		}
		if _, err := app.DB().NewQuery(query).Execute(); err != nil {
			return err
		}
	}
	return nil
}

func isDestructive(before, after Schema) bool {
	newCollections := map[string]map[string]any{}
	for _, c := range after.Collections {
		newCollections[c["id"].(string)] = c
	}
	for _, old := range before.Collections {
		next := newCollections[old["id"].(string)]
		if next == nil || next["type"] != old["type"] {
			return true
		}
		fields := map[string]map[string]any{}
		if values, ok := next["fields"].([]any); ok {
			for _, v := range values {
				f := v.(map[string]any)
				fields[f["id"].(string)] = f
			}
		}
		if values, ok := old["fields"].([]any); ok {
			for _, v := range values {
				f := v.(map[string]any)
				nextField := fields[f["id"].(string)]
				if nextField == nil || nextField["type"] != f["type"] {
					return true
				}
				// PocketBase converts multi-valued select/relation/file columns
				// when maxSelect becomes 1. Field type alone cannot detect this
				// data-losing change. Also flag narrower limits and relation targets.
				switch f["type"] {
				case "select", "relation", "file":
					oldMax, newMax := selectionLimit(f), selectionLimit(nextField)
					if newMax < oldMax {
						return true
					}
				}
				if f["type"] == "relation" && f["collectionId"] != nextField["collectionId"] {
					return true
				}
			}
		}
	}
	return false
}

func selectionLimit(field map[string]any) int {
	// Normalized schema comes from JSON; accept int as well for Go callers.
	switch n := field["maxSelect"].(type) {
	case float64:
		return max(1, int(n))
	case int:
		return max(1, n)
	default:
		return 1
	}
}

func LockProductionSchema(app core.App) {
	deny := func(e *core.CollectionEvent) error {
		return errors.New("production schema is managed by Git migrations")
	}
	app.OnCollectionCreate().BindFunc(deny)
	app.OnCollectionUpdate().BindFunc(deny)
	app.OnCollectionDelete().BindFunc(deny)
}
