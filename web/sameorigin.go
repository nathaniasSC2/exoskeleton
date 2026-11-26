package web

import (
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Cross-site guard. The dashboard trusts whoever reaches it (tailscale serve
// adds the tailnet login), so a web page open on one of the owner's devices
// could auto-submit a form to /console or /mode and the browser would send it
// with the owner's tailnet identity attached. Every state-changing request
// therefore has to show it came from the dashboard's own pages.
//
// A request with a method other than GET, HEAD or OPTIONS passes when:
//   - Sec-Fetch-Site is "same-origin" or "none" (every current browser sends
//     it; "same-site" is refused because other ports on the same host are
//     other apps), or
//   - there is no Sec-Fetch-Site but Origin names this host (older browsers),
//     or
//   - it carries neither header and came straight from this machine: loopback
//     address and none of the headers tailscale serve adds. That is a local
//     script or tool calling 127.0.0.1 directly, never a browser (a browser
//     always sends Origin on a cross-site POST), and a local process could
//     forge any header anyway.
//
// Everything else gets 403. Header-less requests through tailscale serve (curl
// from another tailnet device) are refused too: send an Origin to use them.

// CrossSite reports why r must be refused, or "" when it may go ahead.
// Safe methods always pass.
func CrossSite(r *http.Request) string {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return ""
	}
	if sfs := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))); sfs != "" {
		if sfs == "same-origin" || sfs == "none" {
			return ""
		}
		return "request came from another site (Sec-Fetch-Site: " + sfs + ")"
	}
	if o := r.Header.Get("Origin"); o != "" {
		if originMatches(o, r) {
			return ""
		}
		return "request came from another site (Origin: " + o + ")"
	}
	if directLocal(r) {
		return ""
	}
	return "request has no Origin or Sec-Fetch-Site header"
}

// SameOrigin wraps h with the cross-site guard.
func SameOrigin(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if why := CrossSite(r); why != "" {
			http.Error(w, "Refused: "+why+". Use the dashboard's own page.", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func originMatches(o string, r *http.Request) bool {
	u, err := url.Parse(o)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false // includes "null"
	}
	for _, h := range []string{r.Host, r.Header.Get("X-Forwarded-Host")} {
		if h != "" && strings.EqualFold(u.Host, h) {
			return true
		}
	}
	return false
}

// directLocal: a loopback peer that did not come through tailscale serve
// (which always adds X-Forwarded-For and its Tailscale-* headers).
func directLocal(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Tailscale-User-Login") != "" ||
		r.Header.Get("Tailscale-Headers-Info") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
