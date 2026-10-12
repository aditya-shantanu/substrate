package main

// Window coalescing: nydusd asks for ~1 MiB ranges (Nydus zran contexts cap read
// amplification), and registries meter requests, not bytes. Serve blob range
// reads from node-local aligned windows fetched once each, single-flight, so a
// 4.5 GB image costs ~size/window requests instead of ~2,300.

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var rangeRe = regexp.MustCompile(`^bytes=(\d+)-(\d+)$`)

type window struct {
	data  []byte
	total int64 // blob size from Content-Range
	err   error
	ready chan struct{}
	elem  *list.Element
	key   string
}

type windowCache struct {
	mu      sync.Mutex
	size    int64
	budget  int64
	entries map[string]*window
	lru     *list.List
	wsize   int64

	fetches, hits, evictions, fullBodies, bodyRetries, bodyErrors atomic.Int64
}

func newWindowCache(windowBytes, budgetBytes int64) *windowCache {
	return &windowCache{budget: budgetBytes, entries: map[string]*window{}, lru: list.New(), wsize: windowBytes}
}

// parseBlobRange returns the closed range for blob GETs that fit in one window.
func (c *windowCache) parseBlobRange(r *http.Request) (lo, hi int64, ok bool) {
	if r.Method != http.MethodGet || !strings.Contains(r.URL.Path, "/blobs/sha256:") {
		return 0, 0, false
	}
	m := rangeRe.FindStringSubmatch(r.Header.Get("Range"))
	if m == nil {
		return 0, 0, false
	}
	lo, _ = strconv.ParseInt(m[1], 10, 64)
	hi, _ = strconv.ParseInt(m[2], 10, 64)
	if hi < lo || lo/c.wsize != hi/c.wsize {
		return 0, 0, false // crosses a window boundary: pass through
	}
	return lo, hi, true
}

// get returns the window containing offset, fetching it once if needed.
func (c *windowCache) get(ctx context.Context, target string, offset int64, fetch func(ctx context.Context, lo, hi int64) (*http.Response, error)) *window {
	idx := offset / c.wsize
	key := fmt.Sprintf("%s#%d", target, idx)
	c.mu.Lock()
	if w, ok := c.entries[key]; ok {
		c.lru.MoveToFront(w.elem)
		c.mu.Unlock()
		<-w.ready
		c.hits.Add(1)
		return w
	}
	w := &window{ready: make(chan struct{}), key: key}
	w.elem = c.lru.PushFront(w)
	c.entries[key] = w
	c.mu.Unlock()

	c.fetches.Add(1)
	lo, hi := idx*c.wsize, (idx+1)*c.wsize-1
	var resp *http.Response
	var err error
	// A 32 MiB body is far likelier to hit a mid-stream reset than a 1 MiB one,
	// and a failed window fails every reader queued on it, so retry transport
	// and body errors a few times before surfacing an error.
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			c.bodyRetries.Add(1)
			select {
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			case <-ctx.Done():
			}
		}
		resp, err = fetch(context.WithoutCancel(ctx), lo, hi)
		if err != nil {
			continue
		}
		w.data, w.total, err = readWindow(resp, lo, hi, c)
		if err == nil {
			break
		}
	}
	w.err = err
	if err != nil {
		c.bodyErrors.Add(1)
	}
	close(w.ready)

	c.mu.Lock()
	if err != nil {
		// Do not cache failures; the next reader retries.
		c.lru.Remove(w.elem)
		delete(c.entries, key)
	} else {
		c.size += int64(len(w.data))
		for c.size > c.budget && c.lru.Len() > 1 {
			old := c.lru.Back().Value.(*window)
			if old == w {
				break
			}
			c.lru.Remove(old.elem)
			delete(c.entries, old.key)
			c.size -= int64(len(old.data))
			c.evictions.Add(1)
		}
	}
	c.mu.Unlock()
	return w
}

func contentTotal(cr string) int64 {
	// "bytes lo-hi/total"
	if i := strings.LastIndex(cr, "/"); i >= 0 {
		if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
			return n
		}
	}
	return -1
}

// readWindow consumes one upstream window response into memory.
func readWindow(resp *http.Response, lo, hi int64, c *windowCache) (data []byte, total int64, err error) {
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		total = contentTotal(resp.Header.Get("Content-Range"))
		data, err = io.ReadAll(resp.Body)
		if err == nil && total > 0 && lo+int64(len(data)) > total {
			err = fmt.Errorf("upstream 206 longer than Content-Range total")
		}
	case http.StatusOK:
		// Range ignored: the body is the whole blob. Keep only this window
		// so the slice offsets stay correct.
		c.fullBodies.Add(1)
		var all []byte
		all, err = io.ReadAll(resp.Body)
		if err == nil {
			total = int64(len(all))
			if lo >= total {
				err = fmt.Errorf("window start %d beyond blob size %d", lo, total)
			} else {
				data = all[lo:min(hi+1, total)]
			}
		}
	default:
		err = fmt.Errorf("upstream %d", resp.StatusCode)
	}
	return data, total, err
}
