//go:build linux

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
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"

	"go.ate.dev/substrate/internal/actorlock"
	"go.ate.dev/substrate/internal/actorlog"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/atunnel"
	"go.ate.dev/substrate/internal/atunnel/egress"
	"go.ate.dev/substrate/internal/atunnel/ingress"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/resources"
	"go.ate.dev/substrate/internal/testcert"
)

const (
	adoptUID      = "0f6c2b0e-1b7e-4b3f-9a6a-1d2c3e4f5a70"
	adoptAtespace = "team-a"
	adoptPrevious = "resident-1"
	adoptName     = "client-1"
)

// adoptFixture is an actor hosted under its placeholder name, with its
// ingress registered on a tunnel that serves nothing. The egress proxy has
// no active egress for it, so a request naming a gateway fails the mint.
type adoptFixture struct {
	s        *AteWorkerService
	hosted   *hostedActor
	previous resources.ActorAttribution
}

func newAdoptFixture(t *testing.T) *adoptFixture {
	t.Helper()
	tunnel := &atunnel.Tunnel{Ingress: newTestIngress(t), Egress: newTestEgress(t)}
	s := &AteWorkerService{
		locks:       actorlock.New(),
		inFlight:    actorlock.NewInFlight(),
		actors:      map[string]*hostedActor{},
		tunnel:      tunnel,
		actorLogger: actorlog.NewActorLogger(io.Discard, false),
	}
	s.epoch.Store(7)
	previous := resources.ActorAttribution{
		Ref: resources.ActorRef{Atespace: adoptAtespace, Name: adoptPrevious}, UID: adoptUID,
		TemplateAtespace: adoptAtespace, TemplateName: "tmpl-1",
	}
	hosted := newHostedActor(previous)
	hosted.dnsPolicy = tunnel.ActorEgressPolicy(previous)
	s.actors[adoptUID] = hosted
	if err := tunnel.ActivateIngress(previous, s.sandboxDialer(adoptUID)); err != nil {
		t.Fatal(err)
	}
	return &adoptFixture{s: s, hosted: hosted, previous: previous}
}

func newTestIngress(t *testing.T) *ingress.Server {
	t.Helper()
	dir := t.TempDir()
	certs := testcert.NewServerTLS(t, net.IPv4(127, 0, 0, 1))
	bundle := filepath.Join(dir, "bundle.pem")
	trust := filepath.Join(dir, "trust.pem")
	if err := os.WriteFile(bundle, append(certs.Certificate, certs.PrivateKey...), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trust, certs.RootCA, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := ingress.NewServer(ingress.Config{
		CredentialBundlePath: bundle,
		TrustBundlePath:      trust,
		AllowedClientID:      "spiffe://cluster.local/ns/ate-system/sa/atenet-router",
		Upstream:             &url.URL{Scheme: "http", Host: "actor.internal:80"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func newTestEgress(t *testing.T) *egress.Proxy {
	t.Helper()
	proxy, err := egress.New(func(net.Conn) (string, error) { return "", errors.New("no original destination in tests") })
	if err != nil {
		t.Fatal(err)
	}
	return proxy
}

func (f *adoptFixture) request() *ateworkerpb.AdoptWorkloadRequest {
	return &ateworkerpb.AdoptWorkloadRequest{
		Atespace:              adoptAtespace,
		ActorUid:              adoptUID,
		PreviousActorName:     adoptPrevious,
		ActorName:             adoptName,
		ActorTemplateAtespace: adoptAtespace,
		ActorTemplateName:     "tmpl-1",
	}
}

// adopted is the attribution the actor carries after the adopt.
func (f *adoptFixture) adopted() resources.ActorAttribution {
	a := f.previous
	a.Ref.Name = adoptName
	return a
}

// ingressActive reports whether the actor's ingress is registered under name.
// Activate refuses a name the actor is already active under, and a probe that
// got through is undone.
func (f *adoptFixture) ingressActive(t *testing.T, name string) bool {
	t.Helper()
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, net.ErrClosed }
	err := f.s.tunnel.Ingress.Activate(adoptAtespace, name, adoptUID, dial)
	if err == nil {
		if err := f.s.tunnel.Ingress.Deactivate(t.Context(), adoptAtespace, name, adoptUID); err != nil {
			t.Fatal(err)
		}
		return false
	}
	if !strings.Contains(err.Error(), "already active") {
		t.Fatalf("probing ingress under %q: %v", name, err)
	}
	return true
}

// expectNames checks the name the actor carries and the name its ingress is
// registered under.
func (f *adoptFixture) expectNames(t *testing.T, want resources.ActorAttribution) {
	t.Helper()
	if got := f.hosted.attribution(); got != want {
		t.Errorf("hosted attribution = %+v, want %+v", got, want)
	}
	other := adoptPrevious
	if want.Ref.Name == adoptPrevious {
		other = adoptName
	}
	if !f.ingressActive(t, want.Ref.Name) {
		t.Errorf("ingress is not registered under %q", want.Ref.Name)
	}
	if f.ingressActive(t, other) {
		t.Errorf("ingress is still registered under %q", other)
	}
}

func TestAdoptWorkload(t *testing.T) {
	t.Run("moves ingress and the attribution to the new name", func(t *testing.T) {
		f := newAdoptFixture(t)
		resp, err := f.s.AdoptWorkload(t.Context(), f.request())
		if err != nil {
			t.Fatalf("AdoptWorkload: %v", err)
		}
		if resp.GetWorkerEpoch() != 7 {
			t.Errorf("WorkerEpoch = %d, want 7", resp.GetWorkerEpoch())
		}
		f.expectNames(t, f.adopted())
	})
	t.Run("a retry after the adopt completed succeeds", func(t *testing.T) {
		f := newAdoptFixture(t)
		for range 2 {
			if _, err := f.s.AdoptWorkload(t.Context(), f.request()); err != nil {
				t.Fatalf("AdoptWorkload: %v", err)
			}
		}
		f.expectNames(t, f.adopted())
	})
	for name, tc := range map[string]struct {
		mutate func(*ateworkerpb.AdoptWorkloadRequest)
		want   codes.Code
	}{
		"the actor is not hosted": {
			mutate: func(r *ateworkerpb.AdoptWorkloadRequest) { r.ActorUid = "1f6c2b0e-1b7e-4b3f-9a6a-1d2c3e4f5a71" },
			want:   codes.NotFound,
		},
		"the previous name is not the one hosted": {
			mutate: func(r *ateworkerpb.AdoptWorkloadRequest) { r.PreviousActorName = "resident-2" },
			want:   codes.FailedPrecondition,
		},
		"the atespace is not the one hosted": {
			mutate: func(r *ateworkerpb.AdoptWorkloadRequest) { r.Atespace = "team-b" },
			want:   codes.FailedPrecondition,
		},
		// The actor has no tunneled egress, so there is no certificate to
		// mint again. Ingress was already moved and is put back.
		"a failed egress adopt puts the previous name back": {
			mutate: func(r *ateworkerpb.AdoptWorkloadRequest) {
				r.EgressGateway = &ateworkerpb.EgressGateway{Address: "gateway:443"}
			},
			want: codes.FailedPrecondition,
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newAdoptFixture(t)
			req := f.request()
			tc.mutate(req)
			_, err := f.s.AdoptWorkload(t.Context(), req)
			if apierror.Code(err) != tc.want {
				t.Fatalf("AdoptWorkload error = %v, want %v", err, tc.want)
			}
			f.expectNames(t, f.previous)
		})
	}
}

// A terminate that arrives under the placeholder name after an adopt the
// control plane never recorded retires the ingress under the adopted name
// too.
func TestTerminateWorkloadRetiresTheAdoptedName(t *testing.T) {
	f := newAdoptFixture(t)
	if _, err := f.s.AdoptWorkload(t.Context(), f.request()); err != nil {
		t.Fatalf("AdoptWorkload: %v", err)
	}
	dir := t.TempDir()
	actorDirs := &ateworkerpb.ActorDirs{RootDir: dir, OciBundleDir: filepath.Join(dir, "bundle")}
	if err := f.s.terminateWorkload(t.Context(), f.previous, actorDirs); err != nil {
		t.Fatalf("terminateWorkload: %v", err)
	}
	for _, name := range []string{adoptPrevious, adoptName} {
		if f.ingressActive(t, name) {
			t.Errorf("ingress is still registered under %q", name)
		}
	}
}
