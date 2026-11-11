package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/storage/turso"
	"github.com/pocketbase/dbx"
)

// consentPage starts an authorization request and returns its transaction
// identifier and browser cookie.
func (f fixture) consentPage(t *testing.T, client string) (id, cookie, verifier string) {
	t.Helper()
	verifier = strings.Repeat("a", 64)
	sum := sha256.Sum256([]byte(verifier))
	values := url.Values{"client_id": {client}, "redirect_uri": {"https://client.example/callback"}, "response_type": {"code"}, "scope": {"bb:notes:list offline_access"}, "state": {"state-123456789"}, "resource": {f.server.Resource}, "code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}}
	w := f.request("GET", "/oauth/authorize?"+values.Encode(), "", nil)
	if w.Code != 200 {
		t.Fatalf("authorize: %d %s", w.Code, w.Body.String())
	}
	match := regexp.MustCompile(`const id=("[^"]+")`).FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("missing consent transaction")
	}
	json.Unmarshal([]byte(match[1]), &id)
	for _, c := range w.Result().Cookies() {
		if c.Name == "bb_oauth_consent" {
			cookie = c.String()
		}
	}
	return id, cookie, verifier
}

func (f fixture) register(t *testing.T, name string) (int, string) {
	t.Helper()
	w := f.request("POST", "/oauth/register", `{"client_name":"`+name+`","redirect_uris":["https://client.example/callback"],"token_endpoint_auth_method":"none"}`, nil)
	var result map[string]any
	json.Unmarshal(w.Body.Bytes(), &result)
	id, _ := result["client_id"].(string)
	return w.Code, id
}

func (f fixture) tokens(t *testing.T) (access, refresh string) {
	t.Helper()
	code, verifier := f.code(t)
	w := f.exchange(url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "client_id": {f.client}, "redirect_uri": {"https://client.example/callback"}, "resource": {f.server.Resource}})
	if w.Code != 200 {
		t.Fatalf("token: %d %s", w.Code, w.Body.String())
	}
	var result map[string]any
	json.Unmarshal(w.Body.Bytes(), &result)
	access, _ = result["access_token"].(string)
	refresh, _ = result["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatal("missing tokens")
	}
	return access, refresh
}

func TestConsentIsBoundToBrowserUserAndCollection(t *testing.T) {
	f := setup(t, "sqlite") // this test creates a second auth collection
	id, cookie, _ := f.consentPage(t, f.client)
	_, otherCookie, _ := f.consentPage(t, f.client)
	body, _ := json.Marshal(map[string]any{"id": id, "allow": true})
	approve := func(token, cookie string) int {
		return f.request("POST", "/oauth/consent", string(body), map[string]string{"Authorization": token, "Cookie": cookie, "Origin": f.server.Origin}).Code
	}
	if approve(f.token, otherCookie) < 400 {
		t.Fatal("consent accepted with the cookie of another authorization request")
	}
	if approve(f.token, "") < 400 {
		t.Fatal("consent accepted without the browser cookie")
	}
	if approve("", cookie) < 400 {
		t.Fatal("consent accepted without a signed-in user")
	}
	superusers, err := f.app.FindCollectionByNameOrId(core.CollectionNameSuperusers)
	if err != nil {
		t.Fatal(err)
	}
	admin := core.NewRecord(superusers)
	admin.SetEmail("admin@example.com")
	admin.SetPassword("a-long-test-password")
	if err = f.app.Save(admin); err != nil {
		t.Fatal(err)
	}
	adminToken, _ := admin.NewAuthToken()
	if approve(adminToken, cookie) < 400 {
		t.Fatal("superuser accepted as MCP user identity")
	}
	other := core.NewAuthCollection("partners")
	if err = f.app.Save(other); err != nil {
		t.Fatal(err)
	}
	partner := core.NewRecord(other)
	partner.SetEmail("partner@example.com")
	partner.SetPassword("a-long-test-password")
	if err = f.app.Save(partner); err != nil {
		t.Fatal(err)
	}
	partnerToken, _ := partner.NewAuthToken()
	if approve(partnerToken, cookie) < 400 {
		t.Fatal("user of another auth collection accepted")
	}
	// None of the rejected attempts may have consumed the transaction.
	if approve(f.token, cookie) != 200 {
		t.Fatal("legitimate consent rejected after failed attempts")
	}
}

func TestGrantsAreBoundToClientRedirectScopeAndTokenType(t *testing.T) {
	f := setup(t, "turso")
	status, attacker := f.register(t, "Attacker")
	if status != 201 {
		t.Fatal("second registration failed")
	}
	code, verifier := f.code(t)
	redeem := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "code_verifier": {verifier}, "client_id": {attacker}, "redirect_uri": {"https://client.example/callback"}, "resource": {f.server.Resource}}
	if f.exchange(redeem).Code == 200 {
		t.Fatal("authorization code redeemed by another client")
	}
	code, verifier = f.code(t)
	redeem.Set("code", code)
	redeem.Set("client_id", f.client)
	redeem.Set("redirect_uri", "https://client.example/other")
	if f.exchange(redeem).Code == 200 {
		t.Fatal("authorization code redeemed with another redirect URI")
	}
	access, refresh := f.tokens(t)
	rotate := url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {attacker}, "resource": {f.server.Resource}}
	if f.exchange(rotate).Code == 200 {
		t.Fatal("refresh token used by another client")
	}
	if _, err := f.server.Authenticate(context.Background(), access); err != nil {
		t.Fatal("another client's failed refresh revoked the owner's grant", err)
	}
	rotate.Set("client_id", f.client)
	rotate.Set("refresh_token", access)
	if f.exchange(rotate).Code == 200 {
		t.Fatal("access token accepted as a refresh token")
	}
	if _, err := f.server.Authenticate(context.Background(), refresh); err == nil {
		t.Fatal("refresh token accepted as an access token")
	}
	// Asking for more on refresh may be refused or ignored, but the new token
	// must carry exactly what the user approved.
	rotate.Set("refresh_token", refresh)
	rotate.Set("scope", "bb:notes:list bb:notes:delete offline_access")
	if w := f.exchange(rotate); w.Code == 200 {
		var widened map[string]any
		json.Unmarshal(w.Body.Bytes(), &widened)
		token, _ := widened["access_token"].(string)
		principal, err := f.server.Authenticate(context.Background(), token)
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(principal.Scopes, " "); got != "bb:notes:list offline_access" {
			t.Fatalf("refresh widened the granted scopes to %q", got)
		}
	}
	// Revocation by another client must not affect the owner's tokens.
	access, refresh = f.tokens(t)
	f.request("POST", "/oauth/revoke", url.Values{"token": {refresh}, "client_id": {attacker}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if _, err := f.server.Authenticate(context.Background(), access); err != nil {
		t.Fatal("another client revoked the owner's grant", err)
	}
	// The owner can revoke, and both tokens stop working.
	if w := f.request("POST", "/oauth/revoke", url.Values{"token": {refresh}, "client_id": {f.client}}.Encode(), map[string]string{"Content-Type": "application/x-www-form-urlencoded"}); w.Code != 200 {
		t.Fatalf("revocation: %d %s", w.Code, w.Body.String())
	}
	if _, err := f.server.Authenticate(context.Background(), access); err == nil {
		t.Fatal("revoked grant still authenticates")
	}
}

func TestDeletedUserLosesOAuthAccess(t *testing.T) {
	f := setup(t, "turso")
	access, refresh := f.tokens(t)
	if err := f.app.Delete(f.user); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.Authenticate(context.Background(), access); err == nil {
		t.Fatal("deleted user still authenticates")
	}
	if f.exchange(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {f.client}, "resource": {f.server.Resource}}).Code == 200 {
		t.Fatal("deleted user refreshed a grant")
	}
}

// Anyone can register clients. Filling the table must not disable
// registration for good: abandoned registrations expire, while clients that a
// user has authorized are kept.
func TestAbandonedRegistrationsDoNotExhaustTheClientLimit(t *testing.T) {
	f := setup(t, "turso")
	f.code(t) // the fixture client has been authorized by a user
	for i := 0; i < 999; i++ {
		id := fmt.Sprintf("flood-%04d", i)
		raw := `{"id":"` + id + `","redirect_uris":["https://flood.example/callback"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"scopes":["bb:notes:list","offline_access"],"audience":["https://api.example.com/mcp"],"public":true}`
		if _, err := f.app.DB().NewQuery("INSERT INTO _bb_oauth_clients (id,name,value,created,authorized) VALUES ({:id},'Flood',{:value},{:created},0)").Bind(dbx.Params{"id": id, "value": raw, "created": time.Now().Unix()}).Execute(); err != nil {
			t.Fatal(err)
		}
	}
	if status, _ := f.register(t, "Late"); status != 429 {
		t.Fatalf("registration beyond the limit: %d", status)
	}
	// A day later the abandoned registrations no longer count.
	if _, err := f.app.DB().NewQuery("UPDATE _bb_oauth_clients SET created={:old} WHERE name='Flood'").Bind(dbx.Params{"old": time.Now().Add(-25 * time.Hour).Unix()}).Execute(); err != nil {
		t.Fatal(err)
	}
	if status, _ := f.register(t, "Late"); status != 201 {
		t.Fatalf("registration stayed disabled after abandoned clients expired: %d", status)
	}
	if _, err := f.server.Store.GetClient(context.Background(), f.client); err != nil {
		t.Fatal("an authorized client was removed with the abandoned ones", err)
	}
}

func TestAddressesOfOneIPv6NetworkShareARateLimit(t *testing.T) {
	if limiterKey("2001:db8:1:2:aaaa::1") != limiterKey("2001:db8:1:2:bbbb::2") {
		t.Fatal("addresses of one /64 are limited separately")
	}
	if limiterKey("2001:db8:1:2::1") == limiterKey("2001:db8:1:3::1") {
		t.Fatal("separate /64 networks share a limit")
	}
	if limiterKey("203.0.113.7") == limiterKey("203.0.113.8") || limiterKey("::ffff:203.0.113.7") != limiterKey("203.0.113.7") {
		t.Fatal("IPv4 addresses must be limited individually, in either notation")
	}
	f := setup(t, "sqlite")
	limited := 0
	for i := 0; i < 5000; i++ {
		r := httptest.NewRequest("GET", f.server.Origin+"/oauth/authorize", nil)
		r.RemoteAddr = fmt.Sprintf("[2001:db8:1:2:%x:%x::1]:1234", i>>8, i&0xff)
		w := httptest.NewRecorder()
		f.handler.ServeHTTP(w, r)
		if w.Code == 429 {
			limited++
		}
	}
	if limited < 4900 {
		t.Fatalf("one IPv6 network made 5000 requests and only %d were limited", limited)
	}
	// The flood must not have filled the table against everyone else.
	r := httptest.NewRequest("GET", f.server.Origin+"/oauth/authorize", nil)
	r.RemoteAddr = "198.51.100.9:1234"
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	if w.Code == 429 {
		t.Fatal("an unrelated address was refused after one network's flood")
	}
}

// A database written by an earlier runtime has a client table without the
// expiry columns. Its clients may hold live grants and must be kept.
func TestEarlierClientTableIsUpgradedInPlace(t *testing.T) {
	for _, engineName := range []string{"sqlite", "turso"} {
		t.Run(engineName, func(t *testing.T) {
			config := core.BaseAppConfig{DataDir: t.TempDir()}
			if engineName == "turso" {
				config.DBConnect = new(turso.Engine).Connect
			}
			app := core.NewBaseApp(config)
			if err := app.Bootstrap(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { app.OnTerminate().Trigger(&core.TerminateEvent{App: app}); app.ClearBootstrap() })
			for _, query := range []string{
				`CREATE TABLE _bb_oauth_clients (id TEXT PRIMARY KEY, name TEXT NOT NULL, value TEXT NOT NULL)`,
				`INSERT INTO _bb_oauth_clients (id,name,value) VALUES ('earlier','Earlier client','{"id":"earlier","redirect_uris":["https://client.example/callback"],"public":true}')`,
			} {
				if _, err := app.DB().NewQuery(query).Execute(); err != nil {
					t.Fatal(err)
				}
			}
			for range 2 { // every start runs the upgrade check
				s, err := New(app, "https://api.example.com", "users", []string{"bb:notes:list"}, []byte(strings.Repeat("x", 32)))
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.Store.GetClient(context.Background(), "earlier"); err != nil {
					t.Fatal(err)
				}
			}
			var authorized int
			if err := app.DB().NewQuery("SELECT authorized FROM _bb_oauth_clients WHERE id='earlier'").Row(&authorized); err != nil || authorized != 1 {
				t.Fatalf("earlier client could be removed as abandoned: authorized=%d %v", authorized, err)
			}
		})
	}
}
