package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/oauth"
	"github.com/biralo-studio/biralobase/internal/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
)

type bearerTransport struct{ token string }

func credentialForTest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}
func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestMCPUserRulesFieldsScopesAndOfficialSDK(t *testing.T) {
	app, cleanup, err := schema.Sandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	users, _ := app.FindCollectionByNameOrId("users")
	makeUser := func(email string) *core.Record {
		r := core.NewRecord(users)
		r.SetEmail(email)
		r.SetPassword("long-test-password")
		if err := app.Save(r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	alice, bob := makeUser("alice@example.com"), makeUser("bob@example.com")
	c := core.NewBaseCollection("notes")
	c.Fields.Add(&core.TextField{Name: "title"}, &core.TextField{Name: "private", Hidden: true}, &core.RelationField{Name: "owner", CollectionId: users.Id, MaxSelect: 1})
	rule := "owner = @request.auth.id"
	c.ListRule = &rule
	c.ViewRule = &rule
	c.UpdateRule = &rule
	if err = app.Save(c); err != nil {
		t.Fatal(err)
	}
	r := core.NewRecord(c)
	r.Set("owner", alice.Id)
	r.Set("title", "Alice note")
	r.Set("private", "hidden value")
	if err = app.Save(r); err != nil {
		t.Fatal(err)
	}
	collections, _ := schema.Snapshot(app)
	desired := schema.Schema{Version: 1, Collections: collections, AuthCollection: "users", MCP: map[string]schema.Exposure{c.Id: {Name: "notes", Description: "Your notes", Actions: []string{"list", "get", "update"}, Fields: []string{"id", "title"}}}}
	api, _ := apis.NewRouter(app)
	handler, _ := api.BuildMux()
	principal := oauth.Principal{User: alice, Scopes: Scopes(desired)}
	value, err := execute(context.Background(), handler, principal, c.Id, "list", []string{"id", "title"}, []byte(`{}`), "127.0.0.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(value)
	if strings.Contains(string(raw), "hidden value") || strings.Contains(string(raw), "owner") || !strings.Contains(string(raw), "Alice note") {
		t.Fatal("field filtering failed")
	}
	principal.User = bob
	value, err = execute(context.Background(), handler, principal, c.Id, "list", []string{"id", "title"}, []byte(`{}`), "127.0.0.1:1234")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(value)
	if strings.Contains(string(raw), "Alice note") {
		t.Fatal("other user's data leaked")
	}
	get, _ := json.Marshal(map[string]string{"id": r.Id})
	if _, err = execute(context.Background(), handler, principal, c.Id, "get", []string{"id", "title"}, get, ""); err == nil {
		t.Fatal("other user's record accessible")
	}
	principal.User = alice
	update, _ := json.Marshal(map[string]any{"id": r.Id, "data": map[string]string{"owner": bob.Id}})
	if _, err = execute(context.Background(), handler, principal, c.Id, "update", []string{"id", "title"}, update, ""); err == nil {
		t.Fatal("unexposed write field accepted")
	}
	principal.Scopes = []string{Scope(c.Id, "list")}
	if _, err = execute(context.Background(), handler, principal, c.Id, "get", []string{"id", "title"}, get, ""); err == nil {
		t.Fatal("missing scope accepted")
	}
	key := []byte(strings.Repeat("z", 32))
	authorization, err := oauth.New(app, "https://api.example.com", "users", Scopes(desired), key)
	if err != nil {
		t.Fatal(err)
	}
	// Mint a real Fosite access token to exercise the official MCP HTTP client;
	// authorization code / browser consent are covered in oauth/server_test.go.
	request := fosite.NewRequest()
	request.Client = &fosite.DefaultClient{ID: "sdk-client", Public: true}
	request.GrantedScope = principal.Scopes
	request.GrantedAudience = []string{authorization.Resource}
	sum := credentialForTest(alice.TokenKey())
	request.Session = &fosite.DefaultSession{Subject: alice.Id, ExpiresAt: map[fosite.TokenType]time.Time{fosite.AccessToken: time.Now().Add(time.Minute)}, Extra: map[string]any{"collection": users.Id, "credential": sum}}
	strategy := compose.NewOAuth2HMACStrategy(&fosite.Config{GlobalSecret: key})
	token, signature, err := strategy.GenerateAccessToken(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if err = authorization.Store.CreateAccessTokenSession(context.Background(), signature, request); err != nil {
		t.Fatal(err)
	}
	mcpHandler, err := New(authorization, desired, handler)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(mcpHandler)
	defer server.Close()
	unauthorized, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Body.Close()
	if unauthorized.StatusCode != 401 || !strings.Contains(unauthorized.Header.Get("WWW-Authenticate"), "resource_metadata") {
		t.Fatal("missing OAuth challenge")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "compatibility-test", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: &http.Client{Transport: bearerTransport{token}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "notes_list" {
		t.Fatal("tools were not filtered by granted scopes")
	}
	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "notes_list", Arguments: map[string]any{}})
	if err != nil || result.IsError {
		t.Fatal("official SDK tool call failed", err, result)
	}
	if err = authorization.Store.DeleteAccessTokenSession(context.Background(), signature); err != nil {
		t.Fatal(err)
	}
	if _, err = session.ListTools(context.Background(), nil); err == nil {
		t.Fatal("revoked token accepted")
	}
}
