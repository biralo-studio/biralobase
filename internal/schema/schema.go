// Package schema implements deterministic, reviewable schema releases. Schema
// application uses PocketBase's collection services, never raw DDL generation.
package schema

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/biralo-studio/biralobase/core"
	_ "github.com/biralo-studio/biralobase/migrations"
)

const FileName = "biralobase.schema.json"

type Exposure struct {
	Name        string   `json:"name,omitempty"`
	Description string   `json:"description"`
	Actions     []string `json:"actions"`
	Fields      []string `json:"fields"`
}
type Schema struct {
	Version        int                 `json:"version"`
	Collections    []map[string]any    `json:"collections"`
	Settings       map[string]any      `json:"settings,omitempty"`
	MCP            map[string]Exposure `json:"mcp,omitempty"`
	AuthCollection string              `json:"authCollection,omitempty"`
}

func Decode(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return errors.New("expected one JSON document")
	}
	return nil
}

func Read(path string) (Schema, error) {
	var s Schema
	data, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	if len(data) > 8<<20 {
		return s, errors.New("schema exceeds 8 MiB")
	}
	if err := Decode(data, &s); err != nil {
		return s, err
	}
	return s, s.Validate()
}
func (s Schema) Validate() error {
	if s.Version != 1 {
		return errors.New("unsupported schema format version")
	}
	ids, names := map[string]bool{}, map[string]bool{}
	fieldsByCollection := map[string]map[string]map[string]any{}
	for _, c := range s.Collections {
		name, _ := c["name"].(string)
		id, _ := c["id"].(string)
		if name == "" || id == "" {
			return errors.New("collections require stable id and name; use schema export or init")
		}
		if strings.HasPrefix(name, "_") || c["system"] == true {
			return fmt.Errorf("system collection %q cannot be managed by application schema", name)
		}
		if ids[id] || names[strings.ToLower(name)] {
			return errors.New("duplicate collection id or name")
		}
		ids[id] = true
		names[strings.ToLower(name)] = true
		fieldIDs, fieldNames := map[string]bool{}, map[string]map[string]any{}
		fields, ok := c["fields"].([]any)
		if !ok {
			return fmt.Errorf("collection %q needs a fields array", name)
		}
		for _, raw := range fields {
			f, ok := raw.(map[string]any)
			fid, _ := f["id"].(string)
			fname, _ := f["name"].(string)
			ftype, _ := f["type"].(string)
			if !ok || fid == "" || fname == "" || ftype == "" || fieldIDs[fid] || fieldNames[fname] != nil {
				return fmt.Errorf("collection %q fields need unique stable IDs, names, and types", name)
			}
			fieldIDs[fid], fieldNames[fname] = true, f
		}
		fieldsByCollection[id] = fieldNames
	}
	toolNames := map[string]bool{}
	for id, exposure := range s.MCP {
		if !ids[id] {
			return fmt.Errorf("MCP references unknown collection id %q", id)
		}
		if exposure.Description == "" || len(exposure.Fields) == 0 || len(exposure.Actions) == 0 {
			return fmt.Errorf("MCP %q needs description, fields, and actions", id)
		}
		for _, action := range exposure.Actions {
			switch action {
			case "list", "get", "create", "update", "delete":
			default:
				return fmt.Errorf("invalid MCP action %q", action)
			}
			name := exposure.Name
			if name == "" {
				name = id
			}
			name += "_" + action
			if !regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`).MatchString(name) || toolNames[name] {
				return errors.New("MCP tool names must be unique and contain only letters, digits, underscores, or hyphens")
			}
			toolNames[name] = true
		}
		for _, field := range exposure.Fields {
			f := fieldsByCollection[id][field]
			if f == nil || f["hidden"] == true || f["type"] == "password" || field == "tokenKey" || strings.ContainsAny(field, ".*") {
				return fmt.Errorf("MCP field %q cannot be exposed", field)
			}
		}
	}
	if containsSecrets(s.Collections) || containsSecrets(s.Settings) {
		return errors.New("schema contains credentials; supply them through BIRALOBASE_SETTINGS_JSON or collection secret configuration")
	}
	return nil
}

func containsSecrets(v any) bool {
	switch v := v.(type) {
	case []map[string]any:
		for _, child := range v {
			if containsSecrets(child) {
				return true
			}
		}
	case []any:
		for _, child := range v {
			if containsSecrets(child) {
				return true
			}
		}
	case map[string]any:
		for k, child := range v {
			switch k {
			case "secret", "clientSecret", "password", "accessKey", "secretKey":
				if text, ok := child.(string); ok && text != "" {
					return true
				}
			}
			if containsSecrets(child) {
				return true
			}
		}
	}
	return false
}

func Hash(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func Snapshot(app core.App) ([]map[string]any, error) {
	collections, err := app.FindAllCollections()
	if err != nil {
		return nil, err
	}
	result := make([]map[string]any, 0, len(collections))
	for _, c := range collections {
		if c.System {
			continue
		}
		b, err := jsonv2.Marshal(c)
		if err != nil {
			return nil, err
		}
		var item map[string]any
		if err = json.Unmarshal(b, &item); err != nil {
			return nil, err
		}
		delete(item, "created")
		delete(item, "updated")
		stripSecrets(item)
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i]["id"].(string) < result[j]["id"].(string) })
	return result, nil
}

func stripSecrets(v any) {
	switch value := v.(type) {
	case map[string]any:
		for k, child := range value {
			if k == "secret" || k == "clientSecret" {
				delete(value, k)
			} else {
				stripSecrets(child)
			}
		}
	case []any:
		for _, child := range value {
			stripSecrets(child)
		}
	}
}

// Sandbox replays generation and validation against an isolated reference DB.
// It is never used to serve production or as a fallback for the Turso engine.
func Sandbox() (core.App, func(), error) {
	dir, err := os.MkdirTemp("", "biralobase-schema-")
	if err != nil {
		return nil, nil, err
	}
	app := core.NewBaseApp(core.BaseAppConfig{DataDir: dir})
	app.Store().Set("biralobase.schema.sandbox", true)
	cleanup := func() {
		app.OnTerminate().Trigger(&core.TerminateEvent{App: app}, func(e *core.TerminateEvent) error { return app.ClearBootstrap() })
		os.RemoveAll(dir)
	}
	if err = app.Bootstrap(); err != nil {
		cleanup()
		return nil, nil, err
	}
	return app, cleanup, nil
}

func Default() (Schema, error) {
	app, cleanup, err := Sandbox()
	if err != nil {
		return Schema{}, err
	}
	defer cleanup()
	collections, err := Snapshot(app)
	return Schema{Version: 1, Collections: collections, AuthCollection: "users"}, err
}

func Normalize(s Schema) (Schema, error) {
	if err := s.Validate(); err != nil {
		return s, err
	}
	app, cleanup, err := Sandbox()
	if err != nil {
		return s, err
	}
	defer cleanup()
	if err = Import(app, s.Collections); err != nil {
		return s, err
	}
	s.Collections, err = Snapshot(app)
	if s.AuthCollection == "" {
		s.AuthCollection = "users"
	}
	return s, err
}

func Import(app core.App, collections []map[string]any) error {
	if len(collections) > 0 {
		// Collection validation requires provider credentials. Hydrate a copy so
		// secrets never become part of a migration checksum or exported schema.
		raw, err := json.Marshal(collections)
		if err != nil {
			return err
		}
		var hydrated []map[string]any
		if err = json.Unmarshal(raw, &hydrated); err != nil {
			return err
		}
		secrets, _ := app.Store().Get("biralobase.schema.oauthSecrets").(map[string]map[string]string)
		for _, item := range hydrated {
			id, _ := item["id"].(string)
			oauth, _ := item["oauth2"].(map[string]any)
			providers, _ := oauth["providers"].([]any)
			for _, rawProvider := range providers {
				provider, _ := rawProvider.(map[string]any)
				name, _ := provider["name"].(string)
				secret := secrets[id][name]
				if secret == "" {
					if existing, err := app.FindCachedCollectionByNameOrId(id); err == nil {
						for _, p := range existing.OAuth2.Providers {
							if p.Name == name {
								secret = p.ClientSecret
							}
						}
					}
				}
				if secret == "" && app.Store().Get("biralobase.schema.sandbox") == true {
					secret = "validation-only-placeholder"
				}
				if secret == "" {
					return fmt.Errorf("OAuth provider %q on collection %q requires BIRALOBASE_OAUTH_SECRETS_JSON", name, id)
				}
				provider["clientSecret"] = secret
			}
		}
		return app.ImportCollections(hydrated, true)
	}
	return app.RunInTransaction(func(tx core.App) error {
		all, err := tx.FindAllCollections()
		if err != nil {
			return err
		}
		for _, c := range all {
			if !c.System {
				if err = tx.Delete(c); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func ConfigureSecrets(app core.App, raw string) error {
	values := map[string]map[string]string{}
	if raw != "" && json.Unmarshal([]byte(raw), &values) != nil {
		return errors.New("invalid BIRALOBASE_OAUTH_SECRETS_JSON")
	}
	app.Store().Set("biralobase.schema.oauthSecrets", values)
	return nil
}

func Write(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err = os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".biralobase-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
