// Copyright 2026 Google LLC
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

package demo

import (
	"context"
	"path"
	"strings"
	"testing"
	"time"

	"go.ate.dev/substrate/internal/e2e"
	"go.ate.dev/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// A digest reference names a repository as well as a digest. An image cached
// on the node from one repository must not be served to a template that
// names the same digest under a repository the caller cannot pull from.
func TestDigestImageRefNeedsRepositoryAccess(t *testing.T) {
	t.Parallel()
	nsObj := e2e.CreateNamespace(t)
	ctx := context.Background()
	clients := e2e.GetClients()
	env, err := e2e.CheckEnv("BUCKET_NAME")
	if err != nil {
		t.Fatalf("CheckEnv failed: %v", err)
	}

	// One worker, so the golden build caches the counter image on the node
	// every actor of this pool runs on.
	labels := map[string]string{"demo": nsObj.Name}
	legit := e2e.CreateSubstrateCounterTemplate(ctx, t, clients, nsObj.Name, e2e.SubstrateTemplateOptions{
		Atespace:     demoAtespace,
		Name:         "digest-source-" + nsObj.Name,
		PoolName:     "digest",
		PoolReplicas: 1,
		Labels:       labels,
		SnapshotConfig: &ateapipb.SnapshotConfig{
			StorageLocation:   "gs://" + env["BUCKET_NAME"] + "/ate-demo-digest-" + nsObj.Name,
			PreferredFidelity: ateapipb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		},
	})

	image := legit.GetContainers()[0].GetImage()
	repo, digest, ok := strings.Cut(image, "@")
	if !ok {
		t.Fatalf("counter fixture image %q is not a digest reference", image)
	}
	registry := path.Dir(repo)
	if registry == "." {
		registry = "example.invalid"
	}
	forgedImage := registry + "/e2e-never-pushed-" + nsObj.Name + "@" + digest

	forged := proto.CloneOf(legit)
	forgedName := "digest-forged-" + nsObj.Name
	forged.Metadata = &ateapipb.ResourceMetadata{Atespace: demoAtespace, Name: forgedName}
	forged.Status = nil
	forged.Containers[0].Image = forgedImage
	// The resume below is the cold boot that has to fail; a golden build
	// would only reach the same code path first.
	forged.SnapshotConfig.GoldenSnapshotConfig = &ateapipb.GoldenSnapshotConfig{Mode: ateapipb.GoldenSnapshotMode_GOLDEN_SNAPSHOT_MODE_DISABLED}
	if _, err := clients.SubstrateAPI.CreateActorTemplate(ctx, &ateapipb.CreateActorTemplateRequest{ActorTemplate: forged}); err != nil {
		t.Fatalf("failed to create ActorTemplate %s/%s: %v", demoAtespace, forgedName, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := clients.SubstrateAPI.DeleteActorTemplate(cleanupCtx, &ateapipb.DeleteActorTemplateRequest{
			ActorTemplate: &ateapipb.ObjectRef{Atespace: demoAtespace, Name: forgedName},
		}); err != nil && status.Code(err) != codes.NotFound {
			t.Logf("failed to delete ActorTemplate %s/%s: %v", demoAtespace, forgedName, err)
		}
	})

	actorName := "digest-forged-" + nsObj.Name
	ref := &ateapipb.ObjectRef{Atespace: demoAtespace, Name: actorName}
	if _, err := clients.SubstrateAPI.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:      &ateapipb.ResourceMetadata{Atespace: demoAtespace, Name: actorName},
		ActorTemplate: e2e.TemplateRef(forged),
	}}); err != nil {
		t.Fatalf("failed to create Actor %q: %v", actorName, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if _, err := clients.SubstrateAPI.DeleteActor(cleanupCtx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil && status.Code(err) != codes.NotFound {
			t.Logf("failed to delete Actor %q: %v", actorName, err)
		}
	})
	waitForActorState(ctx, t, clients, actorName, ateapipb.ActorState_ACTOR_STATE_SUSPENDED)

	t.Logf("Resuming Actor %q from %s", actorName, forgedImage)
	_, err = e2e.ResumeActorAwaitCapacity(t, ctx, clients, &ateapipb.ResumeActorRequest{Actor: ref})
	if err == nil {
		t.Fatalf("ResumeActor ran %s, which no registry ever served under that name", forgedImage)
	}
	t.Logf("ResumeActor refused: %v", err)
	if actor := getActor(ctx, t, clients, actorName); actor.GetStatus().GetState() == ateapipb.ActorState_ACTOR_STATE_RUNNING {
		t.Fatalf("Actor %q is RUNNING from %s", actorName, forgedImage)
	}
}
