package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"
)

// hopByHopHeaders are stripped from every outbound request per RFC 9110
// §7.6.1, on both passthrough and non-passthrough routes.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "TE", "Trailer", "Transfer-Encoding",
	"Upgrade", "Proxy-Authenticate", "Proxy-Authorization",
}

// tokenLeakMarker is the substring a subscription OAuth bearer always
// contains. The defensive guard in §3 scans every outbound header value on
// non-passthrough routes for it, independent of and in addition to the
// explicit Authorization/x-api-key overwrite, on purpose: the overwrite
// alone is not enough if a future edit reorders it.
const tokenLeakMarker = "sk-ant-"

// ctxKey is a private context-key type so this package's context values
// never collide with another package's.
type ctxKey int

const routeCtxKey ctxKey = 1

// bodyModelOnly is used to read just the "model" field out of an inbound
// request body without touching anything else in it.
type bodyModelOnly struct {
	Model string `json:"model"`
}

// server holds everything a request handler needs: the resolved config, a
// shared ReverseProxy configured once with FlushInterval: -1 (streaming
// hangs otherwise), and the debug-headers flag.
type server struct {
	cfg          *config
	debugHeaders bool
	proxy        *httputil.ReverseProxy
	cooldowns    *cooldownStore
	history      *fallbackHistory
}

func newServer(cfg *config, debugHeaders bool) *server {
	s := &server{cfg: cfg, debugHeaders: debugHeaders, cooldowns: newCooldownStore(), history: newFallbackHistory()}
	s.proxy = &httputil.ReverseProxy{
		FlushInterval: -1, // flush on every write; anything else buffers SSE and the CLI appears to hang.
		Rewrite: func(pr *httputil.ProxyRequest) {
			rt, _ := pr.In.Context().Value(routeCtxKey).(route)
			pr.SetURL(rt.upstreamURL)
			applyHeaderPolicy(pr.Out.Header, rt, s.debugHeaders)
		},
		// ModifyResponse/ErrorHandler together implement the §9 fallback
		// chain: ModifyResponse runs before any byte reaches the client and
		// decides whether this status should trigger a retry;
		// ErrorHandler performs the retry (or gives up). See fallback.go.
		ModifyResponse: s.modifyResponse,
		ErrorHandler:   s.errorHandlerFallback,
	}
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()

	if r.Method == http.MethodGet && r.URL.Path == statusPath {
		s.serveStatus(w, r)
		return
	}

	limit := s.cfg.MaxBodyBytes
	buf, overflowed, err := readBodyLimited(r.Body, limit)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "submux: failed to read request body")
		return
	}
	if overflowed {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "submux: request body exceeds max_body_bytes")
		return
	}

	// A body with no "model" field, or one that is not JSON, falls through
	// to the "*" route rather than erroring (§4).
	var bm bodyModelOnly
	_ = json.Unmarshal(buf, &bm) // best-effort; leaves bm.Model == "" on any failure

	rt, matched := matchRoute(s.cfg.Routes, bm.Model)
	if !matched {
		writeAnthropicError(w, http.StatusBadGateway, "invalid_request_error", "submux: no route matched and no \"*\" fallback is configured")
		return
	}

	outBody := buf
	if rt.modelRewrite != nil {
		if newModel, ok := rt.modelRewrite[bm.Model]; ok {
			rewritten, err := rewriteModelInBody(buf, newModel)
			if err != nil {
				writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "submux: model_rewrite failed: body is not valid JSON")
				return
			}
			outBody = rewritten
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(outBody))
	r.ContentLength = int64(len(outBody))

	ac := &attemptCtx{
		requestedModel: bm.Model,
		originalBody:   buf,
		chain:          rt.fallbackChain(s.cfg),
		tried:          map[string]bool{bm.Model: true},
		attempted:      []string{bm.Model},
		isAgent:        r.Header.Get("x-claude-code-agent-id") != "",
	}
	ctx := context.WithValue(r.Context(), routeCtxKey, rt)
	ctx = context.WithValue(ctx, attemptCtxKey, ac)
	r = r.WithContext(ctx)

	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}

	// §9.1: while the originally requested id is cooling, skip its upstream
	// entirely and go straight down the fallback chain, without ever
	// dispatching a real request to it.
	if until, cooling := s.cooldowns.isCooling(bm.Model, time.Now()); cooling {
		s.retryOrExhaust(ac, rec, r, bm.Model, until, 0, nil, nil, 0)
	} else {
		s.proxy.ServeHTTP(rec, r)
	}

	log.Printf("model=%q match=%q upstream=%s auth=%s status=%d bytes=%d duration=%s agent=%v",
		bm.Model, rt.match, rt.upstreamURL.Host, authModeLabel(rt), rec.status, rec.bytes, time.Since(start), ac.isAgent)
}

// readBodyLimited reads up to limit+1 bytes; if that read produced more
// than limit bytes, overflowed is true and buf should be discarded.
func readBodyLimited(r io.Reader, limit int64) (buf []byte, overflowed bool, err error) {
	lr := io.LimitReader(r, limit+1)
	buf, err = io.ReadAll(lr)
	if err != nil {
		return nil, false, err
	}
	if int64(len(buf)) > limit {
		return nil, true, nil
	}
	return buf, false, nil
}

// rewriteModelInBody is the ONLY place a passthrough body is ever
// re-serialized, and only for a route that declares model_rewrite. Every
// other route forwards the original bytes untouched.
func rewriteModelInBody(body []byte, newModel string) ([]byte, error) {
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(newModel)
	if err != nil {
		return nil, err
	}
	generic["model"] = encoded
	return json.Marshal(generic)
}

// applyHeaderPolicy is the security core of this tool (§3). It mutates hdr
// in place, in the following order:
//  1. passthrough: leave Authorization/x-api-key/anthropic-*/user-agent/
//     x-app exactly as received.
//     non-passthrough: DELETE the inbound Authorization and x-api-key,
//     then set the configured credential. A subscription bearer must never
//     reach a third-party upstream.
//  2. non-passthrough only: strip oauth-* entries from anthropic-beta.
//  3. non-passthrough only: the defensive sk-ant- guard, independent of (1).
//  4. both: strip hop-by-hop headers.
func applyHeaderPolicy(hdr http.Header, rt route, debugHeaders bool) {
	if rt.authKind == "passthrough" {
		stripHopByHop(hdr)
		if debugHeaders {
			logDebugHeaders(hdr)
		}
		return
	}

	hdr.Del("Authorization")
	hdr.Del("X-Api-Key")

	switch rt.authKind {
	case "bearer":
		hdr.Set("Authorization", "Bearer "+rt.authValue)
	case "x-api-key":
		hdr.Set("x-api-key", rt.authValue)
	case "none":
		// send no credential
	}

	if beta := hdr.Get("anthropic-beta"); beta != "" {
		hdr.Set("anthropic-beta", stripOAuthBetaEntries(beta))
		if hdr.Get("anthropic-beta") == "" {
			hdr.Del("anthropic-beta")
		}
	}

	dropHeadersContaining(hdr, tokenLeakMarker)
	stripHopByHop(hdr)

	if debugHeaders {
		logDebugHeaders(hdr)
	}
}

// stripOAuthBetaEntries removes any comma-separated entry that starts with
// "oauth-" from an anthropic-beta header value; a non-Anthropic upstream
// has no use for it and some upstreams 400 on it.
func stripOAuthBetaEntries(beta string) string {
	parts := strings.Split(beta, ",")
	kept := parts[:0]
	for _, p := range parts {
		if strings.HasPrefix(strings.TrimSpace(p), "oauth-") {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, ",")
}

// dropHeadersContaining deletes any header whose value contains marker,
// logging one redacted warning per dropped header (never the value).
func dropHeadersContaining(hdr http.Header, marker string) {
	for key, values := range hdr {
		for _, v := range values {
			if strings.Contains(v, marker) {
				hdr.Del(key)
				log.Printf("submux: WARNING dropped outbound header %q on non-passthrough route: value contained a subscription token marker", key)
				break
			}
		}
	}
}

func stripHopByHop(hdr http.Header) {
	for _, h := range hopByHopHeaders {
		hdr.Del(h)
	}
}

// logDebugHeaders prints header NAMES only, and for Authorization the
// scheme, first 8 characters, and length -- never the full value.
func logDebugHeaders(hdr http.Header) {
	names := make([]string, 0, len(hdr))
	for k := range hdr {
		names = append(names, k)
	}
	summary := fmt.Sprintf("submux: debug-headers names=%v", names)
	if auth := hdr.Get("Authorization"); auth != "" {
		scheme, rest, _ := strings.Cut(auth, " ")
		prefix := rest
		if len(prefix) > 8 {
			prefix = prefix[:8]
		}
		summary += fmt.Sprintf(" authorization_scheme=%s authorization_prefix=%s authorization_len=%d", scheme, prefix, len(rest))
	}
	log.Print(summary)
}

func authModeLabel(rt route) string {
	switch rt.authKind {
	case "passthrough":
		return "passthrough"
	case "none":
		return "none"
	default:
		return fmt.Sprintf("%s(%s)", rt.authKind, rt.authSource)
	}
}

// statusRecorder captures the response status and byte count for the
// per-request log line, while remaining a valid http.Flusher passthrough
// so streaming (FlushInterval: -1) still works through it.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
	_, _ = w.Write(body)
}
