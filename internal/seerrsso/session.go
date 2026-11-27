package seerrsso

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// sessionLimitCookie holds when the browser's Seerr session stops being
// passed to Seerr, and a MAC binding that time to the session cookie.
const sessionLimitCookie = "seerr-sso-session"

// sessionMAC is the MAC of a session cookie's value good until expires.
func (h *Handler) sessionMAC(expires int64, session string) string {
	mac := hmac.New(sha256.New, h.key)
	mac.Write([]byte(strconv.FormatInt(expires, 10) + "." + session))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// limitSessions adds a sessionLimitCookie to the Seerr session cookie that
// header sets, if any: any sign-in, through /sso/login or Seerr's own.
func (h *Handler) limitSessions(header http.Header) {
	if h.SessionDuration <= 0 {
		return
	}
	for _, line := range header.Values("Set-Cookie") {
		c, err := http.ParseSetCookie(line)
		if err != nil || c.Name != sessionCookie || c.Value == "" {
			continue
		}
		expires := h.now().Add(h.SessionDuration).Unix()
		limit := &http.Cookie{
			Name:     sessionLimitCookie,
			Value:    strconv.FormatInt(expires, 10) + "." + h.sessionMAC(expires, c.Value),
			Path:     "/",
			MaxAge:   int(h.SessionDuration.Seconds()),
			Secure:   true,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		}
		header.Add("Set-Cookie", limit.String())
		return
	}
}

// sessionValid reports whether r's Seerr session cookie, if any, is within
// its limit: its sessionLimitCookie is for it, and not past.
func (h *Handler) sessionValid(r *http.Request) bool {
	session, err := r.Cookie(sessionCookie)
	if err != nil {
		return true
	}
	limit, err := r.Cookie(sessionLimitCookie)
	if err != nil {
		return false
	}
	at, mac, ok := strings.Cut(limit.Value, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(at, 10, 64)
	if err != nil || h.now().Unix() >= expires {
		return false
	}
	return hmac.Equal([]byte(mac), []byte(h.sessionMAC(expires, session.Value)))
}

// dropCookies removes the cookies named from header's Cookie.
func dropCookies(header http.Header, names ...string) {
	var kept []string
	for _, line := range header.Values("Cookie") {
		var parts []string
		for part := range strings.SplitSeq(line, ";") {
			part = strings.TrimSpace(part)
			if name, _, _ := strings.Cut(part, "="); part != "" && !slices.Contains(names, name) {
				parts = append(parts, part)
			}
		}
		if len(parts) > 0 {
			kept = append(kept, strings.Join(parts, "; "))
		}
	}
	header.Del("Cookie")
	for _, line := range kept {
		header.Add("Cookie", line)
	}
}

func (h *Handler) now() time.Time {
	if h.clock != nil {
		return h.clock()
	}
	return time.Now()
}
