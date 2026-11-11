// Package oauth provides the application-user OAuth authorization server for MCP.
package oauth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/biralo-studio/biralobase/apis"
	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/tools/router"
	"github.com/biralo-studio/biralobase/tools/security"
	"github.com/ory/fosite"
	"github.com/ory/fosite/compose"
	"github.com/pocketbase/dbx"
)

type Principal struct {
	User   *core.Record
	Scopes []string
}
type Server struct {
	App            core.App
	Provider       fosite.OAuth2Provider
	Store          *Store
	Origin         string
	Resource       string
	AuthCollection string
	Scopes         []string
	mu             sync.Mutex
	limiterMu      sync.Mutex
	requests       map[string]window
}
type window struct {
	Start time.Time
	Count int
}

const (
	maxClients         = 1000
	abandonedClientAge = 24 * time.Hour
)

func New(app core.App, origin, authCollection string, scopes []string, secret []byte) (*Server, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return nil, errors.New("OAuth requires a canonical public origin")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname())) {
		return nil, errors.New("OAuth requires HTTPS except on loopback")
	}
	if len(secret) < 32 {
		return nil, errors.New("OAuth signing secret must contain at least 32 bytes")
	}
	c, err := app.FindCachedCollectionByNameOrId(authCollection)
	if err != nil || !c.IsAuth() || c.System {
		return nil, errors.New("MCP authCollection must be an application auth collection")
	}
	s := &Server{App: app, Origin: strings.TrimSuffix(origin, "/"), AuthCollection: c.Id, Scopes: append(slices.Clone(scopes), "offline_access"), requests: map[string]window{}}
	s.Resource = s.Origin + "/mcp"
	s.Store = &Store{App: app}
	if err = s.Store.Init(); err != nil {
		return nil, err
	}
	config := &fosite.Config{GlobalSecret: secret, AccessTokenLifespan: 15 * time.Minute, RefreshTokenLifespan: 7 * 24 * time.Hour, AuthorizeCodeLifespan: 2 * time.Minute, EnforcePKCE: true, EnablePKCEPlainChallengeMethod: false, ScopeStrategy: fosite.ExactScopeStrategy, RefreshTokenScopes: []string{"offline_access"}, DisableRefreshTokenValidation: true}
	s.Provider = compose.Compose(config, s.Store, compose.NewOAuth2HMACStrategy(config), compose.OAuth2AuthorizeExplicitFactory, compose.OAuth2RefreshTokenGrantFactory, compose.OAuth2TokenIntrospectionFactory, compose.OAuth2TokenRevocationFactory, compose.OAuth2PKCEFactory)
	return s, nil
}

func isLoopback(host string) bool {
	return host == "localhost" || (net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())
}
func credential(user *core.Record) string {
	// PocketBase invalidates JWTs with both the record key and collection key.
	// OAuth grants must follow collection-wide revocation as well as password
	// changes; otherwise MCP could mint fresh JWTs after an administrator reset.
	h := sha256.Sum256([]byte(user.TokenKey() + "\x00" + user.Collection().AuthToken.Secret))
	return hex.EncodeToString(h[:])
}

func (s *Server) current(session fosite.Session) (*core.Record, error) {
	d, ok := session.(*fosite.DefaultSession)
	if !ok || d.Extra["collection"] != s.AuthCollection {
		return nil, fosite.ErrRequestUnauthorized
	}
	user, err := s.App.FindRecordById(s.AuthCollection, d.Subject)
	if err != nil || user.IsSuperuser() {
		return nil, fosite.ErrRequestUnauthorized
	}
	expected, _ := d.Extra["credential"].(string)
	if subtle.ConstantTimeCompare([]byte(expected), []byte(credential(user))) != 1 {
		return nil, fosite.ErrRequestUnauthorized
	}
	// A grant cannot keep minting user tokens after the account no longer
	// satisfies its authentication rule (for example verified = true).
	// This service authenticates as OAuth2, without password-login form data.
	allowed, err := s.App.CanAccessRecord(user, &core.RequestInfo{
		Auth: user, Context: core.RequestInfoContextOAuth2,
		Method: http.MethodPost, Query: map[string]string{},
		Headers: map[string]string{}, Body: map[string]any{},
	}, user.Collection().AuthRule)
	if err != nil || !allowed {
		return nil, fosite.ErrRequestUnauthorized
	}
	return user, nil
}

func (s *Server) Authenticate(ctx context.Context, token string) (Principal, error) {
	// The requested token type is only a hint to fosite, which otherwise goes
	// on to accept a refresh token. A refresh token must never work as a bearer
	// credential: it outlives an access token by days and using it here would
	// bypass rotation and replay detection at the token endpoint.
	use, r, err := s.Provider.IntrospectToken(ctx, token, fosite.AccessToken, new(fosite.DefaultSession))
	if err != nil {
		return Principal{}, err
	}
	if use != fosite.AccessToken {
		return Principal{}, fosite.ErrRequestUnauthorized
	}
	if !r.GetGrantedAudience().Has(s.Resource) {
		return Principal{}, fosite.ErrRequestUnauthorized
	}
	user, err := s.current(r.GetSession())
	if err != nil {
		return Principal{}, err
	}
	return Principal{User: user, Scopes: r.GetGrantedScopes()}, nil
}

func (s *Server) Bind(routes *router.Router[*core.RequestEvent]) {
	routes.GET("/.well-known/oauth-authorization-server", func(e *core.RequestEvent) error {
		return e.JSON(200, map[string]any{
			"issuer": s.Origin, "authorization_endpoint": s.Origin + "/oauth/authorize", "token_endpoint": s.Origin + "/oauth/token", "registration_endpoint": s.Origin + "/oauth/register", "revocation_endpoint": s.Origin + "/oauth/revoke",
			"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"}, "token_endpoint_auth_methods_supported": []string{"none"}, "code_challenge_methods_supported": []string{"S256"}, "scopes_supported": s.Scopes,
		})
	})
	metadata := func(e *core.RequestEvent) error {
		return e.JSON(200, map[string]any{"resource": s.Resource, "authorization_servers": []string{s.Origin}, "scopes_supported": s.Scopes, "bearer_methods_supported": []string{"header"}})
	}
	routes.GET("/.well-known/oauth-protected-resource", metadata)
	routes.GET("/.well-known/oauth-protected-resource/mcp", metadata)
	group := routes.Group("/oauth").Bind(apis.BodyLimit(32 << 10)).BindFunc(s.limit)
	group.POST("/register", s.register)
	group.GET("/authorize", s.authorize)
	group.POST("/consent", s.consent)
	group.POST("/token", apis.WrapStdHandler(http.HandlerFunc(s.token)))
	group.POST("/revoke", apis.WrapStdHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		err := s.Provider.NewRevocationRequest(r.Context(), r)
		s.Provider.WriteRevocationResponse(r.Context(), w, err)
	})))
}

func (s *Server) limit(e *core.RequestEvent) error {
	// Bound memory and use the actual peer unless the operator configures a
	// trusted proxy through PocketBase's validated proxy settings.
	key := limiterKey(e.RealIP())
	now := time.Now()
	s.limiterMu.Lock()
	if len(s.requests) >= 4096 {
		for ip, w := range s.requests {
			if now.Sub(w.Start) > time.Minute {
				delete(s.requests, ip)
			}
		}
	}
	w := s.requests[key]
	if now.Sub(w.Start) > time.Minute {
		w = window{Start: now}
	}
	w.Count++
	allowed := w.Count <= 60 && (len(s.requests) < 4096 || !s.requests[key].Start.IsZero())
	if allowed {
		s.requests[key] = w
	}
	s.limiterMu.Unlock()
	if !allowed {
		e.Response.Header().Set("Retry-After", "60")
		return e.JSON(429, map[string]string{"error": "temporarily_unavailable"})
	}
	e.Response.Header().Set("Cache-Control", "no-store")
	e.Response.Header().Set("Referrer-Policy", "no-referrer")
	return e.Next()
}

// One IPv6 subscriber controls at least a /64. Counting each of its addresses
// separately would let a single host both evade the limit and fill the table,
// which refuses every address it has not seen while it is full.
func limiterKey(ip string) string {
	address, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	address = address.Unmap()
	if address.Is4() {
		return address.String()
	}
	prefix, err := address.Prefix(64)
	if err != nil {
		return ip
	}
	return prefix.String()
}

func (s *Server) register(e *core.RequestEvent) error {
	var form struct {
		Name      string   `json:"client_name"`
		Redirects []string `json:"redirect_uris"`
		Method    string   `json:"token_endpoint_auth_method"`
		Grants    []string `json:"grant_types"`
		Responses []string `json:"response_types"`
	}
	if err := e.BindBody(&form); err != nil {
		return e.JSON(400, map[string]string{"error": "invalid_client_metadata"})
	}
	if form.Name == "" || len(form.Name) > 100 || len(form.Redirects) == 0 || len(form.Redirects) > 10 || (form.Method != "" && form.Method != "none") {
		return e.JSON(400, map[string]string{"error": "invalid_client_metadata"})
	}
	for _, grant := range form.Grants {
		if grant != "authorization_code" && grant != "refresh_token" {
			return e.JSON(400, map[string]string{"error": "invalid_client_metadata"})
		}
	}
	for _, response := range form.Responses {
		if response != "code" {
			return e.JSON(400, map[string]string{"error": "invalid_client_metadata"})
		}
	}
	for _, raw := range form.Redirects {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" || strings.Contains(raw, "*") || (u.Scheme != "https" && !(u.Scheme == "http" && isLoopback(u.Hostname()))) {
			return e.JSON(400, map[string]string{"error": "invalid_redirect_uri"})
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.App.DB().NewQuery("SELECT count(*) FROM _bb_oauth_clients").Row(&count); err != nil {
		return err
	}
	if count >= maxClients {
		// Registration is unauthenticated, so the table can be filled by anyone.
		// Registrations that no user ever authorized are abandoned after a day;
		// without removing them the limit would disable registration for good.
		if _, err := s.App.DB().NewQuery("DELETE FROM _bb_oauth_clients WHERE authorized=0 AND created < {:before}").Bind(dbx.Params{"before": time.Now().Add(-abandonedClientAge).Unix()}).Execute(); err != nil {
			return err
		}
		if err := s.App.DB().NewQuery("SELECT count(*) FROM _bb_oauth_clients").Row(&count); err != nil {
			return err
		}
	}
	if count >= maxClients {
		e.Response.Header().Set("Retry-After", "3600")
		return e.JSON(429, map[string]string{"error": "registration_limit_reached"})
	}
	c := &fosite.DefaultClient{ID: security.RandomString(32), RedirectURIs: form.Redirects, GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, Scopes: s.Scopes, Audience: []string{s.Resource}, Public: true}
	if err := s.Store.PutClient(e.Request.Context(), form.Name, c); err != nil {
		return err
	}
	return e.JSON(201, map[string]any{"client_id": c.ID, "client_name": form.Name, "redirect_uris": c.RedirectURIs, "grant_types": c.GrantTypes, "response_types": c.ResponseTypes, "token_endpoint_auth_method": "none", "scope": strings.Join(c.Scopes, " ")})
}

func (s *Server) request(ctx context.Context, query string) (fosite.AuthorizeRequester, error) {
	values, err := url.ParseQuery(query)
	if err != nil || len(values["resource"]) != 1 || values.Get("resource") != s.Resource || values.Get("code_challenge_method") != "S256" || len(values.Get("code_challenge")) != 43 {
		return fosite.NewAuthorizeRequest(), fosite.ErrInvalidRequest
	}
	values.Set("audience", s.Resource)
	r, _ := http.NewRequestWithContext(ctx, "GET", s.Origin+"/oauth/authorize?"+values.Encode(), nil)
	request, err := s.Provider.NewAuthorizeRequest(ctx, r)
	if err != nil {
		return request, err
	}
	if len(request.GetRequestedScopes()) == 0 {
		return request, fosite.ErrInvalidScope
	}
	for _, scope := range request.GetRequestedScopes() {
		if !slices.Contains(s.Scopes, scope) {
			return request, fosite.ErrInvalidScope
		}
	}
	return request, nil
}

func (s *Server) authorize(e *core.RequestEvent) error {
	request, err := s.request(e.Request.Context(), e.Request.URL.RawQuery)
	if err != nil {
		s.Provider.WriteAuthorizeError(e.Request.Context(), e.Response, request, err)
		return nil
	}
	var name string
	if err = s.App.DB().NewQuery("SELECT name FROM _bb_oauth_clients WHERE id={:id}").Bind(dbx.Params{"id": request.GetClient().GetID()}).Row(&name); err != nil {
		return err
	}
	id, cookie := security.RandomString(40), security.RandomString(40)
	_, err = s.App.DB().NewQuery("DELETE FROM _bb_oauth_consent WHERE expires < {:now}").Bind(dbx.Params{"now": time.Now().Unix()}).Execute()
	if err != nil {
		return err
	}
	_, err = s.App.DB().NewQuery("INSERT INTO _bb_oauth_consent (id,cookie,query,expires) VALUES ({:id},{:cookie},{:query},{:expires})").Bind(dbx.Params{"id": id, "cookie": cookie, "query": e.Request.URL.RawQuery, "expires": time.Now().Add(10 * time.Minute).Unix()}).Execute()
	if err != nil {
		return err
	}
	http.SetCookie(e.Response, &http.Cookie{Name: "bb_oauth_consent", Value: cookie, Path: "/oauth", Secure: strings.HasPrefix(s.Origin, "https:"), HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: 600})
	nonce := security.RandomString(32)
	e.Response.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; style-src 'nonce-"+nonce+"'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
	return consentTemplate.Execute(e.Response, map[string]any{"Name": name, "Client": request.GetClient().GetID(), "Redirect": request.GetRedirectURI().String(), "Scopes": request.GetRequestedScopes(), "ID": id, "Nonce": nonce, "Collection": s.AuthCollection})
}

func (s *Server) consent(e *core.RequestEvent) error {
	if e.Request.Header.Get("Origin") != s.Origin || e.Auth == nil || e.Auth.IsSuperuser() || e.Auth.Collection().Id != s.AuthCollection {
		return e.UnauthorizedError("Sign in with an application user", nil)
	}
	var form struct {
		ID    string `json:"id"`
		Allow bool   `json:"allow"`
	}
	if err := e.BindBody(&form); err != nil {
		return e.BadRequestError("Invalid consent", nil)
	}
	cookie, err := e.Request.Cookie("bb_oauth_consent")
	if err != nil {
		return e.BadRequestError("Consent session expired", nil)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var redirect string
	err = s.App.RunInTransaction(func(tx core.App) error {
		ctx := context.WithValue(e.Request.Context(), txKey{}, &transaction{app: tx})
		var state struct {
			Cookie  string
			Query   string
			Expires int64
		}
		if err := tx.DB().NewQuery("SELECT cookie,query,expires FROM _bb_oauth_consent WHERE id={:id}").Bind(dbx.Params{"id": form.ID}).One(&state); err != nil {
			return errors.New("consent session expired")
		}
		if state.Expires < time.Now().Unix() || subtle.ConstantTimeCompare([]byte(state.Cookie), []byte(cookie.Value)) != 1 {
			return errors.New("invalid consent session")
		}
		request, err := s.request(ctx, state.Query)
		if err != nil {
			return err
		}
		recorder := httptest.NewRecorder()
		if !form.Allow {
			s.Provider.WriteAuthorizeError(ctx, recorder, request, fosite.ErrAccessDenied)
		} else {
			for _, scope := range request.GetRequestedScopes() {
				request.GrantScope(scope)
			}
			request.GrantAudience(s.Resource)
			session := &fosite.DefaultSession{Subject: e.Auth.Id, Extra: map[string]any{"collection": s.AuthCollection, "credential": credential(e.Auth)}}
			response, err := s.Provider.NewAuthorizeResponse(ctx, request, session)
			if err != nil {
				return err
			}
			if _, err = tx.DB().NewQuery("UPDATE _bb_oauth_clients SET authorized=1 WHERE id={:id}").Bind(dbx.Params{"id": request.GetClient().GetID()}).Execute(); err != nil {
				return err
			}
			s.Provider.WriteAuthorizeResponse(ctx, recorder, request, response)
		}
		redirect = recorder.Header().Get("Location")
		if redirect == "" {
			return errors.New("authorization response failed")
		}
		_, err = tx.DB().NewQuery("DELETE FROM _bb_oauth_consent WHERE id={:id}").Bind(dbx.Params{"id": form.ID}).Execute()
		return err
	})
	if err != nil {
		return e.BadRequestError("Authorization failed or consent expired", nil)
	}
	http.SetCookie(e.Response, &http.Cookie{Name: "bb_oauth_consent", Value: "", Path: "/oauth", MaxAge: -1, HttpOnly: true, Secure: strings.HasPrefix(s.Origin, "https:"), SameSite: http.SameSiteLaxMode})
	return e.JSON(200, map[string]string{"redirect": redirect})
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := r.ParseForm(); err != nil || len(r.PostForm["resource"]) != 1 || r.PostForm.Get("resource") != s.Resource {
		s.Provider.WriteAccessError(ctx, w, nil, fosite.ErrInvalidRequest)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var request fosite.AccessRequester
	var response fosite.AccessResponder
	var requestErr error
	err := s.App.RunInTransaction(func(tx core.App) error {
		state := &transaction{app: tx}
		txctx := context.WithValue(ctx, txKey{}, state)
		request, requestErr = s.Provider.NewAccessRequest(txctx, r, new(fosite.DefaultSession))
		// Replay detection deliberately commits revocation of the earlier grant.
		if requestErr != nil {
			if state.revoked {
				return nil
			}
			return requestErr
		}
		if _, requestErr = s.current(request.GetSession()); requestErr != nil {
			return requestErr
		}
		if !request.GetSession().GetExpiresAt(fosite.RefreshToken).IsZero() && request.GetSession().GetExpiresAt(fosite.RefreshToken).Before(time.Now()) {
			requestErr = fosite.ErrInvalidGrant
			return nil
		}
		var err error
		response, err = s.Provider.NewAccessResponse(txctx, request)
		return err
	})
	if err != nil {
		requestErr = err
	}
	if requestErr != nil {
		s.Provider.WriteAccessError(ctx, w, request, requestErr)
		return
	}
	s.Provider.WriteAccessResponse(ctx, w, request, response)
}
