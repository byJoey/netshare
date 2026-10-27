package control

import (
	"crypto/subtle"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type session struct {
	Username, CSRF string
	Created, Seen  time.Time
}
type rateBucket struct {
	Start time.Time
	Count int
}
type authState struct {
	mu       sync.Mutex
	sessions map[string]session
	rates    map[string]rateBucket
}

func newAuth() *authState {
	return &authState{sessions: map[string]session{}, rates: map[string]rateBucket{}}
}
func (a *authState) allowLogin(ip string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for key, b := range a.rates {
		if now.Sub(b.Start) > time.Minute {
			delete(a.rates, key)
		}
	}
	if len(a.rates) > 2048 {
		return false
	}
	for _, key := range []string{"global", ip} {
		b := a.rates[key]
		if b.Start.IsZero() {
			b.Start = now
		}
		limit := 8
		if key == "global" {
			limit = 80
		}
		if b.Count >= limit {
			return false
		}
	}
	for _, key := range []string{"global", ip} {
		b := a.rates[key]
		if b.Start.IsZero() {
			b.Start = now
		}
		b.Count++
		a.rates[key] = b
	}
	return true
}
func (a *authState) create(username string) (string, session) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for id, s := range a.sessions {
		if now.Sub(s.Created) > 12*time.Hour || now.Sub(s.Seen) > time.Hour {
			delete(a.sessions, id)
		}
	}
	if len(a.sessions) >= 128 {
		var oldest string
		var ts time.Time
		for id, s := range a.sessions {
			if oldest == "" || s.Created.Before(ts) {
				oldest = id
				ts = s.Created
			}
		}
		delete(a.sessions, oldest)
	}
	id := randomID() + randomID()
	s := session{Username: username, CSRF: randomID() + randomID(), Created: now, Seen: now}
	a.sessions[id] = s
	return id, s
}
func (a *authState) get(r *http.Request) (string, session, bool) {
	cookie, e := r.Cookie("netshare_session")
	if e != nil {
		return "", session{}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sessions[cookie.Value]
	now := time.Now()
	if !ok || now.Sub(s.Created) > 12*time.Hour || now.Sub(s.Seen) > time.Hour {
		delete(a.sessions, cookie.Value)
		return "", session{}, false
	}
	s.Seen = now
	a.sessions[cookie.Value] = s
	return cookie.Value, s, true
}
func (c *Controller) setCookie(w http.ResponseWriter, id string) {
	http.SetCookie(w, &http.Cookie{Name: "netshare_session", Value: id, Path: "/", HttpOnly: true, Secure: c.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
}
func (c *Controller) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "netshare_session", Value: "", Path: "/", HttpOnly: true, Secure: c.cfg.CookieSecure, SameSite: http.SameSiteStrictMode, MaxAge: -1})
}
func clientIP(r *http.Request) string {
	host, _, e := net.SplitHostPort(r.RemoteAddr)
	if e != nil {
		return r.RemoteAddr
	}
	// HTTP 只监听回环；仅信任本机反向代理重写后的单一访客地址。
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if forwarded := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Forwarded-For"))); forwarded != nil {
			return forwarded.String()
		}
	}
	return host
}
func (c *Controller) validOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	a, e := url.Parse(origin)
	if e != nil {
		return false
	}
	b, e := url.Parse(c.cfg.ExternalURL)
	return e == nil && strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host) && a.User == nil
}
func (c *Controller) protected(next http.HandlerFunc, mutation bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, s, ok := c.auth.get(r)
		if !ok {
			apiError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		if mutation {
			if !c.validOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
				apiError(w, http.StatusForbidden, "页面校验已失效，请刷新后重试")
				return
			}
		}
		next(w, r)
	}
}
