// Package recordapi delegates user operations through the unchanged PocketBase
// record request pipeline, including auth, validation, rules, and request hooks.
package recordapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
)

var recordPath = regexp.MustCompile(`^/api/collections/[a-zA-Z0-9_]+/records(?:/[a-zA-Z0-9_]+)?$`)

type Client struct {
	handler    http.Handler
	context    context.Context
	token      string
	remoteAddr string
}

func New(handler http.Handler, ctx context.Context, token, remoteAddr string) *Client {
	return &Client{handler: handler, context: ctx, token: token, remoteAddr: remoteAddr}
}

type Response struct {
	Status int `json:"status"`
	Body   any `json:"body"`
}

func (c *Client) Fetch(method, path string, body any) (Response, error) {
	var result Response
	u, err := url.Parse(path)
	if err != nil || u.IsAbs() || u.Host != "" || u.Fragment != "" || !recordPath.MatchString(u.Path) {
		return result, errors.New("user API only accepts local collection record paths")
	}
	switch method {
	case "GET", "POST", "PATCH", "DELETE":
	default:
		return result, errors.New("unsupported user API method")
	}
	var raw []byte
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return result, err
		}
	}
	request, err := http.NewRequestWithContext(c.context, method, u.String(), bytes.NewReader(raw))
	if err != nil {
		return result, err
	}
	request.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		request.Header.Set("Authorization", c.token)
	}
	request.RemoteAddr = c.remoteAddr
	recorder := httptest.NewRecorder()
	c.handler.ServeHTTP(recorder, request)
	result.Status = recorder.Code
	if recorder.Body.Len() > 8<<20 {
		return result, errors.New("user API response exceeds 8 MiB; request fewer records or fields")
	}
	if recorder.Body.Len() > 0 {
		if err = json.Unmarshal(recorder.Body.Bytes(), &result.Body); err != nil {
			return result, errors.New("user API returned a non-JSON response")
		}
	}
	return result, nil
}
