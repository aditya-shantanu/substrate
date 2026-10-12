// arproxy: a tiny registry front for nydusd. nydusd's registry backend fails on
// Artifact Registry's relative-Location blob redirects; this proxy forwards /v2/*
// to AR with a bearer token, follows redirects, and streams the body back.
//
// Registries meter requests, not bytes, and a fleet of per-actor nydusd
// instances reading ~1 MiB zran contexts bursts past Artifact Registry's
// per-minute quota. So the proxy also: retries 429/5xx with capped backoff,
// bounds in-flight upstream requests, rate-limits upstream attempts per node
// (token bucket), and serves blob range reads from node-local aligned windows
// fetched once each (window.go) so an image costs ~size/window requests.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	mu    sync.Mutex
	tok   string
	tokAt time.Time

	throttled, retriedOK, failed, requests, bytesOut atomic.Int64

	// Range-length histogram: none, <=128K, <=512K, <=1M, <=4M, >4M.
	rangeHist [6]atomic.Int64
)

func token() string {
	mu.Lock()
	defer mu.Unlock()
	if time.Since(tokAt) < 40*time.Minute && tok != "" {
		return tok
	}
	req, _ := http.NewRequest("GET", "http://metadata.google.internal/computeMetadata/v1/instance/service-accounts/default/token", nil)
	req.Header.Set("Metadata-Flavor", "Google")
	if resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req); err == nil && resp.StatusCode == 200 {
		var body struct {
			AccessToken string `json:"access_token"`
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if json.Unmarshal(b, &body) == nil && body.AccessToken != "" {
			tok, tokAt = body.AccessToken, time.Now()
			return tok
		}
	}
	out, err := exec.Command("gcloud", "auth", "print-access-token").Output()
	if err != nil {
		log.Printf("token: %v", err)
		return tok
	}
	tok, tokAt = strings.TrimSpace(string(out)), time.Now()
	return tok
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

func rangeBucket(h string) int {
	rest, ok := strings.CutPrefix(h, "bytes=")
	if !ok {
		return 0
	}
	a, b, found := strings.Cut(rest, "-")
	if !found {
		return 0
	}
	lo, err1 := strconv.ParseInt(a, 10, 64)
	hi, err2 := strconv.ParseInt(b, 10, 64)
	if err1 != nil || err2 != nil {
		return 5
	}
	switch n := hi - lo + 1; {
	case n <= 128<<10:
		return 1
	case n <= 512<<10:
		return 2
	case n <= 1<<20:
		return 3
	case n <= 4<<20:
		return 4
	}
	return 5
}

func retryable(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// upstream holds the shared limits every upstream attempt goes through.
type upstream struct {
	client   *http.Client
	tokens   chan struct{} // nil = unlimited
	inflight chan struct{}
	budget   time.Duration
}

// do performs one logical upstream request with retry/backoff on 429/5xx. The
// caller owns resp.Body. Every attempt, retries included, is a metered registry
// request, so each one takes a rate token.
func (u *upstream) do(ctx context.Context, method, target, rng, accept, inm string) (*http.Response, error) {
	select {
	case u.inflight <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-u.inflight }()
	deadline := time.Now().Add(u.budget)
	for attempt := 0; ; attempt++ {
		if u.tokens != nil {
			select {
			case <-u.tokens:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, _ := http.NewRequestWithContext(ctx, method, target, nil)
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		req.Header.Set("Authorization", "Bearer "+token())
		resp, err := u.client.Do(req) // follows AR's redirect to the download endpoint
		if err == nil && !retryable(resp.StatusCode) {
			if attempt > 0 {
				retriedOK.Add(1)
			}
			return resp, nil
		}
		throttled.Add(1)
		wait := time.Duration(200*(1<<min(attempt, 5)))*time.Millisecond + time.Duration(rand.Intn(300))*time.Millisecond
		if err == nil {
			if ra, e := strconv.Atoi(resp.Header.Get("Retry-After")); e == nil && ra > 0 {
				wait = max(wait, time.Duration(ra)*time.Second)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		if time.Now().Add(wait).After(deadline) {
			failed.Add(1)
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("upstream rate limited (%d)", resp.StatusCode)
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func main() {
	defaultUpstream := "https://us-east4-docker.pkg.dev"
	if u := os.Getenv("ARPROXY_UPSTREAM"); u != "" {
		defaultUpstream = u
	}
	up := &upstream{
		client:   &http.Client{Timeout: 10 * time.Minute},
		inflight: make(chan struct{}, envInt("ARPROXY_MAX_INFLIGHT", 48)),
		// nydusd gives each request a bounded time before it counts a failure,
		// so the retry budget must fit inside that.
		budget: time.Duration(envInt("ARPROXY_RETRY_BUDGET_S", 25)) * time.Second,
	}
	perMin := envInt("ARPROXY_RATE_PER_MIN", 0)
	if perMin > 0 {
		up.tokens = make(chan struct{}, max(perMin/30, 1)) // 2 s of burst
		go func() {
			t := time.NewTicker(time.Minute / time.Duration(perMin))
			for range t.C {
				select {
				case up.tokens <- struct{}{}:
				default:
				}
			}
		}()
	}
	var wc *windowCache
	if wmb := envInt("ARPROXY_WINDOW_MB", 0); wmb > 0 {
		wc = newWindowCache(int64(wmb)<<20, int64(envInt("ARPROXY_WINDOW_CACHE_MB", 3072))<<20)
	}

	go func() {
		for range time.Tick(30 * time.Second) {
			extra := ""
			if wc != nil {
				extra = fmt.Sprintf(" window-fetches=%d window-hits=%d window-evictions=%d window-fullbodies=%d window-body-retries=%d window-body-errors=%d", wc.fetches.Load(), wc.hits.Load(), wc.evictions.Load(), wc.fullBodies.Load(), wc.bodyRetries.Load(), wc.bodyErrors.Load())
			}
			log.Printf("requests=%d bytes=%d upstream-429/5xx=%d recovered-by-retry=%d gave-up=%d range-hist(none,128K,512K,1M,4M,big)=%d,%d,%d,%d,%d,%d%s",
				requests.Load(), bytesOut.Load(), throttled.Load(), retriedOK.Load(), failed.Load(),
				rangeHist[0].Load(), rangeHist[1].Load(), rangeHist[2].Load(), rangeHist[3].Load(), rangeHist[4].Load(), rangeHist[5].Load(), extra)
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// A repository may be prefixed with its registry host
		// (/v2/us-docker.pkg.dev/proj/repo/...): route there instead of the
		// default upstream, so one proxy serves every registry.
		target := defaultUpstream + r.URL.RequestURI()
		if rest, ok := strings.CutPrefix(r.URL.Path, "/v2/"); ok {
			if host, tail, found := strings.Cut(rest, "/"); found && strings.Contains(host, ".") {
				u := *r.URL
				u.Path = "/v2/" + tail
				target = "https://" + host + u.RequestURI()
			}
		}
		requests.Add(1)
		rangeHist[rangeBucket(r.Header.Get("Range"))].Add(1)

		if wc != nil {
			if lo, hi, ok := wc.parseBlobRange(r); ok {
				win := wc.get(r.Context(), target, lo, func(ctx context.Context, wlo, whi int64) (*http.Response, error) {
					return up.do(ctx, http.MethodGet, target, fmt.Sprintf("bytes=%d-%d", wlo, whi), r.Header.Get("Accept"), "")
				})
				if win.err != nil {
					http.Error(w, win.err.Error(), http.StatusBadGateway)
					return
				}
				base := lo % wc.wsize
				if base >= int64(len(win.data)) {
					http.Error(w, "range beyond blob", http.StatusRequestedRangeNotSatisfiable)
					return
				}
				chunk := win.data[base:min(base+(hi-lo)+1, int64(len(win.data)))]
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
				w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", lo, lo+int64(len(chunk))-1, win.total))
				w.Header().Set("Accept-Ranges", "bytes")
				w.WriteHeader(http.StatusPartialContent)
				n, _ := w.Write(chunk)
				bytesOut.Add(int64(n))
				return
			}
		}

		resp, err := up.do(r.Context(), r.Method, target, r.Header.Get("Range"), r.Header.Get("Accept"), r.Header.Get("If-None-Match"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for _, h := range []string{"Content-Type", "Content-Length", "Content-Range", "Docker-Content-Digest", "Etag", "Accept-Ranges"} {
			if v := resp.Header.Get(h); v != "" {
				w.Header().Set(h, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		n, _ := io.Copy(w, resp.Body)
		bytesOut.Add(n)
	})
	addr := ":5000"
	if len(os.Args) > 1 {
		addr = os.Args[1]
	} else if a := os.Getenv("ARPROXY_LISTEN"); a != "" {
		addr = a
	}
	wdesc := "off"
	if wc != nil {
		wdesc = fmt.Sprintf("%d MiB windows, %d MiB cache", wc.wsize>>20, wc.budget>>20)
	}
	log.Printf("arproxy v9 on %s -> %s (max inflight %d, retry budget %s, rate/min %d, window cache %s)", addr, defaultUpstream, cap(up.inflight), up.budget, perMin, wdesc)
	log.Fatal(http.ListenAndServe(addr, nil))
}
