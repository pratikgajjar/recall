package main

// jsonx — single point where the JSON library is chosen.
//
// Benchmarked on 245,666 Cursor bubble blobs (4.3 GB):
//
//   encoding/json         22.0 s
//   goccy/go-json          6.4 s   (2.8× vs stdlib)
//   bytedance/sonic        5.9 s   (3.7×)
//   sonic ConfigFastest    2.8 s   (7.9×)   ← chosen
//
// ConfigFastest is non-strict (e.g. trailing commas, NaN, sloppy unicode),
// but Cursor / Claude / Codex / pi all emit conformant JSON so the relaxed
// mode is safe for our inputs.
//
// On non-amd64/arm64 sonic falls back to a pure-Go encoder; the build tag
// `nosonic` lets us drop the dep entirely if it ever becomes a problem.

import (
	"io"

	json "github.com/bytedance/sonic"
)

// api is the sonic API used by every adapter. ConfigFastest enables
// the JIT decoder and skips strict validation we don't need.
var api = json.ConfigFastest

// JSONUnmarshal decodes JSON into v. Drop-in for encoding/json.Unmarshal.
func JSONUnmarshal(data []byte, v any) error { return api.Unmarshal(data, v) }

// JSONMarshal encodes v to JSON. Drop-in for encoding/json.Marshal.
func JSONMarshal(v any) ([]byte, error) { return api.Marshal(v) }

// JSONNewDecoder returns a streaming decoder. Falls back to stdlib-compatible
// behavior (sonic's Decoder is API-compatible).
func JSONNewDecoder(r io.Reader) Decoder { return api.NewDecoder(r) }

// Decoder is the minimal streaming interface adapters need.
type Decoder interface {
	Decode(v any) error
}

// JSONNewEncoder returns a streaming encoder for w. Used for --json output
// in CLI commands; sonic's encoder is drop-in compatible with encoding/json.
func JSONNewEncoder(w io.Writer) Encoder { return api.NewEncoder(w) }

// Encoder is the minimal streaming-encode interface we need.
type Encoder interface {
	Encode(v any) error
	SetIndent(prefix, indent string)
}
