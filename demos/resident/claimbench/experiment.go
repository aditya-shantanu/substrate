// Copyright 2026 The Agent Substrate Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.ate.dev/substrate/api/ateapi/v1"
)

// guestInfo is one guest image with its two templates.
type guestInfo struct {
	Label    string `json:"label"`
	Image    string `json:"image"`
	SizeMB   int64  `json:"size_mb"`
	TouchMiB int    `json:"touch_mib"`
	Plain    string `json:"plain"`
	Pooled   string `json:"pooled"`
}

// variantSummary is the percentiles of one template's claims.
type variantSummary struct {
	Template string             `json:"template"`
	N        int                `json:"n"`
	Errors   int                `json:"errors"`
	Adopted  int                `json:"adopted"`
	Create   map[string]float64 `json:"create"`
	Resume   map[string]float64 `json:"resume"`
	First    map[string]float64 `json:"first"`
	Total    map[string]float64 `json:"total"`
}

type guestResult struct {
	guestInfo
	PlainResult  variantSummary `json:"plain_result"`
	PooledResult variantSummary `json:"pooled_result"`
}

type experimentSpec struct {
	Guests     []string `json:"guests"`
	N          int      `json:"n"`
	IntervalMs int      `json:"interval_ms"`
}

type experimentResult struct {
	Started  time.Time      `json:"started"`
	Finished time.Time      `json:"finished"`
	Spec     experimentSpec `json:"spec"`
	Guests   []guestResult  `json:"guests"`
	Claims   []claim        `json:"claims"`
}

// experiments holds the one experiment that may run at a time and the last
// finished one.
type experiments struct {
	mu      sync.Mutex
	running bool
	last    *experimentResult
	sizes   map[string]int64
}

// guests lists the atespace's guests, ordered by image size.
func (b *bench) guests(ctx context.Context) ([]guestInfo, error) {
	names, err := b.templateNames(ctx)
	if err != nil {
		return nil, err
	}
	byLabel := map[string]*guestInfo{}
	for _, n := range names {
		label, variant := n, ""
		for _, suffix := range []string{"-pooled", "-plain"} {
			if strings.HasSuffix(n, suffix) {
				label, variant = strings.TrimSuffix(n, suffix), suffix[1:]
			}
		}
		g := byLabel[label]
		if g == nil {
			g = &guestInfo{Label: label}
			byLabel[label] = g
		}
		if variant == "pooled" {
			g.Pooled = n
		} else {
			g.Plain = n
		}
		if g.Image == "" {
			t, err := b.control.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: &ateapi.ObjectRef{Atespace: *atespace, Name: n}})
			if err != nil {
				return nil, err
			}
			g.Image, g.TouchMiB = describeTemplate(t)
			if size, ok := knownSizes()[label]; ok && size > 0 {
				g.SizeMB = size
			} else {
				g.SizeMB = b.imageSizeMB(ctx, g.Image)
			}
		}
	}
	var out []guestInfo
	for _, g := range byLabel {
		if g.Plain != "" && g.Pooled != "" {
			out = append(out, *g)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SizeMB != out[j].SizeMB {
			return out[i].SizeMB < out[j].SizeMB
		}
		return out[i].Label < out[j].Label
	})
	return out, nil
}

// imageSizeMB is the compressed size of the image's layers, 0 when the
// registry cannot be read from here. Cached per reference.
func (b *bench) imageSizeMB(ctx context.Context, image string) int64 {
	b.exp.mu.Lock()
	if size, ok := b.exp.sizes[image]; ok {
		b.exp.mu.Unlock()
		return size
	}
	b.exp.mu.Unlock()
	var size int64
	ref, err := name.ParseReference(image)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.NewMultiKeychain(authn.DefaultKeychain, google.Keychain)))
		if err == nil {
			if m, err := img.Manifest(); err == nil {
				for _, l := range m.Layers {
					size += l.Size
				}
			}
		}
	}
	size /= 1e6
	b.exp.mu.Lock()
	if b.exp.sizes == nil {
		b.exp.sizes = map[string]int64{}
	}
	b.exp.sizes[image] = size
	b.exp.mu.Unlock()
	return size
}

func summarize(template string, cs []claim) variantSummary {
	s := variantSummary{Template: template, Create: map[string]float64{}, Resume: map[string]float64{}, First: map[string]float64{}, Total: map[string]float64{}}
	var ok []claim
	for _, c := range cs {
		if c.Error != "" {
			s.Errors++
			continue
		}
		ok = append(ok, c)
		if c.Source == "resident" {
			s.Adopted++
		}
	}
	s.N = len(cs)
	fill := func(m map[string]float64, f func(claim) float64) {
		xs := make([]float64, 0, len(ok))
		for _, c := range ok {
			xs = append(xs, f(c))
		}
		m["p50"], m["p90"], m["max"] = pct(xs, 50), pct(xs, 90), pct(xs, 100)
	}
	fill(s.Create, func(c claim) float64 { return c.CreateMs })
	fill(s.Resume, func(c claim) float64 { return c.ResumeMs })
	fill(s.First, func(c claim) float64 { return c.FirstMs })
	fill(s.Total, func(c claim) float64 { return c.TotalMs })
	return s
}

// handleExperiment runs the plain template of every chosen guest, then the
// pooled one, n paced claims each, streaming every claim and then the
// summary as server-sent events. One experiment runs at a time.
func (b *bench) handleExperiment(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	n, _ := strconv.Atoi(q.Get("n"))
	intervalMs, _ := strconv.Atoi(q.Get("interval_ms"))
	if n <= 0 || n > 500 {
		http.Error(w, "n must be 1 to 500", http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	all, err := b.guests(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	want := q.Get("guests")
	var chosen []guestInfo
	for _, g := range all {
		if want == "" || want == "all" || contains(strings.Split(want, ","), g.Label) {
			chosen = append(chosen, g)
		}
	}
	if len(chosen) == 0 {
		http.Error(w, "no such guest", http.StatusBadRequest)
		return
	}
	b.exp.mu.Lock()
	if b.exp.running {
		b.exp.mu.Unlock()
		http.Error(w, "an experiment is already running", http.StatusConflict)
		return
	}
	b.exp.running = true
	b.exp.mu.Unlock()
	defer func() {
		b.exp.mu.Lock()
		b.exp.running = false
		b.exp.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	send := func(event string, v any) {
		data, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
		flusher.Flush()
	}
	spec := experimentSpec{N: n, IntervalMs: intervalMs}
	var order []string
	for _, g := range chosen {
		spec.Guests = append(spec.Guests, g.Label)
		order = append(order, g.Plain)
	}
	for _, g := range chosen {
		order = append(order, g.Pooled)
	}
	result := &experimentResult{Started: time.Now(), Spec: spec}
	send("start", map[string]any{"guests": chosen, "templates": order, "spec": spec})
	byTemplate := map[string][]claim{}
	for i, template := range order {
		if r.Context().Err() != nil {
			return
		}
		send("template", map[string]any{"template": template, "index": i, "of": len(order)})
		b.runMany(r.Context(), template, n, 1, time.Duration(intervalMs)*time.Millisecond, func(c claim) {
			byTemplate[template] = append(byTemplate[template], c)
			result.Claims = append(result.Claims, c)
			send("claim", c)
		})
	}
	for _, g := range chosen {
		result.Guests = append(result.Guests, guestResult{guestInfo: g, PlainResult: summarize(g.Plain, byTemplate[g.Plain]), PooledResult: summarize(g.Pooled, byTemplate[g.Pooled])})
	}
	result.Finished = time.Now()
	b.exp.mu.Lock()
	b.exp.last = result
	b.exp.mu.Unlock()
	send("done", result)
}

func (b *bench) handleLastExperiment(w http.ResponseWriter, r *http.Request) {
	b.exp.mu.Lock()
	last := b.exp.last
	b.exp.mu.Unlock()
	if last == nil {
		http.Error(w, "no experiment has run", http.StatusNotFound)
		return
	}
	writeJSON(w, last)
}

func (b *bench) handleGuests(w http.ResponseWriter, r *http.Request) {
	gs, err := b.guests(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, gs)
}

// knownSizes parses --guest-sizes.
func knownSizes() map[string]int64 {
	out := map[string]int64{}
	for _, part := range strings.Split(*guestSizes, ",") {
		label, mb, ok := strings.Cut(part, "=")
		if !ok {
			continue
		}
		if v, err := strconv.ParseInt(mb, 10, 64); err == nil {
			out[label] = v
		}
	}
	return out
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if strings.TrimSpace(v) == x {
			return true
		}
	}
	return false
}
