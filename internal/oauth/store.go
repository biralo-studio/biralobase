package oauth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"time"

	"github.com/biralo-studio/biralobase/core"
	"github.com/ory/fosite"
	"github.com/pocketbase/dbx"
)

type txKey struct{}
type transaction struct {
	app     core.App
	revoked bool
}
type Store struct{ App core.App }

func (s *Store) db(ctx context.Context) dbx.Builder {
	if tx, ok := ctx.Value(txKey{}).(*transaction); ok {
		return tx.app.DB()
	}
	return s.App.DB()
}
func (s *Store) Init() error {
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS _bb_oauth_clients (id TEXT PRIMARY KEY, name TEXT NOT NULL, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS _bb_oauth_sessions (kind TEXT NOT NULL, signature TEXT NOT NULL, request_id TEXT NOT NULL, value TEXT NOT NULL, active INTEGER NOT NULL DEFAULT 1, PRIMARY KEY(kind,signature))`,
		`CREATE INDEX IF NOT EXISTS bb_oauth_request ON _bb_oauth_sessions(request_id)`,
		`CREATE TABLE IF NOT EXISTS _bb_oauth_consent (id TEXT PRIMARY KEY, cookie TEXT NOT NULL, query TEXT NOT NULL, expires INTEGER NOT NULL)`,
	} {
		if _, err := s.App.DB().NewQuery(query).Execute(); err != nil {
			return err
		}
	}
	// Databases created before registrations expired lack these columns. Their
	// existing clients are treated as authorized: they may hold live grants.
	var columns []struct {
		Name string `db:"name"`
	}
	if err := s.App.DB().NewQuery("PRAGMA table_info(_bb_oauth_clients)").All(&columns); err != nil {
		return err
	}
	present := map[string]bool{}
	for _, column := range columns {
		present[column.Name] = true
	}
	for _, column := range []struct{ name, definition string }{{"created", "INTEGER NOT NULL DEFAULT 0"}, {"authorized", "INTEGER NOT NULL DEFAULT 1"}} {
		if !present[column.name] {
			if _, err := s.App.DB().NewQuery("ALTER TABLE _bb_oauth_clients ADD COLUMN " + column.name + " " + column.definition).Execute(); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Store) GetClient(ctx context.Context, id string) (fosite.Client, error) {
	var raw string
	if err := s.db(ctx).NewQuery("SELECT value FROM _bb_oauth_clients WHERE id={:id}").Bind(dbx.Params{"id": id}).Row(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fosite.ErrNotFound
		}
		return nil, err
	}
	c := new(fosite.DefaultClient)
	return c, json.Unmarshal([]byte(raw), c)
}
func (s *Store) PutClient(ctx context.Context, name string, client *fosite.DefaultClient) error {
	raw, err := json.Marshal(client)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).NewQuery("INSERT INTO _bb_oauth_clients (id,name,value,created,authorized) VALUES ({:id},{:name},{:value},{:created},0)").Bind(dbx.Params{"id": client.ID, "name": name, "value": string(raw), "created": time.Now().Unix()}).Execute()
	return err
}
func (s *Store) ClientAssertionJWTValid(context.Context, string) error {
	return fosite.ErrRequestUnauthorized
}
func (s *Store) SetClientAssertionJWT(context.Context, string, time.Time) error {
	return fosite.ErrRequestUnauthorized
}

func (s *Store) put(ctx context.Context, kind, signature string, r fosite.Requester) error {
	// Never persist plaintext authorization codes, refresh tokens, credentials,
	// or bearer tokens carried in a token endpoint form.
	form := url.Values{}
	for _, key := range []string{"redirect_uri", "client_id", "code_challenge", "code_challenge_method", "resource", "response_type", "scope"} {
		if values := r.GetRequestForm()[key]; values != nil {
			form[key] = values
		}
	}
	request := &fosite.Request{ID: r.GetID(), RequestedAt: r.GetRequestedAt(), Client: r.GetClient(), Session: r.GetSession(), Form: form, RequestedScope: r.GetRequestedScopes(), GrantedScope: r.GetGrantedScopes(), RequestedAudience: r.GetRequestedAudience(), GrantedAudience: r.GetGrantedAudience()}
	raw, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_, err = s.db(ctx).NewQuery(`INSERT INTO _bb_oauth_sessions (kind,signature,request_id,value) VALUES ({:kind},{:signature},{:request},{:value})`).Bind(dbx.Params{"kind": kind, "signature": signature, "request": r.GetID(), "value": string(raw)}).Execute()
	return err
}
func (s *Store) get(ctx context.Context, kind, signature string) (fosite.Requester, error) {
	var row struct {
		Value  string
		Active bool
	}
	err := s.db(ctx).NewQuery(`SELECT value,active FROM _bb_oauth_sessions WHERE kind={:kind} AND signature={:signature}`).Bind(dbx.Params{"kind": kind, "signature": signature}).One(&row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fosite.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r := &fosite.Request{Client: new(fosite.DefaultClient), Session: new(fosite.DefaultSession)}
	if err = json.Unmarshal([]byte(row.Value), r); err != nil {
		return nil, err
	}
	if !row.Active {
		if kind == "code" {
			return r, fosite.ErrInvalidatedAuthorizeCode
		}
		return r, fosite.ErrInactiveToken
	}
	return r, nil
}
func (s *Store) invalidate(ctx context.Context, kind, signature string) error {
	_, err := s.db(ctx).NewQuery(`UPDATE _bb_oauth_sessions SET active=0 WHERE kind={:kind} AND signature={:signature}`).Bind(dbx.Params{"kind": kind, "signature": signature}).Execute()
	return err
}
func (s *Store) revoke(ctx context.Context, kind, request string) error {
	if tx, ok := ctx.Value(txKey{}).(*transaction); ok {
		tx.revoked = true
	}
	_, err := s.db(ctx).NewQuery(`UPDATE _bb_oauth_sessions SET active=0 WHERE kind={:kind} AND request_id={:request}`).Bind(dbx.Params{"kind": kind, "request": request}).Execute()
	return err
}
func (s *Store) CreateAuthorizeCodeSession(c context.Context, k string, r fosite.Requester) error {
	return s.put(c, "code", k, r)
}
func (s *Store) GetAuthorizeCodeSession(c context.Context, k string, _ fosite.Session) (fosite.Requester, error) {
	return s.get(c, "code", k)
}
func (s *Store) InvalidateAuthorizeCodeSession(c context.Context, k string) error {
	return s.invalidate(c, "code", k)
}
func (s *Store) CreateAccessTokenSession(c context.Context, k string, r fosite.Requester) error {
	return s.put(c, "access", k, r)
}
func (s *Store) GetAccessTokenSession(c context.Context, k string, _ fosite.Session) (fosite.Requester, error) {
	return s.get(c, "access", k)
}
func (s *Store) DeleteAccessTokenSession(c context.Context, k string) error {
	return s.invalidate(c, "access", k)
}
func (s *Store) CreateRefreshTokenSession(c context.Context, k, _ string, r fosite.Requester) error {
	return s.put(c, "refresh", k, r)
}
func (s *Store) GetRefreshTokenSession(c context.Context, k string, _ fosite.Session) (fosite.Requester, error) {
	return s.get(c, "refresh", k)
}
func (s *Store) DeleteRefreshTokenSession(c context.Context, k string) error {
	return s.invalidate(c, "refresh", k)
}
func (s *Store) RevokeAccessToken(c context.Context, id string) error {
	return s.revoke(c, "access", id)
}
func (s *Store) RevokeRefreshToken(c context.Context, id string) error {
	return s.revoke(c, "refresh", id)
}
func (s *Store) RotateRefreshToken(c context.Context, id, _ string) error {
	return errors.Join(s.revoke(c, "refresh", id), s.revoke(c, "access", id))
}
func (s *Store) CreatePKCERequestSession(c context.Context, k string, r fosite.Requester) error {
	return s.put(c, "pkce", k, r)
}
func (s *Store) GetPKCERequestSession(c context.Context, k string, _ fosite.Session) (fosite.Requester, error) {
	return s.get(c, "pkce", k)
}
func (s *Store) DeletePKCERequestSession(c context.Context, k string) error {
	return s.invalidate(c, "pkce", k)
}
