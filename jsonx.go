package main

import (
	"io"

	json "github.com/bytedance/sonic"
)

var api = json.ConfigFastest

func JSONUnmarshal(data []byte, v any) error { return api.Unmarshal(data, v) }

func JSONMarshal(v any) ([]byte, error) { return api.Marshal(v) }

func JSONNewDecoder(r io.Reader) Decoder { return api.NewDecoder(r) }

type Decoder interface {
	Decode(v any) error
}

func JSONNewEncoder(w io.Writer) Encoder { return api.NewEncoder(w) }

type Encoder interface {
	Encode(v any) error
	SetIndent(prefix, indent string)
}
