package shared

import (
	"net/url"
	"path"
	"strings"
)

func JoinURL(base *url.URL, suffixPath string) *url.URL {
	out := *base
	basePath := base.Path
	if basePath == "" {
		basePath = "/"
	}
	if suffixPath == "" {
		suffixPath = "/"
	}
	out.Path = path.Join(strings.TrimSuffix(basePath, "/"), suffixPath)
	if strings.HasSuffix(suffixPath, "/") && !strings.HasSuffix(out.Path, "/") {
		out.Path += "/"
	}
	return &out
}

func ToWebSocketURL(u *url.URL) *url.URL {
	out := *u
	switch strings.ToLower(out.Scheme) {
	case "http":
		out.Scheme = "ws"
	case "https":
		out.Scheme = "wss"
	}
	return &out
}
