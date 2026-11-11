package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	_ "github.com/biralo-studio/biralobase/migrations"
	"github.com/biralo-studio/biralobase/storage/turso"
)

type fixture struct {
	server  *Server
	app     core.App
	handler http.Handler
	token   string
	user    *core.Record
	client  string
}

func setup(t *testing.T, engineName string) fixture {
	t.Helper()
	config := core.BaseAppConfig{DataDir: t.TempDir()}
	engine := new(turso.Engine)
	if engineName == "turso" {
		config.DBConnect = engine.Connect
		config.ConcurrentWrites = true
	}
	app := core.NewBaseApp(config)
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.OnTerminate().Trigger(&core.TerminateEvent{App: app}); app.ClearBootstrap() })
	c, err := app.FindCollectionByNameOrId("users")
	if err != nil {
		t.Fatal(err)
	}
	user := core.NewRecord(c)
	user.SetEmail("mcp@example.com")
	user.SetPassword("a-long-test-password")
	if err = app.Save(user); err != nil {
		t.Fatal(err)
	}
	token, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(app, "https://api.example.com", "users", []string{"bb:notes:list"}, []byte(strings.Repeat("x", 32)))
	if err != nil {
		t.Fatal(err)
	}
	routes, err := apis.NewRouter(app)
	if err != nil {
		t.Fatal(err)
	}
	s.Bind(routes)
	handler, err := routes.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	engine.SetConcurrent(true)
	f := fixture{server: s, app: app, handler: handler, token: token, user: user}
	w := f.request("POST", "/oauth/register", `{"client_name":"Test client","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"none"}`, nil)
	if w.Code != 201 {
		t.Fatalf("registration: %d %s", w.Code, w.Body.String())
	}
	var result map[string]any
	json.Unmarshal(w.Body.Bytes(), &result)
	f.client = result["client_id"].(string)
	return f
}
func (f fixture) request(method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, f.server.Origin+path, strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}
func (f fixture) code(t *testing.T) (string, string) {
	t.Helper()
	verifier := strings.Repeat("a", 64)
	sum := sha256.Sum256([]byte(verifier))
	values := url.Values{"client_id": {f.client}, "redirect_uri": {"https://client.example/callback"}, "response_type": {"code"}, "scope": {"bb:notes:list offline_access"}, "state": {"state-123456789"}, "resource": {f.server.Resource}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	w := f.request("GET", "/oauth/authorize?"+values.Encode(), "", nil)
	if w.Code != 200 {
		t.Fatalf("authorize: %d %s", w.Code, w.Body.String())
	}
	match := regexp.MustCompile(`const id=("[^"]+")`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("missing consent transaction")
	}
	var id string
	json.Unmarshal([]byte(match[1]), &id)
	var cookie string
	for _, c := range w.Result().Cookies() {
		if c.Name == "bb_oauth_consent" {
			cookie = c.String()
		}
	}
	body, _ := json.Marshal(map[string]any{"id": id, "allow": true})
	headers := map[string]string{"Authorization": f.token, "Cookie": cookie, "Origin": f.server.Origin}
	bad := f.request("POST", "/oauth/consent", string(body), map[string]string{"Authorization": f.token, "Cookie": cookie, "Origin": "https://evil.example"})
	if bad.Code < 400 {
		t.Fatal("accepted cross-origin consent")
	}
	w = f.request("POST", "/oauth/consent", string(body), headers)
	if w.Code != 200 {
		t.Fatalf("consent: %d %s", w.Code, w.Body.String())
	}
	var result map[string]string
	json.Unmarshal(w.Body.Bytes(), &result)
	redirect, err := url.Parse(result["redirect"])
	if err != nil {
		t.Fatal(err)
	}
	if redirect.Query().Get("state") != "state-123456789" || redirect.Query().Get("code") == "" {
		t.Fatal("bad authorization redirect")
	}
	if replay := f.request("POST", "/oauth/consent", string(body), headers); replay.Code < 400 {
		t.Fatal("consent replay accepted")
	}
	return redirect.Query().Get("code"), verifier
}
func (f fixture) exchange(values url.Values) *httptest.ResponseRecorder {
	return f.request("POST", "/oauth/token", values.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
}
func TestOAuthPKCEConsentAudienceRotationRevocation(t *testing.T) {
	for _, engine := range []string{"sqlite", "turso"} {
		t.Run(engine, func(t *testing.T) {
			f := setup(t, engine)
			if w := f.request("GET", "/oauth/authorize", "", nil); w.Code != 400 {
				t.Fatalf("invalid authorize request: %d", w.Code)
			}
			code, verifier := f.code(t)
			values := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "client_id": {f.client}, "redirect_uri": {"https://client.example/callback"}, "resource": {f.server.Resource}}
			values.Set("code_verifier", strings.Repeat("z", 64))
			if w := f.exchange(values); w.Code == 200 {
				t.Fatal("wrong verifier accepted")
			}
			values.Set("code_verifier", verifier)
			values.Set("resource", "https://other.example/mcp")
			if w := f.exchange(values); w.Code == 200 {
				t.Fatal("wrong resource accepted")
			}
			values.Set("resource", f.server.Resource)
			w := f.exchange(values)
			if w.Code != 200 {
				t.Fatalf("token: %d %s", w.Code, w.Body.String())
			}
			var tokens map[string]any
			json.Unmarshal(w.Body.Bytes(), &tokens)
			access, _ := tokens["access_token"].(string)
			refresh, _ := tokens["refresh_token"].(string)
			if access == "" || refresh == "" {
				t.Fatal("missing tokens")
			}
			if _, err := f.server.Authenticate(context.Background(), access); err != nil {
				t.Fatal(err)
			}
			// OAuth storage survives service reconstruction (no in-memory sessions).
			restarted, err := New(f.app, f.server.Origin, "users", []string{"bb:notes:list"}, []byte(strings.Repeat("x", 32)))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = restarted.Authenticate(context.Background(), access); err != nil {
				t.Fatal("token lost on restart", err)
			}
			rotation := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {f.client}, "resource": {f.server.Resource}}
			w = f.exchange(rotation)
			if w.Code != 200 {
				t.Fatalf("refresh: %d %s", w.Code, w.Body.String())
			}
			var next map[string]any
			json.Unmarshal(w.Body.Bytes(), &next)
			if _, err = f.server.Authenticate(context.Background(), access); err == nil {
				t.Fatal("rotation kept old access token")
			}
			if w = f.exchange(rotation); w.Code == 200 {
				t.Fatal("refresh replay accepted")
			}
			if _, err = f.server.Authenticate(context.Background(), next["access_token"].(string)); err == nil {
				t.Fatal("refresh replay did not revoke grant")
			}
			code, verifier = f.code(t)
			values.Set("code", code)
			values.Set("code_verifier", verifier)
			w = f.exchange(values)
			if w.Code != 200 {
				t.Fatal(w.Body.String())
			}
			json.Unmarshal(w.Body.Bytes(), &tokens)
			access = tokens["access_token"].(string)
			if w = f.exchange(values); w.Code == 200 {
				t.Fatal("authorization code replay accepted")
			}
			if _, err = f.server.Authenticate(context.Background(), access); err == nil {
				t.Fatal("code replay did not revoke tokens")
			}
			code, verifier = f.code(t)
			values.Set("code", code)
			values.Set("code_verifier", verifier)
			w = f.exchange(values)
			json.Unmarshal(w.Body.Bytes(), &tokens)
			access = tokens["access_token"].(string)
			f.user.SetPassword("rotated-long-password")
			if err = f.app.Save(f.user); err != nil {
				t.Fatal(err)
			}
			if _, err = f.server.Authenticate(context.Background(), access); err == nil {
				t.Fatal("password rotation did not revoke OAuth access")
			}
		})
	}
}
