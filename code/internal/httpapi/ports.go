package httpapi

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// portsPrefix is where a sandbox's ports are proxied:
// /v1/sandboxes/{id}/ports/{port}/<path> (D66).
func portsPrefix(id string, port int) string {
	return "/v1/sandboxes/" + id + "/ports/" + strconv.Itoa(port)
}

// Response headers a sandbox may not send through this origin, which also
// serves the dashboard (review of #77): cookies, and headers that act on the
// whole origin or could pass for the API's own (WWW-Authenticate would look
// like our key was refused).
var droppedPortHeaders = []string{
	"Set-Cookie", "Set-Cookie2", "Clear-Site-Data", "Strict-Transport-Security",
	"Alt-Svc", "WWW-Authenticate", "Access-Control-Allow-Origin",
	"Access-Control-Allow-Credentials", "Access-Control-Allow-Headers",
	"Access-Control-Allow-Methods", "Access-Control-Expose-Headers", "Access-Control-Max-Age",
}

// maxPortResponseHeaders bounds the response headers read from a sandbox
// (net/http's default is 10 MiB per response, held in the API's memory).
const maxPortResponseHeaders = 64 << 10

// GET (any method) /v1/sandboxes/{id}/ports/{port}/{path...}
//
// An HTTP reverse proxy to a port inside the sandbox, e.g. an app an agent
// started with `docker compose up` (D66). It needs the owner's (or an
// admin's) API key like every /v1 call; the key and any cookies are removed
// before the request reaches the sandbox, which is untrusted. Responses come
// back without cookies or other origin-wide headers (1xx ones aren't passed
// on at all), with a CSP sandbox header, nosniff, and X-Sandbox-Port saying
// they came from the sandbox, so a page from the sandbox can't act as this
// origin (the dashboard) in a browser. WebSocket upgrades work
// (httputil.ReverseProxy handles them).
func handlePort(log *slog.Logger, svc SandboxService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, rawPort := r.PathValue("id"), r.PathValue("port")
		port, err := strconv.Atoi(rawPort)
		if err != nil || strconv.Itoa(port) != rawPort { // no "+80" or "0080"
			writeError(w, http.StatusBadRequest, "port must be a number between 1 and 65535")
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
				// The rest of the path as the client encoded it (an
				// escaped "/" stays escaped).
				rest := strings.TrimPrefix(pr.In.URL.EscapedPath(), prefix)
				if !strings.HasPrefix(rest, "/") {
					rest = "/" + rest
				}
				pr.Out.URL.RawPath = rest
				pr.Out.URL.Path, _ = url.PathUnescape(rest)
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
				DisableKeepAlives:      true,
				ResponseHeaderTimeout:  5 * time.Minute,
				MaxResponseHeaderBytes: maxPortResponseHeaders,
			},
			ModifyResponse: func(resp *http.Response) error {
				for _, h := range droppedPortHeaders {
					resp.Header.Del(h)
				}
				// A redirect to "/x" means the app's own /x: keep it under
				// the prefix.
				if loc := resp.Header.Get("Location"); strings.HasPrefix(loc, "/") && !strings.HasPrefix(loc, "//") {
					resp.Header.Set("Location", prefix+loc)
				}
				resp.Header.Set("Content-Security-Policy", "sandbox")
				resp.Header.Set("X-Content-Type-Options", "nosniff")
				resp.Header.Set("X-Sandbox-Port", strconv.Itoa(port))
				return nil
			},
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				log.Info("sandbox port unreachable", "sandbox", id, "port", port, "err", err)
				writeError(w, http.StatusBadGateway,
					"nothing answered on port "+strconv.Itoa(port)+" inside the sandbox (is a server listening on 127.0.0.1 or 0.0.0.0?)")
			},
		}
		proxy.ServeHTTP(no1xx{w}, r)
	}
}

// no1xx drops informational (1xx) responses other than 101: ReverseProxy
// passes them on without ModifyResponse, so their headers (a Set-Cookie in
// a 103) would skip the filter above (review of #77). ReverseProxy copies
// a 1xx's headers into Header() before WriteHeader and clears them after,
// so skipping WriteHeader leaves nothing behind.
type no1xx struct{ http.ResponseWriter }

func (w no1xx) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

// Hijack (WebSockets), Flush (streaming) and Unwrap pass through: ReverseProxy
// asserts these interfaces on the writer it gets.
func (w no1xx) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(w.ResponseWriter).Hijack()
}
func (w no1xx) Flush()                      { _ = http.NewResponseController(w.ResponseWriter).Flush() }
func (w no1xx) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// GET /v1/sandboxes/{id}/ports/{port} → the same with a trailing slash, so
// relative links in the app's pages resolve under the prefix.
func handlePortRoot(w http.ResponseWriter, r *http.Request) {
	target := r.URL.EscapedPath() + "/"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}
