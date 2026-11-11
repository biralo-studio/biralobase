package functions

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/biralo-studio/biralobase/core"
	"github.com/biralo-studio/biralobase/internal/recordapi"
	"github.com/biralo-studio/biralobase/tools/router"
)

func (w *Worker) BindHTTP(routes *router.Router[*core.RequestEvent], api http.Handler) {
	for _, name := range w.Definition.Functions {
		routes.Any("/api/functions/"+name, func(e *core.RequestEvent) error {
			body, err := io.ReadAll(io.LimitReader(e.Request.Body, (1<<20)+1))
			if err != nil || len(body) > 1<<20 {
				return e.BadRequestError("Function body exceeds 1 MiB", nil)
			}
			var requestBody any
			if len(body) > 0 && json.Unmarshal(body, &requestBody) != nil {
				return e.BadRequestError("Function body must be JSON", nil)
			}
			var auth any
			if e.Auth != nil {
				auth = e.Auth.PublicExport()
			}
			// Credentials stay in Go; public request headers exclude authorization
			// and cookies. The caller's identity is forwarded by ctx.api.
			headers := e.Request.Header.Clone()
			headers.Del("Authorization")
			headers.Del("Cookie")
			value, err := w.Invoke(e.Request.Context(), "function", name, map[string]any{
				"request": map[string]any{"method": e.Request.Method, "url": e.Request.URL.String(), "headers": headers, "body": requestBody},
				"auth":    auth, "admin": e.App,
				"api": recordapi.New(api, e.Request.Context(), e.Request.Header.Get("Authorization"), recordapi.PeerAddress(e.App, e.Request)),
			})
			if err != nil {
				return err
			}
			var response struct {
				Status  int               `json:"status"`
				Headers map[string]string `json:"headers"`
				Body    any               `json:"body"`
			}
			if json.Unmarshal(value, &response) != nil {
				return e.InternalServerError("Invalid function response", nil)
			}
			if response.Status == 0 {
				response.Status = http.StatusOK
			}
			if response.Status < 200 || response.Status > 599 {
				return e.InternalServerError("Invalid function response status", nil)
			}
			for key, value := range response.Headers {
				lower := strings.ToLower(key)
				if lower == "connection" || lower == "transfer-encoding" || lower == "content-length" || lower == "upgrade" || strings.ContainsAny(key+value, "\r\n") {
					return e.InternalServerError("Invalid function response header", nil)
				}
				e.Response.Header().Set(key, value)
			}
			return e.JSON(response.Status, response.Body)
		})
	}
}
