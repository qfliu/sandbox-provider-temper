package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"strconv"
	"time"
)

// portsPrefix is where a sandbox's ports are proxied:
// /v1/sandboxes/{id}/ports/{port}/<path> (D66).
func portsPrefix(id string, port int) string {
	return "/v1/sandboxes/" + id + "/ports/" + strconv.Itoa(port)
}

// GET (any method) /v1/sandboxes/{id}/ports/{port}/{path...}
//
// An HTTP reverse proxy to a port inside the sandbox, e.g. an app an agent
// started with `docker compose up` (D66). It needs the owner's (or an
// admin's) API key like every /v1 call; the key and any cookies are removed
// before the request reaches the sandbox, which is untrusted. Responses are
// passed back with their cookies removed and a CSP sandbox header, so a page
// from the sandbox can't act as this origin (the dashboard) in a browser.
// WebSocket upgrades work (httputil.ReverseProxy handles them).
func handlePort(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		port, err := strconv.Atoi(r.PathValue("port"))
		if err != nil {
			writeError(w, http.StatusBadRequest, "port must be a number")
			return
		}
		dial, release, err := svc.OpenPort(r.Context(), userFrom(r.Context()), id, port)
		if err != nil {
			writeServiceError(log, w, err)
			return
		}
		defer release()

		prefix := portsPrefix(id, port)
		proxy := &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL.Scheme = "http"
				pr.Out.URL.Host = net.JoinHostPort("localhost", strconv.Itoa(port))
				pr.Out.Host = pr.Out.URL.Host
				pr.Out.URL.Path = "/" + pr.In.PathValue("path")
				pr.Out.URL.RawPath = ""
				// Never hand the caller's credentials to the sandbox.
				pr.Out.Header.Del("Authorization")
				pr.Out.Header.Del("Proxy-Authorization")
				pr.Out.Header.Del("Cookie")
				pr.SetXForwarded()
				pr.Out.Header.Set("X-Forwarded-Prefix", prefix)
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					return dial(ctx)
				},
				// One connection per request: it lives on this request's
				// SSH connection, which is released when the request ends.
				DisableKeepAlives:     true,
				ResponseHeaderTimeout: 5 * time.Minute,
			},
			ModifyResponse: func(resp *http.Response) error {
				resp.Header.Del("Set-Cookie")
				resp.Header.Set("Content-Security-Policy", "sandbox")
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				log.Info("sandbox port unreachable", "sandbox", id, "port", port, "err", err)
				writeError(w, http.StatusBadGateway,
					"nothing answered on port "+strconv.Itoa(port)+" inside the sandbox (is a server listening on 127.0.0.1 or 0.0.0.0?)")
			},
		}
		proxy.ServeHTTP(w, r)
	}
}

// GET /v1/sandboxes/{id}/ports/{port} → the same with a trailing slash, so
// relative links in the app's pages resolve under the prefix.
func handlePortRoot(w http.ResponseWriter, r *http.Request) {
	target := r.URL.Path + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}
