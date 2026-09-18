package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// attemptCtxKey stores the *attemptCtx for the whole lifetime of one inbound
// request, including every retry down its fallback chain.
const attemptCtxKey ctxKey = 2

// statusPath is the admin endpoint `submux status` polls. It is served by
// server.ServeHTTP directly, before any proxy/fallback logic, so it never
// collides with a real Anthropic API path (those all live under /v1/...).
const statusPath = "/__submux/status"

// errFallbackTrigger is the sentinel ModifyResponse returns to force
// httputil.ReverseProxy to call ErrorHandler instead of writing the
// triggering response to the client. Go's ReverseProxy only calls
// ErrorHandler BEFORE any response header has been written -- once
// WriteHeader has fired (i.e. after the first byte reaches the client), a
// later body-copy failure is just logged, never routed back through
// ErrorHandler. That is what makes the §9.2.3 "no retry after the first
// byte" guarantee hold for free here, instead of needing to be
// hand-enforced.
var errFallbackTrigger = errors.New("submux: fallback trigger")

// attemptCtx is the mutable, request-scoped state threaded through every
// attempt of one inbound request via context, so a fallback retry (which is
// a fresh call to s.proxy.ServeHTTP) can see what has already happened.
type attemptCtx struct {
	requestedModel string
	originalBody   []byte
	chain          []string
	tried          map[string]bool
	attempted      []string // ids actually dispatched upstream, in order
	isAgent        bool

	// populated by modifyResponse when the most recent attempt hit a
	// fallback_status_codes status; consumed by errorHandlerFallback.
	lastTriggerStatus  int
	lastTriggerHeader  http.Header
	lastTriggerBody    []byte
	lastRetryAfterSecs int
	lastCooldownUntil  time.Time
}

// cooldownStore tracks model ids that recently triggered a fallback status,
// in memory only, per §9.1 ("no persistence").
type cooldownStore struct {
	mu    sync.Mutex
	until map[string]time.Time
}

func newCooldownStore() *cooldownStore {
	return &cooldownStore{until: map[string]time.Time{}}
}

func (c *cooldownStore) markCooling(id string, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until[id] = until
}

func (c *cooldownStore) isCooling(id string, now time.Time) (time.Time, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.until[id]
	if !ok || !now.Before(until) {
		return time.Time{}, false
	}
	return until, true
}

func (c *cooldownStore) snapshot(now time.Time) map[string]time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]time.Time)
	for id, until := range c.until {
		if now.Before(until) {
			out[id] = until
		}
	}
	return out
}

// fallbackEvent is one row of the last-20 history `submux status` shows.
type fallbackEvent struct {
	At        time.Time
	Requested string
	Attempted []string
	Outcome   string // "fell back" | "chain exhausted"
}

// fallbackHistory keeps the last 20 fallback events, in memory only.
type fallbackHistory struct {
	mu     sync.Mutex
	events []fallbackEvent
}

func newFallbackHistory() *fallbackHistory {
	return &fallbackHistory{}
}

func (h *fallbackHistory) record(ev fallbackEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, ev)
	if len(h.events) > 20 {
		h.events = h.events[len(h.events)-20:]
	}
}

func (h *fallbackHistory) snapshot() []fallbackEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]fallbackEvent, len(h.events))
	copy(out, h.events)
	return out
}

// isFallbackStatus reports whether status is one of the configured
// fallback-triggering statuses.
func isFallbackStatus(status int, codes []int) bool {
	for _, c := range codes {
		if c == status {
			return true
		}
	}
	return false
}

// parseRetryAfterOrDefault parses a Retry-After header value, which per RFC
// 9110 §10.2.3 is either a delta-seconds integer or an HTTP-date. An empty
// or unparseable value falls back to def.
func parseRetryAfterOrDefault(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return def
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		return 0
	}
	return def
}

// composeFallbackBody re-marshals the ORIGINAL inbound body with model
// substituted for modelID, then applies the target route's own
// model_rewrite (if any) on top -- one place (the route table) still
// defines how a model is reached, even for a fallback hop (§9.2.1). This
// re-marshal is allowed only here, never on a request's first attempt
// (§9.2.5, §3): the caller is responsible for using the original bytes
// untouched on the first attempt.
func composeFallbackBody(original []byte, modelID string, rt route) ([]byte, error) {
	body, err := rewriteModelInBody(original, modelID)
	if err != nil {
		return nil, err
	}
	if rt.modelRewrite != nil {
		if newModel, ok := rt.modelRewrite[modelID]; ok {
			body, err = rewriteModelInBody(body, newModel)
			if err != nil {
				return nil, err
			}
		}
	}
	return body, nil
}

// pickNext scans ac.chain in order for the first candidate that has not
// already been tried this request (§9.2.2, the no-loop guard, covers a
// chain entry that repeats the originally requested id) and is not
// currently cooling (§9.1: "skips the upstream entirely and goes straight
// down the chain"). A candidate with no matching route is likewise skipped
// rather than erroring, since a misconfigured chain entry should not sink
// the whole request.
func (s *server) pickNext(ac *attemptCtx) (string, route, bool) {
	now := time.Now()
	for _, id := range ac.chain {
		if ac.tried[id] {
			continue
		}
		if _, cooling := s.cooldowns.isCooling(id, now); cooling {
			ac.tried[id] = true
			continue
		}
		rt, ok := matchRoute(s.cfg.Routes, id)
		if !ok {
			ac.tried[id] = true
			continue
		}
		return id, rt, true
	}
	return "", route{}, false
}

// attempt dispatches ONE fallback hop for modelID down rt, recording it in
// ac. It is only ever used for the SECOND and later attempts of a request:
// the first attempt is dispatched directly by ServeHTTP, which must keep
// the original inbound bytes untouched on a passthrough route (§3) and so
// never re-marshals via composeFallbackBody.
func (s *server) attempt(ac *attemptCtx, w http.ResponseWriter, r *http.Request, modelID string, rt route) {
	ac.tried[modelID] = true
	ac.attempted = append(ac.attempted, modelID)

	// This re-marshal is allowed ONLY on a fallback attempt (§9.2.5); the
	// first attempt never reaches this function.
	body, err := composeFallbackBody(ac.originalBody, modelID, rt)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "invalid_request_error", "submux: fallback re-marshal failed")
		return
	}

	ctx := context.WithValue(r.Context(), routeCtxKey, rt)
	ctx = context.WithValue(ctx, attemptCtxKey, ac)
	newReq := r.Clone(ctx)
	newReq.Body = io.NopCloser(bytes.NewReader(body))
	newReq.ContentLength = int64(len(body))

	s.proxy.ServeHTTP(w, newReq)
}

// modifyResponse is httputil.ReverseProxy's ModifyResponse hook. It runs
// after headers arrive from upstream but BEFORE anything is written to the
// client, which is exactly the window in which a fallback decision must be
// made (§9.2.3).
func (s *server) modifyResponse(resp *http.Response) error {
	ac, _ := resp.Request.Context().Value(attemptCtxKey).(*attemptCtx)
	if ac == nil || len(ac.attempted) == 0 {
		return nil
	}
	currentModel := ac.attempted[len(ac.attempted)-1]

	if isFallbackStatus(resp.StatusCode, s.cfg.FallbackStatusCodes) {
		cooldown := parseRetryAfterOrDefault(resp.Header.Get("Retry-After"), s.cfg.CooldownDefault)
		until := time.Now().Add(cooldown)
		s.cooldowns.markCooling(currentModel, until)

		bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()

		ac.lastTriggerStatus = resp.StatusCode
		ac.lastTriggerHeader = resp.Header.Clone()
		ac.lastTriggerBody = bodyBytes
		ac.lastRetryAfterSecs = int(cooldown.Seconds())
		ac.lastCooldownUntil = until

		return errFallbackTrigger
	}

	if len(ac.attempted) > 1 {
		resp.Header.Set("x-submux-fallback", strings.Join(ac.attempted, " -> "))
		if err := rewriteResponseModel(resp, currentModel); err != nil {
			return err
		}
		s.history.record(fallbackEvent{
			At:        time.Now(),
			Requested: ac.requestedModel,
			Attempted: append([]string(nil), ac.attempted...),
			Outcome:   "fell back",
		})
		log.Printf("submux: request answered by %q after falling back from %q (chain: %s)",
			currentModel, ac.requestedModel, strings.Join(ac.attempted, " -> "))
	}
	return nil
}

// errorHandlerFallback is httputil.ReverseProxy's ErrorHandler hook. It
// fires either for a genuine transport failure (RoundTrip error -- treated
// as a hard 502, never retried, since only an HTTP status in
// fallback_status_codes is a defined trigger per §9.2.3) or for our
// errFallbackTrigger sentinel from modifyResponse, in which case it drives
// the retry-or-exhaust decision.
func (s *server) errorHandlerFallback(w http.ResponseWriter, r *http.Request, err error) {
	ac, _ := r.Context().Value(attemptCtxKey).(*attemptCtx)
	if ac == nil || !errors.Is(err, errFallbackTrigger) {
		writeAnthropicError(w, http.StatusBadGateway, "upstream_error", "submux: upstream request failed")
		return
	}
	failedModel := ac.attempted[len(ac.attempted)-1]
	s.retryOrExhaust(ac, w, r, failedModel, ac.lastCooldownUntil, ac.lastRetryAfterSecs,
		ac.lastTriggerHeader, ac.lastTriggerBody, ac.lastTriggerStatus)
}

// retryOrExhaust picks the next fallback candidate and retries, or -- if
// the chain is exhausted -- returns the last real upstream error to the
// client unchanged, per §9.4.
func (s *server) retryOrExhaust(ac *attemptCtx, w http.ResponseWriter, r *http.Request,
	failedModel string, until time.Time, retryAfterSecs int, lastHeader http.Header, lastBody []byte, lastStatus int) {

	next, nextRoute, ok := s.pickNext(ac)
	if !ok {
		tried := ac.attempted[1:]
		log.Printf("✗ CHAIN EXHAUSTED %s → [%s]", ac.requestedModel, strings.Join(tried, ", "))
		s.history.record(fallbackEvent{
			At:        time.Now(),
			Requested: ac.requestedModel,
			Attempted: append([]string(nil), ac.attempted...),
			Outcome:   "chain exhausted",
		})
		w.Header().Set("x-submux-fallback", strings.Join(ac.attempted, " -> "))
		if lastStatus == 0 {
			writeAnthropicError(w, http.StatusServiceUnavailable, "overloaded_error",
				"submux: fallback chain exhausted; every remaining candidate is cooling")
			return
		}
		for k, vs := range lastHeader {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(lastStatus)
		_, _ = w.Write(lastBody)
		return
	}

	if lastStatus == 0 {
		log.Printf("⚠ SKIPPED COOLING %s → %s (cooling until %s)", failedModel, next, until.Format("15:04"))
	} else {
		log.Printf("⚠ FELL BACK %s → %s (%d, retry-after %ds) cooling until %s",
			failedModel, next, lastStatus, retryAfterSecs, until.Format("15:04"))
	}
	s.attempt(ac, w, r, next, nextRoute)
}

// rewriteResponseModel overwrites the response's `model` field with
// actualModel, the id that really answered, so the client never displays a
// substitution as if it came from the model it named (§9.3). Streaming
// (text/event-stream) responses are rewritten via sseModelRewriter, which
// only buffers the first SSE event; everything else is read fully and
// re-marshaled, since a non-streaming completion is a single JSON blob
// anyway.
func rewriteResponseModel(resp *http.Response, actualModel string) error {
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		resp.Body = newSSEModelRewriter(resp.Body, actualModel)
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<24))
	resp.Body.Close()
	if err != nil {
		return err
	}

	var generic map[string]json.RawMessage
	if json.Unmarshal(body, &generic) != nil {
		// Not JSON we understand (e.g. a non-JSON error body); forward as-is.
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
	encodedModel, err := json.Marshal(actualModel)
	if err != nil {
		return err
	}
	generic["model"] = encodedModel
	newBody, err := json.Marshal(generic)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(newBody))
	resp.ContentLength = int64(len(newBody))
	resp.Header.Set("Content-Length", strconv.Itoa(len(newBody)))
	return nil
}

// sseModelRewriter wraps a streaming SSE response body, rewriting the
// message_start event's message.model field the moment it is seen and then
// becoming a pure passthrough for everything after. It buffers at most the
// first SSE event (bounded), never the whole stream, so live token-by-token
// streaming is unaffected once past that first event.
type sseModelRewriter struct {
	src         io.ReadCloser
	br          *bufio.Reader
	actualModel string
	pending     *bytes.Reader
	scanned     bool
}

func newSSEModelRewriter(body io.ReadCloser, actualModel string) *sseModelRewriter {
	return &sseModelRewriter{src: body, br: bufio.NewReader(body), actualModel: actualModel}
}

func (s *sseModelRewriter) Read(p []byte) (int, error) {
	if !s.scanned {
		s.scanned = true
		s.pending = s.scanFirstEvent()
	}
	if s.pending != nil && s.pending.Len() > 0 {
		return s.pending.Read(p)
	}
	return s.br.Read(p)
}

func (s *sseModelRewriter) Close() error {
	return s.src.Close()
}

// scanFirstEvent reads lines up to and including the first blank line (the
// end of the first SSE event, per the SSE wire format), rewriting a
// message_start data line's model field if present, bounded to avoid
// unbounded buffering on a malformed or unexpectedly huge first event.
func (s *sseModelRewriter) scanFirstEvent() *bytes.Reader {
	var lines []string
	const maxLines = 200
	for i := 0; i < maxLines; i++ {
		line, err := s.br.ReadString('\n')
		if line != "" {
			lines = append(lines, line)
		}
		isBlank := line == "\n" || line == "\r\n"
		if isBlank || err != nil {
			break
		}
	}
	for i, line := range lines {
		if strings.HasPrefix(line, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if rewritten, ok := rewriteMessageStartPayload(payload, s.actualModel); ok {
				lines[i] = "data: " + rewritten + "\n"
			}
		}
	}
	return bytes.NewReader([]byte(strings.Join(lines, "")))
}

// rewriteMessageStartPayload parses a single SSE data-line JSON payload; if
// it is a message_start event, it returns the payload with message.model
// set to actualModel.
func rewriteMessageStartPayload(payload, actualModel string) (string, bool) {
	var evt map[string]json.RawMessage
	if json.Unmarshal([]byte(payload), &evt) != nil {
		return "", false
	}
	t, ok := evt["type"]
	if !ok || !strings.Contains(string(t), "message_start") {
		return "", false
	}
	mraw, ok := evt["message"]
	if !ok {
		return "", false
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(mraw, &msg) != nil {
		return "", false
	}
	modelEnc, err := json.Marshal(actualModel)
	if err != nil {
		return "", false
	}
	msg["model"] = modelEnc
	newMsgBytes, err := json.Marshal(msg)
	if err != nil {
		return "", false
	}
	evt["message"] = newMsgBytes
	newPayload, err := json.Marshal(evt)
	if err != nil {
		return "", false
	}
	return string(newPayload), true
}

// --- submux status -----------------------------------------------------

type statusCoolingEntry struct {
	Model string `json:"model"`
	Until string `json:"until"`
}

type statusFallbackEntry struct {
	At        string   `json:"at"`
	Requested string   `json:"requested"`
	Attempted []string `json:"attempted"`
	Outcome   string   `json:"outcome"`
}

type statusResponse struct {
	Cooling         []statusCoolingEntry  `json:"cooling"`
	RecentFallbacks []statusFallbackEntry `json:"recent_fallbacks"`
}

// serveStatus answers GET /__submux/status with the current cooling ids and
// the last 20 fallback events, for the `submux status` CLI command.
func (s *server) serveStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	resp := statusResponse{Cooling: []statusCoolingEntry{}, RecentFallbacks: []statusFallbackEntry{}}

	for id, until := range s.cooldowns.snapshot(now) {
		resp.Cooling = append(resp.Cooling, statusCoolingEntry{Model: id, Until: until.Format(time.RFC3339)})
	}
	sort.Slice(resp.Cooling, func(i, j int) bool { return resp.Cooling[i].Model < resp.Cooling[j].Model })

	for _, ev := range s.history.snapshot() {
		resp.RecentFallbacks = append(resp.RecentFallbacks, statusFallbackEntry{
			At:        ev.At.Format(time.RFC3339),
			Requested: ev.Requested,
			Attempted: ev.Attempted,
			Outcome:   ev.Outcome,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// fetchStatus is used by `submux status` (main.go) to query a running
// `submux serve` process over HTTP.
func fetchStatus(addr string) (*statusResponse, error) {
	resp, err := http.Get("http://" + addr + statusPath)
	if err != nil {
		return nil, fmt.Errorf("reach submux serve at %s: %w", addr, err)
	}
	defer resp.Body.Close()
	var st statusResponse
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("decode status response: %w", err)
	}
	return &st, nil
}
