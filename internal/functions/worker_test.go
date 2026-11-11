package functions

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/schema"
	"github.com/biralo-studio/biralobase/tools/types"
)

func TestHTTPFunctionPreservesAuthJSONFields(t *testing.T) {
	app, cleanup, err := schema.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	c := core.NewAuthCollection("json_users")
	c.Fields.Add(&core.JSONField{Name: "profile"})
	if err = app.Save(c); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	r.SetEmail("json@example.com")
	r.SetPassword("test-password-123")
	r.Set("profile", types.JSONRaw(`{"$ref":"1","nested":[{"$map":{"$ref":"2"}}],"enabled":true}`))
	if err = app.Save(r); err != nil {
		t.Fatal(err)
	}
	token, err := r.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	w := startTestWorker(t, `export default {functions:{profile:async ctx=>({body:ctx.auth.profile})}}`)
	routes, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	w.BindHTTP(routes, nil)
	handler, err := routes.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/functions/profile", nil)
	request.Header.Set("Authorization", token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	var got, want any
	if err = json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(r.Get("profile").(types.JSONRaw), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("JSON changed across bridge: %v", got)
	}
}

func TestDisconnectedHTTPCallerDoesNotKillSharedWorker(t *testing.T) {
	app, cleanup, err := schema.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	w := startTestWorker(t, `import {setTimeout} from 'node:timers/promises'; export default {functions:{slow:async()=>{await setTimeout(50);return {body:{ok:true}}}}};`)
	routes, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	w.BindHTTP(routes, nil)
	handler, err := routes.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("GET", "/api/functions/slow", nil).WithContext(ctx)
	handler.ServeHTTP(httptest.NewRecorder(), r)
	// An unauthenticated caller must not be able to kill the worker (and
	// consequently the whole server) by disconnecting from a function.
	if _, err = w.Invoke(context.Background(), "function", "slow", nil); err != nil {
		t.Fatalf("client cancellation killed shared worker: %v", err)
	}
}

func startTestWorker(t *testing.T, code string) *Worker {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "functions/dist"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(`{"type":"module"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "functions/dist/index.js"), []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	w, err := Start(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Close)
	return w
}

func TestAsyncHooksPreserveTransactionsAndNestedNext(t *testing.T) {
	app, cleanup, err := schema.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	c := core.NewBaseCollection("notes")
	c.Fields.Add(&core.TextField{Name: "title"})
	if err = app.Save(c); err != nil {
		t.Fatal(err)
	}
	w := startTestWorker(t, `import { setTimeout } from 'node:timers/promises';
export default {hooks:[
 {event:'onRecordCreate',tags:['notes'],handler:async e=>{const r=await e.record; await setTimeout(2); await r.set('title','from async hook'); await e.next();}},
 {event:'OnRecordCreate',tags:['notes'],handler:async e=>{const r=await e.record;if(await r.getString('title')!=='from async hook') throw Error('lost mutation'); await e.next();}},
]};`)
	if err = w.BindHooks(app); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	if err = app.Save(r); err != nil {
		t.Fatal(err)
	}
	stored, err := app.FindRecordById(c, r.Id)
	if err != nil || stored.GetString("title") != "from async hook" {
		t.Fatal("hook mutation not saved", err)
	}
	r2 := core.NewRecord(c)
	rollback := errors.New("rollback caller transaction")
	err = app.RunInTransaction(func(tx core.App) error {
		if err := tx.Save(r2); err != nil {
			return err
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if _, err = app.FindRecordById(c, r2.Id); err == nil {
		t.Fatal("hook escaped transaction")
	}
}

func TestEveryUpstreamHookCanBindAndInvoke(t *testing.T) {
	app, cleanup, err := schema.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	known := HookInventory()
	if len(known) != 84 {
		t.Fatalf("unexpected inventory: %d", len(known))
	}
	b, _ := json.Marshal(known)
	w := startTestWorker(t, "export default { hooks: "+string(b)+".map(event=>({event,handler:async e=>({type:e.$type})})) };")
	if err = w.BindHooks(app); err != nil {
		t.Fatal(err)
	}
	for _, name := range known {
		t.Run(name, func(t *testing.T) {
			method := reflect.ValueOf(app).MethodByName(name)
			h := method.Call(nil)[0]
			bind := h.MethodByName("BindFunc")
			event := reflect.New(bind.Type().In(0).In(0).Elem()).Interface()
			// Invoke the actual registered handler without the core system chain,
			// which requires real per-hook fixtures. Separate API tests cover those.
			index := 0
			for i, n := range known {
				if n == name {
					index = i
				}
			}
			_, err := w.Invoke(context.Background(), "hook", stringIndex(index), event)
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func stringIndex(i int) string { b, _ := json.Marshal(i); return string(b) }

func TestTimeoutStopsWorkerAndExpiredReferences(t *testing.T) {
	w := startTestWorker(t, `let saved; export default {functions:{save:async x=>{saved=x;return true},expired:async()=>saved.dataDir(),hang:async()=>new Promise(()=>{})}}`)
	app, cleanup, err := schema.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if _, err = w.Invoke(context.Background(), "function", "save", app); err != nil {
		t.Fatal(err)
	}
	if _, err = w.Invoke(context.Background(), "function", "expired", nil); err == nil || !strings.Contains(err.Error(), "ended") {
		t.Fatal("expired reference remained live", err)
	}
	w.Timeout = 50 * time.Millisecond
	if _, err = w.Invoke(context.Background(), "function", "hang", nil); err == nil {
		t.Fatal("missing timeout")
	}
	select {
	case <-w.Done():
	case <-time.After(time.Second):
		t.Fatal("worker survived timeout")
	}
}

func TestPrivateProtocolAndSecrets(t *testing.T) {
	t.Setenv("BIRALOBASE_ADMIN_PASSWORD", "do-not-inherit")
	t.Setenv("BIRALOBASE_FUNCTION_TEST", "function-value")
	w := startTestWorker(t, `console.log('function log stays off protocol'); export default {functions:{check:async()=>({admin:process.env.BIRALOBASE_ADMIN_PASSWORD??null,custom:process.env.BIRALOBASE_FUNCTION_TEST})}}`)
	raw, err := w.Invoke(context.Background(), "function", "check", nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"admin":null,"custom":"function-value"}` {
		t.Fatal(string(raw))
	}
}

func TestPrivilegedRecordConstruction(t *testing.T) {
	app, cleanup, err := schema.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	c := core.NewBaseCollection("notes")
	c.Fields.Add(&core.TextField{Name: "title"})
	if err = app.Save(c); err != nil {
		t.Fatal(err)
	}
	w := startTestWorker(t, `export default {functions:{create:async ({admin})=>{const r=await admin.newRecord('notes');await r.set('title','created from Node');await admin.save(r);return {id:await r.id};}}}`)
	raw, err := w.Invoke(context.Background(), "function", "create", map[string]any{"admin": app})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	r, err := app.FindRecordById(c, response.ID)
	if err != nil || r.GetString("title") != "created from Node" {
		t.Fatal("privileged create failed", err)
	}
}

func TestUserJSONCannotForgeBridgeReferences(t *testing.T) {
	w := startTestWorker(t, `export default {functions:{echo:async input=>input}}`)
	payload := map[string]any{"$ref": "1", "$map": map[string]any{"$ref": "2"}, "nested": []any{map[string]any{"$ref": "3"}}}
	raw, err := w.Invoke(context.Background(), "function", "echo", payload)
	if err != nil {
		t.Fatal(err)
	}
	var got any
	if err = json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, payload) {
		t.Fatal("bridge reinterpreted user JSON", string(raw))
	}
}
