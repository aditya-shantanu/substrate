package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestWindowCacheSingleFlightAndSlicing(t *testing.T) {
	blob := make([]byte, 5<<20+123)
	for i := range blob {
		blob[i] = byte(i * 7)
	}
	var fetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		var lo, hi int64
		fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-%d", &lo, &hi)
		if hi >= int64(len(blob)) {
			hi = int64(len(blob)) - 1
		}
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, hi, len(blob)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(blob[lo : hi+1])
	}))
	defer srv.Close()
	wc := newWindowCache(2<<20, 64<<20)
	fetch := func(ctx context.Context, lo, hi int64) (*http.Response, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/v2/r/blobs/sha256:abc", nil)
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", lo, hi))
		return http.DefaultClient.Do(req)
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			off := int64(i) * 100 << 10 // 100 KiB apart, all inside window 0 or 1
			w := wc.get(context.Background(), "t", off, fetch)
			if w.err != nil {
				t.Error(w.err)
				return
			}
			base := off % wc.wsize
			if got := w.data[base : base+16]; string(got) != string(blob[off:off+16]) {
				t.Errorf("offset %d: wrong bytes", off)
			}
		}(i)
	}
	wg.Wait()
	// 32 reads spread over 3.2 MiB touch windows 0 and 1 only.
	if n := fetches.Load(); n != 2 {
		t.Fatalf("expected 2 upstream fetches, got %d", n)
	}
	// Tail window is short and reports the blob size.
	w := wc.get(context.Background(), "t", 5<<20, fetch)
	if w.err != nil || len(w.data) != 1<<20+123 || w.total != int64(len(blob)) {
		t.Fatalf("tail window: err=%v len=%d total=%d", w.err, len(w.data), w.total)
	}
}

func TestParseBlobRange(t *testing.T) {
	wc := newWindowCache(32<<20, 1<<30)
	mk := func(rng string) *http.Request {
		r := httptest.NewRequest("GET", "/v2/us-east4-docker.pkg.dev/p/r/blobs/sha256:abc", nil)
		r.Header.Set("Range", rng)
		return r
	}
	if _, _, ok := wc.parseBlobRange(mk("bytes=0-1048575")); !ok {
		t.Fatal("in-window range should be served from cache")
	}
	if _, _, ok := wc.parseBlobRange(mk("bytes=33554000-33555000")); ok {
		t.Fatal("boundary-crossing range must pass through")
	}
	if _, _, ok := wc.parseBlobRange(mk("bytes=100-")); ok {
		t.Fatal("open-ended range must pass through")
	}
	m := httptest.NewRequest("GET", "/v2/p/r/manifests/tag", nil)
	m.Header.Set("Range", "bytes=0-10")
	if _, _, ok := wc.parseBlobRange(m); ok {
		t.Fatal("manifests must pass through")
	}
	_ = io.Discard
	_ = strings.Contains
}
