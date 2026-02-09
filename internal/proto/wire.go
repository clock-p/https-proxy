package proto

import (
	"encoding/json"
)

type ReqStart struct {
	Method      string              `json:"m"`
	Path        string              `json:"p"`
	RawQuery    string              `json:"q,omitempty"`
	Host        string              `json:"o,omitempty"`
	Header      map[string][]string `json:"h,omitempty"`
	TrailerKeys []string            `json:"t,omitempty"`
	IsWebSocket bool                `json:"ws,omitempty"`
}

type ResStart struct {
	Status      int                 `json:"s"`
	Header      map[string][]string `json:"h,omitempty"`
	TrailerKeys []string            `json:"t,omitempty"`
	Interim     bool                `json:"i,omitempty"`
}

type WsOpenErr struct {
	Message string `json:"m"`
}

type WsOpenOK struct {
	Subprotocol string `json:"p,omitempty"`
}

type WsClose struct {
	Code   int    `json:"c,omitempty"`
	Reason string `json:"r,omitempty"`
}

func MustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
