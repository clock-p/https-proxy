package shared

import (
	"net/http"
	"sort"
	"strings"
)

var hopByHopHeaderKeys = map[string]struct{}{
	"Connection":          {},
	"Proxy-Connection":    {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"TE":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

var wsRequestStripKeys = map[string]struct{}{
	http.CanonicalHeaderKey("Sec-WebSocket-Key"):        {},
	http.CanonicalHeaderKey("Sec-WebSocket-Version"):    {},
	http.CanonicalHeaderKey("Sec-WebSocket-Extensions"): {},
	http.CanonicalHeaderKey("Sec-WebSocket-Accept"):     {},
}

func IsHopByHopHeaderKey(k string) bool {
	_, ok := hopByHopHeaderKeys[http.CanonicalHeaderKey(k)]
	return ok
}

func IsWSStripHeaderKey(k string) bool {
	_, ok := wsRequestStripKeys[http.CanonicalHeaderKey(k)]
	return ok
}

func StripConnectionHeaders(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, token := range strings.Split(v, ",") {
			k := http.CanonicalHeaderKey(strings.TrimSpace(token))
			if k == "" {
				continue
			}
			h.Del(k)
		}
	}
	for k := range hopByHopHeaderKeys {
		h.Del(k)
	}
}

func TrailerKeysFromHeader(h http.Header) []string {
	vals := h.Values("Trailer")
	if len(vals) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	keys := make([]string, 0, len(vals))
	for _, v := range vals {
		for _, token := range strings.Split(v, ",") {
			k := http.CanonicalHeaderKey(strings.TrimSpace(token))
			if k == "" {
				continue
			}
			if _, ok := seen[k]; ok {
				continue
			}
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
	}
	if len(keys) > 1 {
		sort.Strings(keys)
	}
	return keys
}

func HeaderKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	seen := map[string]struct{}{}
	for k := range h {
		ck := http.CanonicalHeaderKey(k)
		if ck == "" {
			continue
		}
		if _, ok := seen[ck]; ok {
			continue
		}
		seen[ck] = struct{}{}
		keys = append(keys, ck)
	}
	if len(keys) > 1 {
		sort.Strings(keys)
	}
	return keys
}

func CopyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if IsHopByHopHeaderKey(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func CopyHeadersForWebSocket(dst, src http.Header) {
	for k, vv := range src {
		if IsHopByHopHeaderKey(k) || IsWSStripHeaderKey(k) {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
