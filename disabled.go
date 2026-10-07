package main

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// A subscription that the provider has switched off (plan ended, org
// disabled access, key revoked) answers every request with the same 4xx
// until a human acts. 2026-10-07: the Claude Max passthrough route returned
// "Your organization has disabled Claude subscription access" for hours and
// killed 13 agents in 6 sessions, because every route had fallback: [] and
// nothing failed over.
//
// submux now marks such a subscription disabled for disabledFor and sends
// the same model to another subscription that serves it (today: the Kiro
// route, via kiroResolve). No config edit is needed when a plan ends.
// Re-enable is a half-open circuit breaker: after disabledFor the next real
// request goes through again, and a repeat failure re-disables. A probe
// cannot do this for passthrough routes, since submux holds no credential
// for them.
const disabledFor = 6 * time.Hour

// disabledBodyRe matches the provider's "this subscription is off" wording.
// A plain auth error ("invalid x-api-key", "OAuth token has expired") must
// NOT match: that is one client's stale token, not the subscription.
var disabledBodyRe = regexp.MustCompile(`(?i)(disabled|expired|revoked|deactivated|suspended|cancel+ed|ended)\b.{0,60}\b(subscription|plan|account|organization)|(subscription|plan|account)\b.{0,40}\b(disabled|expired|revoked|deactivated|suspended|cancel+ed|ended)`)

// subKey names the subscription a route bills to: its fixed label, else its
// match pattern (one upstream credential per route).
func subKey(rt route) string {
	if rt.subscription != "" {
		return rt.subscription
	}
	return rt.match
}

// isDisabledResponse reports whether resp says the route's subscription is
// switched off. It reads the body and always puts it back, so a non-match
// reaches the client unchanged.
func isDisabledResponse(resp *http.Response, rt route) bool {
	switch resp.StatusCode {
	case http.StatusPaymentRequired, http.StatusForbidden:
	case http.StatusUnauthorized:
		// A 401 on a passthrough route is the client's own token; it says
		// nothing about the subscription for everyone else.
		if rt.authKind == "passthrough" {
			return false
		}
	default:
		return false
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(b))
	return disabledBodyRe.Match(b)
}

// sameModelAlt finds the same model on another subscription: the requested
// id resolved onto the live Kiro list, on a route that is neither disabled
// nor a passthrough nor already tried.
func (s *server) sameModelAlt(ac *attemptCtx) (string, route, bool) {
	liveFn := s.kiroLive
	if s.kiroLiveFn != nil {
		liveFn = s.kiroLiveFn
	}
	live := liveFn()
	if len(live) == 0 {
		return "", route{}, false
	}
	id, _, ok := kiroResolve(ac.requestedModel, live)
	if !ok || ac.tried[id] {
		return "", route{}, false
	}
	rt, ok := matchRoute(s.conf().Routes, id)
	if !ok || rt.authKind == "passthrough" {
		return "", route{}, false
	}
	if _, off := s.disabled.isCooling(subKey(rt), time.Now()); off {
		return "", route{}, false
	}
	return id, rt, true
}

// failOverDisabled dispatches the same model on another subscription. It
// returns false (nothing written) when there is no such route.
func (s *server) failOverDisabled(ac *attemptCtx, w http.ResponseWriter, r *http.Request, from string) bool {
	id, rt, ok := s.sameModelAlt(ac)
	if !ok {
		return false
	}
	// From here on the request must never bounce back to a passthrough route.
	ac.kiroMode = true
	log.Printf("⚠ SUBSCRIPTION DISABLED %s → %s", from, id)
	s.attempt(ac, w, r, id, rt)
	return true
}

// unknownKiroID reports whether id is a kiro/* id missing from the live list,
// so a dash/dot or [1m] spelling the upstream does not list
// ("kiro/claude-sonnet-5-5") can be mapped instead of 400ing.
func unknownKiroID(id string, live []string) bool {
	if !strings.HasPrefix(id, kiroIDPrefix) || len(live) == 0 {
		return false
	}
	for _, l := range live {
		if l == id {
			return false
		}
	}
	return true
}
