// Microbenchmark for cursor bubble decode strategies on real corpus.
// Compares: encoding/json (stdlib), goccy/go-json, bytedance/sonic.
package main

import (
	"database/sql"
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	sonic "github.com/bytedance/sonic"
	gojson "github.com/goccy/go-json"
	_ "modernc.org/sqlite"
)

// BubbleBlob is the typed view recall needs from a Cursor bubble.
// `type` lives on the composer header, not the bubble itself.
type BubbleBlob struct {
	Text     string `json:"text"`
	RichText string `json:"richText"`
	Content  string `json:"content"`
}

func main() {
	home, _ := os.UserHomeDir()
	gpath := filepath.Join(home, "Library", "Application Support", "Cursor", "User",
		"globalStorage", "state.vscdb")
	db, err := sql.Open("sqlite", gpath+"?mode=ro&immutable=1")
	if err != nil {
		panic(err)
	}
	defer db.Close()

	t0 := time.Now()
	rows, err := db.Query(`SELECT value FROM cursorDiskKV WHERE key LIKE 'bubbleId:%'`)
	if err != nil {
		panic(err)
	}
	var blobs [][]byte
	var totalBytes int64
	for rows.Next() {
		var v []byte
		_ = rows.Scan(&v)
		blobs = append(blobs, v)
		totalBytes += int64(len(v))
	}
	rows.Close()
	fmt.Printf("READ: %d blobs, %.1f MB in %v (%.1f MB/s)\n\n",
		len(blobs), float64(totalBytes)/1e6, time.Since(t0),
		float64(totalBytes)/1e6/time.Since(t0).Seconds())

	bench("encoding/json", blobs, func(raw []byte, b *BubbleBlob) error {
		return stdjson.Unmarshal(raw, b)
	})
	bench("goccy/go-json", blobs, func(raw []byte, b *BubbleBlob) error {
		return gojson.Unmarshal(raw, b)
	})
	bench("bytedance/sonic", blobs, func(raw []byte, b *BubbleBlob) error {
		return sonic.Unmarshal(raw, b)
	})

	// sonic supports a "fast" decoder that skips strict spec compliance.
	bench("sonic ConfigFastest", blobs, func(raw []byte, b *BubbleBlob) error {
		return sonic.ConfigFastest.Unmarshal(raw, b)
	})
}

func bench(name string, blobs [][]byte, fn func([]byte, *BubbleBlob) error) {
	t0 := time.Now()
	hits, errs := 0, 0
	for _, raw := range blobs {
		var b BubbleBlob
		if err := fn(raw, &b); err != nil {
			errs++
			continue
		}
		if b.Text != "" {
			hits++
		}
	}
	fmt.Printf("  %-22s hits=%-7d errs=%-4d %v\n", name, hits, errs, time.Since(t0))
}
