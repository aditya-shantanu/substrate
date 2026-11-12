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

// Command claimbench measures how long a fresh actor takes from CreateActor
// to its first HTTP response, template by template, and serves a page that
// shows the claims as they happen. It runs in the cluster: it reaches
// ate-api-server with a projected ServiceAccount token and the actors through
// atenet-router with the pod's identity certificate.
package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.ate.dev/substrate/api/ateapi/v1"
	"go.ate.dev/substrate/internal/atenet"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

//go:embed ui.html
var uiHTML []byte

var (
	listen         = flag.String("listen", ":8080", "address of the page and the JSON API")
	atespace       = flag.String("atespace", "ate-demo-resident", "atespace the claimed actors are created in")
	templates      = flag.String("templates", "", "comma-separated ActorTemplates to claim from; empty lists the atespace's <guest>-pooled and <guest>-plain pairs")
	controlAddr    = flag.String("control-addr", "api.ate-system.svc.cluster.local:443", "ate-api-server address")
	apiServerName  = flag.String("api-server-name", "api.ate-system.svc", "name on ate-api-server's serving certificate")
	routerAddr     = flag.String("router-addr", "atenet-router.ate-system.svc.cluster.local:443", "atenet-router HTTPS address")
	routerName     = flag.String("router-server-name", "atenet-router.ate-system.svc", "name on the router's serving certificate")
	tokenFile      = flag.String("token-file", "/var/run/secrets/ateapi/token", "projected ServiceAccount token with ate-api-server's audience")
	clientCertFile = flag.String("client-cert", "/run/podidentity/credential-bundle.pem", "pod identity credential bundle the router admits")
	caFile         = flag.String("ca-file", "/run/servicedns-ca/trust-bundle.pem", "servicedns trust bundle that signs both serving certificates")
	runSpec        = flag.String("run", "", "headless: comma-separated template=count to claim serially, then exit")
	out            = flag.String("out", "", "headless: file the claims are appended to as JSON lines")
	firstTimeout   = flag.Duration("first-response-timeout", 30*time.Second, "how long to wait for the actor's first HTTP response")
	interval       = flag.Duration("interval", 0, "headless: pause between claims, the arrival rate a pool has to keep up with")
	guestSizes     = flag.String("guest-sizes", "", "compressed image size per guest as label=MB,..., for registries this pod cannot read")
)

// claim is one measured actor: created, resumed, and reached once.
type claim struct {
	Template string  `json:"template"`
	Actor    string  `json:"actor"`
	Source   string  `json:"source"` // "resident" when the resume adopted a pooled sandbox, else "restore"
	Worker   string  `json:"worker"`
	CreateMs float64 `json:"create_ms"`
	ResumeMs float64 `json:"resume_ms"`
	FirstMs  float64 `json:"first_ms"`
	TotalMs  float64 `json:"total_ms"`
	Attempts int     `json:"attempts"`
	Error    string  `json:"error,omitempty"`
	At       string  `json:"at"`
}

type bench struct {
	control ateapi.ControlClient
	router  *http.Client
	mu      sync.Mutex
	recent  []claim
	exp     experiments
}

func main() {
	flag.Parse()
	ctx := context.Background()
	b, err := newBench(ctx)
	if err != nil {
		slog.Error("starting", slog.Any("err", err))
		os.Exit(1)
	}
	if *runSpec != "" {
		if err := b.headless(ctx, *runSpec); err != nil {
			slog.Error("headless run failed", slog.Any("err", err))
			os.Exit(1)
		}
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(uiHTML)
	})
	mux.HandleFunc("/api/state", b.handleState)
	mux.HandleFunc("/api/run", b.handleRun)
	mux.HandleFunc("/api/guests", b.handleGuests)
	mux.HandleFunc("/api/experiment", b.handleExperiment)
	mux.HandleFunc("/api/experiment/last", b.handleLastExperiment)
	mux.HandleFunc("/api/recent", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		defer b.mu.Unlock()
		writeJSON(w, b.recent)
	})
	slog.Info("serving", slog.String("listen", *listen), slog.String("templates", *templates))
	srv := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("serving", slog.Any("err", err))
		os.Exit(1)
	}
}

func newBench(ctx context.Context) (*bench, error) {
	caPEM, err := os.ReadFile(*caFile)
	if err != nil {
		return nil, fmt.Errorf("reading the trust bundle: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("%s holds no certificates", *caFile)
	}
	conn, err := grpc.NewClient(*controlAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: *apiServerName})),
		grpc.WithPerRPCCredentials(fileToken{path: *tokenFile}))
	if err != nil {
		return nil, fmt.Errorf("dialing ate-api-server: %w", err)
	}
	certs := &bundleCerts{path: *clientCertFile}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion:           tls.VersionTLS12,
		RootCAs:              roots,
		ServerName:           *routerName,
		GetClientCertificate: certs.get,
	}
	transport.MaxIdleConnsPerHost = 64
	return &bench{
		control: ateapi.NewControlClient(conn),
		router:  &http.Client{Transport: transport, Timeout: 10 * time.Second},
	}, nil
}

// fileToken sends the current contents of a token file, so rotations are
// picked up.
type fileToken struct{ path string }

func (t fileToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return nil, fmt.Errorf("reading the bearer token: %w", err)
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, nil
}

func (fileToken) RequireTransportSecurity() bool { return true }

// bundleCerts loads the pod identity credential bundle on each handshake,
// at most once a minute, so a renewed certificate is used.
type bundleCerts struct {
	path   string
	mu     sync.Mutex
	cert   *tls.Certificate
	loaded time.Time
}

func (c *bundleCerts) get(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cert != nil && time.Since(c.loaded) < time.Minute {
		return c.cert, nil
	}
	pemBytes, err := os.ReadFile(c.path)
	if err != nil {
		return nil, fmt.Errorf("reading the credential bundle: %w", err)
	}
	cert, err := tls.X509KeyPair(pemBytes, pemBytes)
	if err != nil {
		return nil, fmt.Errorf("parsing the credential bundle: %w", err)
	}
	c.cert, c.loaded = &cert, time.Now()
	return c.cert, nil
}

// claimOne creates an actor from template, resumes it, waits for its first
// HTTP 200 through the router, and deletes it in the background.
func (b *bench) claimOne(ctx context.Context, template string) claim {
	name := "c-" + strings.ToLower(rand.Text()[:8])
	ref := &ateapi.ObjectRef{Atespace: *atespace, Name: name}
	c := claim{Template: template, Actor: name, Source: "restore", At: time.Now().UTC().Format(time.RFC3339Nano)}
	start := time.Now()
	fail := func(err error) claim {
		c.Error = err.Error()
		c.TotalMs = ms(time.Since(start))
		return c
	}

	if _, err := b.control.CreateActor(ctx, &ateapi.CreateActorRequest{Actor: &ateapi.Actor{
		Metadata:      &ateapi.ResourceMetadata{Atespace: *atespace, Name: name},
		ActorTemplate: &ateapi.ObjectRef{Atespace: *atespace, Name: template},
	}}); err != nil {
		return fail(fmt.Errorf("CreateActor: %w", err))
	}
	created := time.Now()
	c.CreateMs = ms(created.Sub(start))
	defer b.deleteLater(ref)

	resp, err := b.control.ResumeActor(ctx, &ateapi.ResumeActorRequest{Actor: ref})
	if err != nil {
		return fail(fmt.Errorf("ResumeActor: %w", err))
	}
	resumed := time.Now()
	c.ResumeMs = ms(resumed.Sub(created))
	if resp.GetActor().GetStatus().GetResident().GetState() == ateapi.ResidentState_RESIDENT_STATE_ADOPTED {
		c.Source = "resident"
	}
	c.Worker = resp.GetActor().GetStatus().GetWorkerAssignment().GetWorkerPod()

	deadline := time.Now().Add(*firstTimeout)
	for {
		c.Attempts++
		status, err := b.get(ctx, ref)
		if err == nil && status == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("status %d", status)
			}
			return fail(fmt.Errorf("first response: %w", err))
		}
		time.Sleep(20 * time.Millisecond)
	}
	c.FirstMs = ms(time.Since(resumed))
	c.TotalMs = ms(time.Since(start))
	return c
}

func (b *bench) get(ctx context.Context, ref *ateapi.ObjectRef) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+*routerAddr+"/", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set(atenet.TargetActorHeader, ref.GetAtespace()+"/"+ref.GetName())
	resp, err := b.router.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

func (b *bench) deleteLater(ref *ateapi.ObjectRef) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if _, err := b.control.DeleteActor(ctx, &ateapi.DeleteActorRequest{Actor: ref}); err != nil {
			slog.Warn("deleting a claimed actor", slog.String("actor", ref.GetName()), slog.Any("err", err))
		}
	}()
}

func (b *bench) record(c claim) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append(b.recent, c)
	if len(b.recent) > 200 {
		b.recent = b.recent[len(b.recent)-200:]
	}
}

// handleRun claims n actors from template, concurrency at a time, and streams
// each claim as a server-sent event.
func (b *bench) handleRun(w http.ResponseWriter, r *http.Request) {
	template := r.URL.Query().Get("template")
	n, _ := strconv.Atoi(r.URL.Query().Get("n"))
	concurrency, _ := strconv.Atoi(r.URL.Query().Get("concurrency"))
	intervalMs, _ := strconv.Atoi(r.URL.Query().Get("interval_ms"))
	if template == "" || n <= 0 {
		http.Error(w, "template and n are required", http.StatusBadRequest)
		return
	}
	if concurrency <= 0 {
		concurrency = 1
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	results := make(chan claim)
	go func() {
		defer close(results)
		b.runMany(r.Context(), template, n, concurrency, time.Duration(intervalMs)*time.Millisecond, func(c claim) { results <- c })
	}()
	for c := range results {
		data, _ := json.Marshal(c)
		fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
	fmt.Fprint(w, "event: done\ndata: {}\n\n")
	flusher.Flush()
}

// runMany claims n actors from template, at most concurrency at a time and
// at least pause apart.
func (b *bench) runMany(ctx context.Context, template string, n, concurrency int, pause time.Duration, emit func(claim)) {
	var wg sync.WaitGroup
	sem := make(chan struct{}, concurrency)
	var mu sync.Mutex
	for i := range n {
		if ctx.Err() != nil {
			break
		}
		if i > 0 && pause > 0 {
			time.Sleep(pause)
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			c := b.claimOne(ctx, template)
			b.record(c)
			mu.Lock()
			emit(c)
			mu.Unlock()
		}()
	}
	wg.Wait()
}

type templateState struct {
	Name     string `json:"name"`
	Guest    string `json:"guest"`
	Image    string `json:"image"`
	TouchMiB int    `json:"touch_mib"`
	Replicas int32  `json:"replicas"`
	Ready    int32  `json:"ready"`
	Pending  int32  `json:"pending"`
	Golden   bool   `json:"golden"`
	Error    string `json:"error,omitempty"`
}

// templateNames returns the templates to show: the --templates list, or every
// <guest>-pooled and <guest>-plain template in the atespace.
func (b *bench) templateNames(ctx context.Context) ([]string, error) {
	if *templates != "" {
		return strings.Split(*templates, ","), nil
	}
	var names []string
	for token := ""; ; {
		resp, err := b.control.ListActorTemplates(ctx, &ateapi.ListActorTemplatesRequest{Atespace: *atespace, Options: &ateapi.ListOptions{PageToken: token}})
		if err != nil {
			return nil, err
		}
		for _, t := range resp.GetActorTemplates() {
			if name := t.GetMetadata().GetName(); strings.HasSuffix(name, "-pooled") || strings.HasSuffix(name, "-plain") {
				names = append(names, name)
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}
	sort.Strings(names)
	return names, nil
}

// describeTemplate reads the guest's image and held memory off the template.
func describeTemplate(t *ateapi.ActorTemplate) (image string, touchMiB int) {
	for _, ctr := range t.GetContainers() {
		image = ctr.GetImage()
		for _, arg := range ctr.GetCommand() {
			if v, ok := strings.CutPrefix(arg, "--touch-memory-mb="); ok {
				touchMiB, _ = strconv.Atoi(v)
			}
		}
	}
	return image, touchMiB
}

type workerState struct {
	Name     string `json:"name"`
	Pod      string `json:"pod"`
	Actors   int32  `json:"actors"`
	Capacity int32  `json:"capacity"`
}

func (b *bench) handleState(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	names, err := b.templateNames(ctx)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	var ts []templateState
	for _, name := range names {
		t, err := b.control.GetActorTemplate(ctx, &ateapi.GetActorTemplateRequest{ActorTemplate: &ateapi.ObjectRef{Atespace: *atespace, Name: name}})
		guest := strings.TrimSuffix(strings.TrimSuffix(name, "-pooled"), "-plain")
		st := templateState{Name: name, Guest: guest}
		if err != nil {
			st.Error = err.Error()
		} else {
			st.Image, st.TouchMiB = describeTemplate(t)
			st.Replicas = t.GetResidentPool().GetReplicas()
			st.Ready = t.GetStatus().GetResidentPoolStatus().GetReady()
			st.Pending = t.GetStatus().GetResidentPoolStatus().GetPending()
			st.Golden = t.GetStatus().GetGoldenSnapshotStatus().GetGoldenTag() != nil
			st.Error = t.GetStatus().GetResidentPoolStatus().GetErrorMessage()
		}
		ts = append(ts, st)
	}
	var ws []workerState
	resp, err := b.control.ListWorkers(ctx, &ateapi.ListWorkersRequest{})
	if err == nil {
		for _, wk := range resp.GetWorkers() {
			if !strings.HasPrefix(wk.GetWorkerPod(), "resident") {
				continue
			}
			ws = append(ws, workerState{
				Name:     wk.GetMetadata().GetName(),
				Pod:      wk.GetWorkerPod(),
				Actors:   wk.GetStatus().GetAllocated().GetActors(),
				Capacity: wk.GetStatus().GetCapacity().GetActors(),
			})
		}
		sort.Slice(ws, func(i, j int) bool { return ws[i].Pod < ws[j].Pod })
	}
	writeJSON(w, map[string]any{"templates": ts, "workers": ws, "atespace": *atespace})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// headless claims per the spec "template=count,..." one at a time and prints
// percentiles per template.
func (b *bench) headless(ctx context.Context, spec string) error {
	sink := io.Discard
	if *out != "" {
		f, err := os.OpenFile(*out, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		sink = f
	}
	byTemplate := map[string][]claim{}
	var order []string
	for _, part := range strings.Split(spec, ",") {
		template, countStr, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return fmt.Errorf("bad --run entry %q, want template=count", part)
		}
		n, err := strconv.Atoi(countStr)
		if err != nil {
			return fmt.Errorf("bad count in %q: %w", part, err)
		}
		if !slices.Contains(order, template) {
			order = append(order, template)
		}
		b.runMany(ctx, template, n, 1, *interval, func(c claim) {
			byTemplate[template] = append(byTemplate[template], c)
			line, _ := json.Marshal(c)
			fmt.Fprintln(sink, string(line))
			fmt.Printf("%-16s %-12s %-9s create %6.0f  resume %6.0f  first %6.0f  total %6.0f ms  %s\n",
				template, c.Actor, c.Source, c.CreateMs, c.ResumeMs, c.FirstMs, c.TotalMs, c.Error)
		})
	}
	fmt.Printf("\n%-16s %3s %5s %-18s %-18s %-18s %-22s\n", "template", "n", "err", "create p50/p90", "resume p50/p90", "first p50/p90", "total p50/p90/max (ms)")
	for _, template := range order {
		cs := byTemplate[template]
		var ok []claim
		errs := 0
		for _, c := range cs {
			if c.Error != "" {
				errs++
				continue
			}
			ok = append(ok, c)
		}
		pick := func(f func(claim) float64) (float64, float64, float64) {
			xs := make([]float64, 0, len(ok))
			for _, c := range ok {
				xs = append(xs, f(c))
			}
			return pct(xs, 50), pct(xs, 90), pct(xs, 100)
		}
		c50, c90, _ := pick(func(c claim) float64 { return c.CreateMs })
		r50, r90, _ := pick(func(c claim) float64 { return c.ResumeMs })
		f50, f90, _ := pick(func(c claim) float64 { return c.FirstMs })
		t50, t90, tmax := pick(func(c claim) float64 { return c.TotalMs })
		fmt.Printf("%-16s %3d %5d %6.0f/%-11.0f %6.0f/%-11.0f %6.0f/%-11.0f %6.0f/%.0f/%.0f\n",
			template, len(cs), errs, c50, c90, r50, r90, f50, f90, t50, t90, tmax)
	}
	return nil
}

func pct(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sorted := append([]float64(nil), xs...)
	sort.Float64s(sorted)
	i := int(p/100*float64(len(sorted)-1) + 0.5)
	return sorted[i]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

var _ = errors.New
