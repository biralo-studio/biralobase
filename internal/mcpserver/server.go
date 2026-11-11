// Package mcpserver generates explicitly opted-in, user-scoped database tools.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/biralo-studio/biralobase/internal/oauth"
	"github.com/biralo-studio/biralobase/internal/recordapi"
	"github.com/biralo-studio/biralobase/internal/schema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func Scope(id, action string) string { return "bb:" + id + ":" + action }
func Scopes(s schema.Schema) []string {
	result := []string{}
	for id, exposure := range s.MCP {
		for _, action := range exposure.Actions {
			result = append(result, Scope(id, action))
		}
	}
	sort.Strings(result)
	return result
}

type principalKey struct{}

func New(auth *oauth.Server, desired schema.Schema, api http.Handler) (http.Handler, error) {
	if err := desired.Validate(); err != nil {
		return nil, err
	}
	stream := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		principal := r.Context().Value(principalKey{}).(oauth.Principal)
		server := mcp.NewServer(&mcp.Implementation{Name: "biralobase", Version: "1"}, nil)
		for id, exposure := range desired.MCP {
			for _, action := range exposure.Actions {
				if !slices.Contains(principal.Scopes, Scope(id, action)) {
					continue
				}
				name := exposure.Name
				if name == "" {
					name = id
				}
				name += "_" + action
				destructive, openWorld := action == "delete" || action == "update", false
				server.AddTool(&mcp.Tool{Name: name, Description: exposure.Description + " (" + action + ")", InputSchema: inputSchema(action, exposure.Fields), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: action == "get" || action == "list", DestructiveHint: &destructive, OpenWorldHint: &openWorld}}, func(ctx context.Context, call *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					result, err := execute(ctx, api, principal, id, action, exposure.Fields, call.Params.Arguments, r.RemoteAddr)
					if err != nil {
						return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil
					}
					b, err := json.Marshal(result)
					if err != nil {
						return nil, err
					}
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}, nil
				})
			}
		}
		return server
	}, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, MaxRequestBodyBytes: 1 << 20})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if origin := r.Header.Get("Origin"); origin != "" && origin != auth.Origin {
			http.Error(w, "Untrusted origin", http.StatusForbidden)
			return
		}
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			challenge(w, auth, false)
			return
		}
		principal, err := auth.Authenticate(r.Context(), parts[1])
		if err != nil {
			challenge(w, auth, true)
			return
		}
		stream.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, principal)))
	}), nil
}
func challenge(w http.ResponseWriter, s *oauth.Server, invalid bool) {
	value := `Bearer resource_metadata="` + s.Origin + `/.well-known/oauth-protected-resource/mcp", scope="` + strings.Join(s.Scopes, " ") + `"`
	if invalid {
		value += `, error="invalid_token"`
	}
	w.Header().Set("WWW-Authenticate", value)
	http.Error(w, "OAuth access token required", http.StatusUnauthorized)
}

func inputSchema(action string, fields []string) map[string]any {
	properties := map[string]any{}
	required := []string{}
	if action == "get" || action == "update" || action == "delete" {
		properties["id"] = map[string]any{"type": "string", "minLength": 1}
		required = append(required, "id")
	}
	if action == "list" {
		properties["page"] = map[string]any{"type": "integer", "minimum": 1}
		properties["perPage"] = map[string]any{"type": "integer", "minimum": 1, "maximum": 100}
	}
	if action == "create" || action == "update" {
		data := map[string]any{}
		for _, field := range fields {
			data[field] = map[string]any{}
		}
		properties["data"] = map[string]any{"type": "object", "properties": data, "additionalProperties": false}
		required = append(required, "data")
	}
	return map[string]any{"type": "object", "properties": properties, "additionalProperties": false, "required": required}
}
func execute(ctx context.Context, api http.Handler, p oauth.Principal, id, action string, fields []string, raw json.RawMessage, remote string) (any, error) {
	if !slices.Contains(p.Scopes, Scope(id, action)) {
		return nil, errors.New("insufficient_scope")
	}
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	var args struct {
		ID      string         `json:"id"`
		Page    int            `json:"page"`
		PerPage int            `json:"perPage"`
		Data    map[string]any `json:"data"`
	}
	if schema.Decode(raw, &args) != nil {
		return nil, errors.New("invalid tool arguments")
	}
	var supplied map[string]any
	if json.Unmarshal(raw, &supplied) != nil || supplied == nil {
		return nil, errors.New("arguments must be an object")
	}
	allowed := inputSchema(action, fields)["properties"].(map[string]any)
	for key := range supplied {
		if _, ok := allowed[key]; !ok {
			return nil, fmt.Errorf("unexpected argument %q", key)
		}
	}
	path := "/api/collections/" + url.PathEscape(id) + "/records"
	method := "GET"
	var body any
	if action == "get" || action == "update" || action == "delete" {
		if args.ID == "" {
			return nil, errors.New("id is required")
		}
		path += "/" + url.PathEscape(args.ID)
	}
	switch action {
	case "list":
		if args.Page == 0 {
			args.Page = 1
		}
		if args.PerPage == 0 {
			args.PerPage = 30
		}
		if args.Page < 1 || args.PerPage < 1 || args.PerPage > 100 {
			return nil, errors.New("invalid pagination")
		}
		path += "?page=" + strconv.Itoa(args.Page) + "&perPage=" + strconv.Itoa(args.PerPage)
	case "create", "update":
		if args.Data == nil {
			return nil, errors.New("data is required")
		}
		for key := range args.Data {
			if !slices.Contains(fields, key) {
				return nil, fmt.Errorf("field %q is not exposed", key)
			}
		}
		body = args.Data
		method = "POST"
		if action == "update" {
			method = "PATCH"
		}
	case "delete":
		method = "DELETE"
	case "get":
	default:
		return nil, errors.New("unsupported action")
	}
	token, err := p.User.NewAuthToken()
	if err != nil {
		return nil, err
	}
	client := recordapi.New(api, ctx, token, remote)
	response, err := client.Fetch(method, path, body)
	if err != nil {
		return nil, err
	}
	if response.Status >= 400 {
		return nil, fmt.Errorf("record operation rejected (HTTP %d)", response.Status)
	}
	if action == "delete" {
		return map[string]bool{"deleted": true}, nil
	}
	if action == "list" {
		page, ok := response.Body.(map[string]any)
		if !ok {
			return nil, errors.New("invalid record response")
		}
		items, ok := page["items"].([]any)
		if !ok {
			return nil, errors.New("invalid list response")
		}
		for i, item := range items {
			items[i] = project(item, fields)
		}
		return map[string]any{"page": page["page"], "perPage": page["perPage"], "totalItems": page["totalItems"], "totalPages": page["totalPages"], "items": items}, nil
	}
	return project(response.Body, fields), nil
}
func project(value any, fields []string) map[string]any {
	result := map[string]any{}
	if record, ok := value.(map[string]any); ok {
		for _, field := range fields {
			if value, ok := record[field]; ok {
				result[field] = value
			}
		}
	}
	return result
}
