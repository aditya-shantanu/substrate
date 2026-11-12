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
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"

	"go.ate.dev/substrate/cmd/atelet/internal/ateletpath"
	"go.ate.dev/substrate/cmd/atelet/internal/trustbundle"
	"go.ate.dev/substrate/internal/apierror"
	"go.ate.dev/substrate/internal/proto/ateletpb"
	"go.ate.dev/substrate/internal/proto/ateworkerpb"
	"go.ate.dev/substrate/internal/resources"
)

// adoptFixture is an actor running on a fake worker under its previous name:
// its sandbox record is on the node, its system-info volumes are registered
// and written, and its JWT is cached.
type adoptFixture struct {
	s        *Atelet
	worker   *fakeWorker
	previous resources.ActorRef
	uid      string
	nameFile string
}

func newAdoptFixture(t *testing.T) *adoptFixture {
	t.Helper()
	useTempNodeDirs(t)
	const uid = "0f6c2b0e-1b7e-4b3f-9a6a-1d2c3e4f5a70"
	previous := resources.ActorRef{Atespace: "team-a", Name: "resident-1"}
	store := newCTBStore(t)
	store.set(t, string(testCertPEM(t)))
	refresher := newSystemInfoVolumeRefresher(trustbundle.NewSource(store.lister.Get, nil), nil)
	volumes := systemInfoVolumesFor(uid, &ateletpb.WorkloadSpec{Volumes: []*ateletpb.Volume{
		{Name: "trust", SystemInfo: trustVolumeSpec("ca.pem")},
		{Name: "meta", SystemInfo: metadataVolumeSpec()},
	}})
	if _, err := refresher.Register(uid, previous, volumes); err != nil {
		t.Fatal(err)
	}
	if err := writeSandboxRecord(uid, &sandboxAssetsRecord{SandboxClass: "gvisor"}); err != nil {
		t.Fatal(err)
	}
	jwts := newWorkerActorJWTs(&fakeJWTMinter{jwt: "resident-jwt", expiry: time.Now().Add(time.Hour)})
	if _, err := jwts.get(t.Context(), actorRef{atespace: previous.Atespace, name: previous.Name, uid: uid}); err != nil {
		t.Fatal(err)
	}
	worker := &fakeWorker{workerEpoch: 7}
	serveFakeWorker(t, worker)
	return &adoptFixture{
		s:        &Atelet{workerDialer: newWorkerDialer(1), systemInfoVolumes: refresher, workerActorJWTs: jwts},
		worker:   worker,
		previous: previous,
		uid:      uid,
		nameFile: filepath.Join(ateletpath.SystemInfoVolumeRoot(uid, "meta"), "actor-name"),
	}
}

func (f *adoptFixture) request() *ateletpb.AdoptRequest {
	return &ateletpb.AdoptRequest{
		WorkerPodUid:          "5f1c0d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f",
		Atespace:              f.previous.Atespace,
		ActorUid:              f.uid,
		PreviousActorName:     f.previous.Name,
		ActorName:             "client-1",
		ActorTemplateAtespace: "team-a",
		ActorTemplateName:     "tmpl-1",
		EgressGateway:         &ateletpb.EgressGateway{Address: "gateway:443"},
	}
}

// registeredName is the name the actor's system-info volumes are registered
// and written under.
func (f *adoptFixture) registeredName(t *testing.T) string {
	t.Helper()
	f.s.systemInfoVolumes.mu.Lock()
	actor := f.s.systemInfoVolumes.actors[f.uid]
	f.s.systemInfoVolumes.mu.Unlock()
	if actor == nil {
		t.Fatal("the actor's system-info volumes are not registered")
	}
	written, err := os.ReadFile(f.nameFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != actor.ref.Name {
		t.Fatalf("system-info volume names the actor %q, registration names it %q", written, actor.ref.Name)
	}
	return actor.ref.Name
}

func TestAdopt(t *testing.T) {
	t.Run("renames the node state and the workload", func(t *testing.T) {
		f := newAdoptFixture(t)
		resp, err := f.s.Adopt(t.Context(), f.request())
		if err != nil {
			t.Fatalf("Adopt: %v", err)
		}
		if resp.GetWorkerEpoch() != 7 {
			t.Errorf("WorkerEpoch = %d, want the worker's 7", resp.GetWorkerEpoch())
		}
		want := &ateworkerpb.AdoptWorkloadRequest{
			Atespace:              "team-a",
			ActorUid:              f.uid,
			PreviousActorName:     "resident-1",
			ActorName:             "client-1",
			ActorTemplateAtespace: "team-a",
			ActorTemplateName:     "tmpl-1",
			EgressGateway:         &ateworkerpb.EgressGateway{Address: "gateway:443"},
		}
		if diff := cmp.Diff(want, f.worker.adopted, protocmp.Transform()); diff != "" {
			t.Errorf("AdoptWorkload request (-want +got):\n%s", diff)
		}
		if got := f.registeredName(t); got != "client-1" {
			t.Errorf("system-info volumes are registered under %q, want client-1", got)
		}
		if _, ok := f.s.workerActorJWTs.cache.Get(f.uid); ok {
			t.Error("the actor's JWT minted for its previous name is still cached")
		}
	})
	t.Run("a worker failure keeps the previous name", func(t *testing.T) {
		f := newAdoptFixture(t)
		f.worker.adoptErr = status.Error(codes.Unimplemented, "microvm")
		_, err := f.s.Adopt(t.Context(), f.request())
		if apierror.Code(err) != codes.Unimplemented {
			t.Fatalf("Adopt error = %v, want the worker's Unimplemented", err)
		}
		if got := f.registeredName(t); got != "resident-1" {
			t.Errorf("system-info volumes are registered under %q, want resident-1", got)
		}
	})
	t.Run("no sandbox on this node", func(t *testing.T) {
		f := newAdoptFixture(t)
		if err := os.Remove(ateletpath.ActorSandboxAssetsFile(f.uid)); err != nil {
			t.Fatal(err)
		}
		_, err := f.s.Adopt(t.Context(), f.request())
		if apierror.Code(err) != codes.NotFound {
			t.Fatalf("Adopt error = %v, want NotFound", err)
		}
		if f.worker.adopted != nil {
			t.Error("the worker was asked to adopt an actor atelet never started")
		}
	})
}
