package jellybridge

import (
	"net/http"
	"strings"

	"github.com/pdeslaur/hangar/internal/jellyfin"
)

// Headers that carry a token or the device to Jellyfin: Authorization
// always, the rest with its legacy authorization on.
const (
	embyAuthorization   = "X-Emby-Authorization"
	embyToken           = "X-Emby-Token"
	mediaBrowserToken   = "X-MediaBrowser-Token"
	authorizationScheme = "MediaBrowser"
	legacyScheme        = "Emby"
)

// credentials are what a request tells Jellyfin about its device and token.
type credentials struct {
	// device is from the Authorization header, its fields empty when the
	// request doesn't name them.
	device jellyfin.Device
	// token is the one Jellyfin would take, empty if none.
	token string
}

// takeCredentials returns the device and token Jellyfin would read from r,
// reading the legacy places too, and removes every token from r: its
// Authorization headers and the token headers, and the query parameters
// apikey and api_key in any case. It leaves r without an Authorization
// header; the caller sets one.
//
// It takes the token Jellyfin would, from the first place holding one, in
// Jellyfin's order (AuthorizationContext): the Authorization header's Token,
// X-Emby-Token, X-MediaBrowser-Token, ApiKey, api_key. A parameter given
// twice reaches Jellyfin as both values joined by a comma, which no token
// matches.
func takeCredentials(r *http.Request) credentials {
	var c credentials
	header := r.Header.Get("Authorization")
	if header == "" {
		header = r.Header.Get(embyAuthorization)
	}
	if params, ok := parseAuthorization(header); ok {
		c.device = jellyfin.Device{
			Client:  params["Client"],
			Name:    params["Device"],
			ID:      params["DeviceId"],
			Version: params["Version"],
		}
		c.token = params["Token"]
	}
	for _, h := range []string{embyToken, mediaBrowserToken} {
		if c.token == "" {
			c.token = strings.Join(r.Header.Values(h), ",")
		}
	}
	for _, h := range []string{"Authorization", embyAuthorization, embyToken, mediaBrowserToken} {
		r.Header.Del(h)
	}

	query, apiKey, legacyKey := takeQueryTokens(r.URL.RawQuery)
	r.URL.RawQuery = query
	for _, v := range [][]string{apiKey, legacyKey} {
		if c.token == "" {
			c.token = strings.Join(v, ",")
		}
	}
	return c
}

// takeQueryTokens removes from query every parameter named apikey or
// api_key in any case, as ASP.NET compares names, leaving the rest as they
// were, and returns their values.
func takeQueryTokens(query string) (rest string, apiKey, legacyKey []string) {
	var kept []string
	for part := range strings.SplitSeq(query, "&") {
		if part == "" {
			continue
		}
		rawKey, rawValue, _ := strings.Cut(part, "=")
		key, value := unescape(rawKey), unescape(rawValue)
		switch {
		case strings.EqualFold(key, "apikey"):
			apiKey = append(apiKey, value)
		case strings.EqualFold(key, "api_key"):
			legacyKey = append(legacyKey, value)
		default:
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, "&"), apiKey, legacyKey
}

// parseAuthorization returns the parameters of an Authorization header of
// the MediaBrowser or Emby scheme, and false for another scheme.
func parseAuthorization(header string) (map[string]string, bool) {
	scheme, rest, ok := strings.Cut(header, " ")
	if !ok || !(strings.EqualFold(scheme, authorizationScheme) || strings.EqualFold(scheme, legacyScheme)) {
		return nil, false
	}
	return parseParams(rest), true
}

// parseParams parses an Authorization header's parameters as Jellyfin does
// (AuthorizationContext.GetParts): key="value" pairs separated by commas,
// the quotes optional, each value URL-decoded, a later key replacing an
// earlier one.
func parseParams(s string) map[string]string {
	params := map[string]string{}
	escaped := false
	start := 0
	key := ""
	add := func(end int) {
		if start < end {
			params[key] = unescape(strings.Trim(s[start:end], `"`))
			key = ""
		}
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == ',':
			// A quote opens or closes a value; a comma outside one ends it.
			escaped = !escaped == (c == '"')
			if c == ',' && !escaped {
				add(i)
				start = i + 1
			}
		case !escaped && c == '=':
			key = strings.TrimSpace(s[start:i])
			start = i + 1
		}
	}
	add(len(s))
	return params
}

// unescape URL-decodes s as .NET's WebUtility.UrlDecode does: a plus is a
// space, and a percent sign not followed by two hex digits stays as it is,
// where url.QueryUnescape would refuse the whole string.
func unescape(s string) string {
	if !strings.ContainsAny(s, "%+") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '+':
			b.WriteByte(' ')
		case s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]):
			b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
			i += 2
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	default:
		return c - 'a' + 10
	}
}
