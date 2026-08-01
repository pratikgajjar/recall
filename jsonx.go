package main

import (
	"io"

	json "github.com/bytedance/sonic"
)

// ConfigFastest skips UTF-8 validation, which is most of why it is fastest —
// and it will happily emit a truncated multibyte sequence straight into a JSON
// document. Real transcripts contain them: 5 rows in a 200,000-row index, but
// they were enough to make two of six sample searches return a body no JSON
// parser accepts, so an agent got a crash instead of results. Validation turns
// those bytes into U+FFFD.
var api = json.Config{
	NoQuoteTextMarshaler: true,
	ValidateString:       true,
}.Froze()

func JSONUnmarshal(data []byte, v any) error { return api.Unmarshal(data, v) }

func JSONMarshal(v any) ([]byte, error) { return api.Marshal(v) }

func JSONNewEncoder(w io.Writer) Encoder { return api.NewEncoder(w) }

type Encoder interface {
	Encode(v any) error
}
