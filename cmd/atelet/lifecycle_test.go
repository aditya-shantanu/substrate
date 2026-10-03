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

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/agent-substrate/substrate/cmd/atelet/internal/ateletpath"
	"github.com/agent-substrate/substrate/internal/nodepath"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
	"github.com/agent-substrate/substrate/internal/proto/ateompb"
	"github.com/agent-substrate/substrate/pkg/objectstorage"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// useTempNodeDirs roots atelet's on-node state in temp directories so a test
// can drive the real filesystem layout. Not parallel-safe: the paths are
// process-global.
func useTempNodeDirs(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	origActors, origStatic := nodepath.ActorsDir, nodepath.StaticFilesDir
	nodepath.ActorsDir = filepath.Join(root, "actors")
	nodepath.StaticFilesDir = filepath.Join(root, "static-files")
	t.Cleanup(func() {
		nodepath.ActorsDir, nodepath.StaticFilesDir = origActors, origStatic
	})
}

// fakeAteom is a fake ateom in a worker pod. It writes the files a
// real checkpoint would leave in the checkpoint dir, and reads back what a
// restore was handed. Like a real ateom it takes every actor directory from
// the request, never derived from the actor UID.
type fakeAteom struct {
	ateompb.UnimplementedAteomServer
	// snapshotFiles are written at checkpoint and reported back to atelet as
	// the exact set the snapshot consists of.
	snapshotFiles map[string]string
	// restored holds the file contents staged into the restore dir by the
	// most recent RestoreWorkload.
	restored map[string]string
	// actorDirs records the ActorDirs each RPC arrived with, by RPC name.
	actorDirs map[string]*ateompb.ActorDirs
}

func (f *fakeAteom) recordActorDirs(rpc string, actorDirs *ateompb.ActorDirs) {
	if f.actorDirs == nil {
		f.actorDirs = map[string]*ateompb.ActorDirs{}
	}
	f.actorDirs[rpc] = actorDirs
}

func (f *fakeAteom) RunWorkload(_ context.Context, req *ateompb.RunWorkloadRequest) (*ateompb.RunWorkloadResponse, error) {
	f.recordActorDirs("RunWorkload", req.GetActorDirs())
	return &ateompb.RunWorkloadResponse{}, nil
}

func (f *fakeAteom) CheckpointWorkload(_ context.Context, req *ateompb.CheckpointWorkloadRequest) (*ateompb.CheckpointWorkloadResponse, error) {
	f.recordActorDirs("CheckpointWorkload", req.GetActorDirs())
	dir := req.GetActorDirs().GetCheckpointDir()
	names := make([]string, 0, len(f.snapshotFiles))
	for name, body := range f.snapshotFiles {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return &ateompb.CheckpointWorkloadResponse{SnapshotFiles: names}, nil
}

func (f *fakeAteom) RestoreWorkload(_ context.Context, req *ateompb.RestoreWorkloadRequest) (*ateompb.RestoreWorkloadResponse, error) {
	f.recordActorDirs("RestoreWorkload", req.GetActorDirs())
	dir := req.GetActorDirs().GetRestoreDir()
	f.restored = map[string]string{}
	for name := range f.snapshotFiles {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		f.restored[name] = string(body)
	}
	return &ateompb.RestoreWorkloadResponse{}, nil
}

func (f *fakeAteom) TerminateWorkload(_ context.Context, req *ateompb.TerminateWorkloadRequest) (*ateompb.TerminateWorkloadResponse, error) {
	f.recordActorDirs("TerminateWorkload", req.GetActorDirs())
	return &ateompb.TerminateWorkloadResponse{}, nil
}

// serveFakeAteom serves ateom on a unix socket and points atelet's dialer at
// it. The socket lives in its own short temp dir.
func serveFakeAteom(t *testing.T, f *fakeAteom) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ateom-")
	if err != nil {
		t.Fatalf("creating socket dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })

	sock := filepath.Join(dir, "ateom.sock")
	lis, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listening on %q: %v", sock, err)
	}
	srv := grpc.NewServer()
	ateompb.RegisterAteomServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	orig := ateomSocketPath
	ateomSocketPath = func(string) string { return sock }
	t.Cleanup(func() { ateomSocketPath = orig })
}

// TestLocalSnapshotGC walks an actor through
// run -> pause -> resume -> terminate over atelet's RPC surface and ensures that
// the local snapshot is garbage collected after the actor is terminated.
func TestLocalSnapshotGC(t *testing.T) {
	useTempNodeDirs(t)
	ctx := t.Context()

	const (
		atespace     = "ate-demo"
		actorName    = "counter"
		actorUID     = "actor-uid-1"
		ateomUID     = "ateom-uid-1"
		snapshotName = "pause-snap-1"
	)

	ateom := &fakeAteom{snapshotFiles: map[string]string{"checkpoint.img": "guest-memory"}}
	serveFakeAteom(t, ateom)

	host := imageVolumeTestRegistry(t)
	image := host + "/actor:v1"
	pushTestImage(t, image, singleFileLayer(t, "bin/app", "app"))

	// A single "runsc" asset served from a fake bucket: enough to exercise the
	// content-addressed asset fetch without a gVisor release tarball.
	runsc := []byte("runsc binary")
	s := &AteomHerder{
		ateomDialer:       newAteomDialer(1),
		imageCache:        newImageVolumeStore(t),
		anonGCSClient:     fakeObjectStorage{data: runsc},
		systemInfoVolumes: newSystemInfoVolumeRefresher(nil, nil),
	}
	sandboxAssets := &ateletpb.SandboxAssets{
		SandboxClass: "gvisor",
		PauseImage:   image,
		Assets: map[string]*ateletpb.ArchAssets{
			runtime.GOARCH: {Files: map[string]*ateletpb.AssetFile{
				runscAssetName: {
					Url:    "gs://test-bucket/runsc",
					Sha256: fmt.Sprintf("%x", sha256.Sum256(runsc)),
				},
			}},
		},
	}
	spec := &ateletpb.WorkloadSpec{
		Containers: []*ateletpb.Container{{Name: "app", Image: image, Command: []string{"/bin/app"}}},
	}

	if _, err := s.Run(ctx, &ateletpb.RunRequest{
		Atespace:              atespace,
		ActorName:             actorName,
		ActorUid:              actorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		TargetAteomUid:        ateomUID,
		SandboxAssets:         sandboxAssets,
		Spec:                  spec,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Pause: a local checkpoint, which leaves the snapshot on this node.
	if _, err := s.Checkpoint(ctx, &ateletpb.CheckpointRequest{
		Atespace:              atespace,
		ActorName:             actorName,
		ActorUid:              actorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		TargetAteomUid:        ateomUID,
		Spec:                  spec,
		Scope:                 ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
		Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_LOCAL,
		Config: &ateletpb.CheckpointRequest_LocalConfig{
			LocalConfig: &ateletpb.LocalCheckpointConfiguration{SnapshotName: snapshotName},
		},
	}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	snapshotFile := filepath.Join(ateletpath.LocalSnapshotDir(actorUID, snapshotName), "checkpoint.img")
	if _, err := os.Stat(snapshotFile); err != nil {
		t.Fatalf("pause did not write the local snapshot: %v", err)
	}

	// Resume: restores from that local snapshot.
	if _, err := s.Restore(ctx, &ateletpb.RestoreRequest{
		Atespace:              atespace,
		ActorName:             actorName,
		ActorUid:              actorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		TargetAteomUid:        ateomUID,
		SandboxAssets:         sandboxAssets,
		Spec:                  spec,
		Scope:                 ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
		Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_LOCAL,
		Config: &ateletpb.RestoreRequest_LocalConfig{
			LocalConfig: &ateletpb.LocalCheckpointConfiguration{SnapshotName: snapshotName},
		},
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if got := ateom.restored["checkpoint.img"]; got != "guest-memory" {
		t.Fatalf("restore staged %q for ateom, want the pause snapshot's %q", got, "guest-memory")
	}

	// Terminate: the actor is gone, and so should its snapshot be.
	if _, err := s.Terminate(ctx, &ateletpb.TerminateRequest{
		Atespace:              atespace,
		ActorName:             actorName,
		ActorUid:              actorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		TargetAteomUid:        ateomUID,
		Spec:                  spec,
	}); err != nil {
		t.Fatalf("Terminate: %v", err)
	}

	// Every RPC hands ateom the same directory set; the fake already relied
	// on checkpoint_dir and restore_dir above to place and find the snapshot.
	want := ateletpath.ActorDirs(actorUID)
	for _, rpc := range []string{"RunWorkload", "CheckpointWorkload", "RestoreWorkload", "TerminateWorkload"} {
		if got := ateom.actorDirs[rpc]; !proto.Equal(got, want) {
			t.Errorf("%s carried actor actorDirs %v, want %v", rpc, got, want)
		}
	}

	localDir := ateletpath.LocalCheckpointsDir(actorUID)
	if _, err := os.Stat(localDir); !os.IsNotExist(err) {
		leaked, _ := filepath.Glob(filepath.Join(localDir, "*", "*"))
		t.Errorf("local checkpoint dir survived terminate (stat err = %v), leaked files: %v", err, leaked)
	}

	// Terminate is the only chance to reclaim the actor's directory: nothing
	// else on the node deletes it.
	actorDir := ateletpath.ActorPath(actorUID)
	if entries, err := os.ReadDir(actorDir); err == nil {
		left := make([]string, 0, len(entries))
		for _, e := range entries {
			left = append(left, e.Name())
		}
		t.Errorf("actor dir %s survived terminate with %d entries: %v", actorDir, len(left), left)
	} else if !os.IsNotExist(err) {
		t.Errorf("reading actor dir %s: %v", actorDir, err)
	}
}

// TestRestoreUsesRequestSandboxAssets checks that Restore runs the actor with
// the sandbox assets on the request, not the ones recorded in the snapshot
// manifest.
func TestRestoreUsesRequestSandboxAssets(t *testing.T) {
	useTempNodeDirs(t)
	ctx := t.Context()

	const (
		atespace     = "ate-demo"
		actorName    = "counter"
		actorUID     = "actor-uid-1"
		ateomUID     = "ateom-uid-1"
		snapshotName = "pause-snap-1"
	)

	ateom := &fakeAteom{snapshotFiles: map[string]string{"checkpoint.img": "guest-memory"}}
	serveFakeAteom(t, ateom)

	host := imageVolumeTestRegistry(t)
	image := host + "/actor:v1"
	pushTestImage(t, image, singleFileLayer(t, "bin/app", "app"))
	checkpointPause := host + "/pause:v1"
	pushTestImage(t, checkpointPause, singleFileLayer(t, "pause", "pause-v1"))
	restorePause := host + "/pause:v2"
	pushTestImage(t, restorePause, singleFileLayer(t, "pause", "pause-v2"))

	runsc := []byte("runsc binary")
	s := &AteomHerder{
		ateomDialer:       newAteomDialer(1),
		imageCache:        newImageVolumeStore(t),
		anonGCSClient:     fakeObjectStorage{data: runsc},
		systemInfoVolumes: newSystemInfoVolumeRefresher(nil, nil),
	}
	assetsWithPause := func(pause string) *ateletpb.SandboxAssets {
		return &ateletpb.SandboxAssets{
			SandboxClass: "gvisor",
			PauseImage:   pause,
			Assets: map[string]*ateletpb.ArchAssets{
				runtime.GOARCH: {Files: map[string]*ateletpb.AssetFile{
					runscAssetName: {
						Url:    "gs://test-bucket/runsc",
						Sha256: fmt.Sprintf("%x", sha256.Sum256(runsc)),
					},
				}},
			},
		}
	}
	spec := &ateletpb.WorkloadSpec{
		Containers: []*ateletpb.Container{{Name: "app", Image: image, Command: []string{"/bin/app"}}},
	}

	if _, err := s.Run(ctx, &ateletpb.RunRequest{
		Atespace:              atespace,
		ActorName:             actorName,
		ActorUid:              actorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		TargetAteomUid:        ateomUID,
		SandboxAssets:         assetsWithPause(checkpointPause),
		Spec:                  spec,
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := s.Checkpoint(ctx, &ateletpb.CheckpointRequest{
		Atespace:              atespace,
		ActorName:             actorName,
		ActorUid:              actorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		TargetAteomUid:        ateomUID,
		Spec:                  spec,
		Scope:                 ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
		Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_LOCAL,
		Config: &ateletpb.CheckpointRequest_LocalConfig{
			LocalConfig: &ateletpb.LocalCheckpointConfiguration{SnapshotName: snapshotName},
		},
	}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(ateletpath.LocalSnapshotDir(actorUID, snapshotName), sandboxManifestName))
	if err != nil {
		t.Fatalf("reading snapshot manifest: %v", err)
	}
	manifestRec, err := unmarshalSandboxRecord(manifest)
	if err != nil {
		t.Fatalf("unmarshalling snapshot manifest: %v", err)
	}
	if manifestRec.PauseImage != checkpointPause {
		t.Fatalf("manifest pause image = %q, want %q", manifestRec.PauseImage, checkpointPause)
	}

	if _, err := s.Restore(ctx, &ateletpb.RestoreRequest{
		Atespace:              atespace,
		ActorName:             actorName,
		ActorUid:              actorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		TargetAteomUid:        ateomUID,
		SandboxAssets:         assetsWithPause(restorePause),
		Spec:                  spec,
		Scope:                 ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
		Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_LOCAL,
		Config: &ateletpb.RestoreRequest_LocalConfig{
			LocalConfig: &ateletpb.LocalCheckpointConfiguration{SnapshotName: snapshotName},
		},
	}); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	got, err := readSandboxRecord(actorUID)
	if err != nil {
		t.Fatalf("reading on-node sandbox record: %v", err)
	}
	if got.PauseImage != restorePause {
		t.Errorf("restored actor pause image = %q, want the request's %q", got.PauseImage, restorePause)
	}
}

// externalRestoreFixture is what an EXTERNAL Restore needs on the node side:
// a registry serving the app and pause images, a fake ateom, and a request
// whose sandbox assets name the runsc bytes that assetStore serves.
type externalRestoreFixture struct {
	herder *AteomHerder
	req    *ateletpb.RestoreRequest
}

func newExternalRestoreFixture(t *testing.T, snapshots *recordingObjectStorage, assetStore objectstorage.ObjectStorage) externalRestoreFixture {
	t.Helper()
	useTempNodeDirs(t)
	serveFakeAteom(t, &fakeAteom{snapshotFiles: map[string]string{"checkpoint.img": "guest-memory"}})

	host := imageVolumeTestRegistry(t)
	image := host + "/actor:v1"
	pushTestImage(t, image, singleFileLayer(t, "bin/app", "app"))
	pause := host + "/pause:v1"
	pushTestImage(t, pause, singleFileLayer(t, "pause", "pause-v1"))

	runsc := []byte("runsc binary")
	return externalRestoreFixture{
		herder: &AteomHerder{
			ateomDialer:       newAteomDialer(1),
			imageCache:        newImageVolumeStore(t),
			gcsClient:         snapshots,
			anonGCSClient:     assetStore,
			systemInfoVolumes: newSystemInfoVolumeRefresher(nil, nil),
		},
		req: &ateletpb.RestoreRequest{
			Atespace:              "ate-demo",
			ActorName:             "counter-1",
			ActorUid:              snapshotOwnerUID,
			ActorTemplateAtespace: "default",
			ActorTemplateName:     "counter",
			TargetAteomUid:        "ateom-uid-1",
			SandboxAssets: &ateletpb.SandboxAssets{
				SandboxClass: "gvisor",
				PauseImage:   pause,
				Assets: map[string]*ateletpb.ArchAssets{
					runtime.GOARCH: {Files: map[string]*ateletpb.AssetFile{
						runscAssetName: {
							Url:    "gs://test-bucket/runsc",
							Sha256: fmt.Sprintf("%x", sha256.Sum256(runsc)),
						},
					}},
				},
			},
			Spec: &ateletpb.WorkloadSpec{
				Containers: []*ateletpb.Container{{Name: "app", Image: image, Command: []string{"/bin/app"}}},
			},
			Scope: ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
			Type:  ateletpb.CheckpointType_CHECKPOINT_TYPE_EXTERNAL,
			Config: &ateletpb.RestoreRequest_ExternalConfig{
				ExternalConfig: &ateletpb.ExternalRestoreConfiguration{SnapshotUri: testSnapshotURI},
			},
		},
	}
}

// uploadTestSnapshot writes a one-file snapshot and its manifest under
// testSnapshotURI, the way a suspend would.
func uploadTestSnapshot(t *testing.T, store *recordingObjectStorage, pauseImage string) {
	t.Helper()
	ctx := context.Background()
	payload := filepath.Join(t.TempDir(), "checkpoint.img")
	if err := os.WriteFile(payload, []byte("guest-memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := objectstorage.SendLocalFileToGCSWithZstd(ctx, store, testSnapshotURI+"/checkpoint.img.zstd", payload); err != nil {
		t.Fatalf("uploading checkpoint: %v", err)
	}
	manifest, err := json.Marshal(&sandboxAssetsRecord{
		SandboxClass:  "gvisor",
		PauseImage:    pauseImage,
		Atespace:      "ate-demo",
		ActorName:     "counter-1",
		SnapshotFiles: []string{"checkpoint.img"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := objectstorage.SendBytesToGCS(ctx, store, testSnapshotURI+"/"+sandboxManifestName, manifest); err != nil {
		t.Fatalf("uploading manifest: %v", err)
	}
}

// The manifest fetch and the sandbox prep run concurrently. When the manifest
// is missing, the restore must report that, not the context cancellation the
// prep leg sees as collateral, and must leave no system-info registration
// behind.
func TestRestoreReportsManifestErrorOverCollateral(t *testing.T) {
	f := newExternalRestoreFixture(t, &recordingObjectStorage{}, fakeObjectStorage{data: []byte("runsc binary")})

	_, err := f.herder.Restore(t.Context(), f.req)
	if err == nil {
		t.Fatal("Restore succeeded without a snapshot manifest")
	}
	if !strings.Contains(err.Error(), "while fetching snapshot manifest") || !errors.Is(err, objectstorage.ErrObjectNotFound) {
		t.Errorf("Restore error = %v, want the manifest fetch failure", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("Restore reported the collateral cancellation instead of the manifest error: %v", err)
	}
	f.herder.systemInfoVolumes.mu.Lock()
	_, registered := f.herder.systemInfoVolumes.actors[f.req.GetActorUid()]
	f.herder.systemInfoVolumes.mu.Unlock()
	if registered {
		t.Error("failed Restore left the actor's system-info volumes registered")
	}
}

// With a good manifest, a failure in the prep leg still surfaces as the
// restore's error.
func TestRestoreSurfacesPrepErrorAfterManifest(t *testing.T) {
	snapshots := &recordingObjectStorage{}
	f := newExternalRestoreFixture(t, snapshots, fakeObjectStorage{err: errors.New("asset store down")})
	uploadTestSnapshot(t, snapshots, f.req.GetSandboxAssets().GetPauseImage())

	_, err := f.herder.Restore(t.Context(), f.req)
	if err == nil {
		t.Fatal("Restore succeeded although the sandbox asset fetch failed")
	}
	if !strings.Contains(err.Error(), "asset store down") {
		t.Errorf("Restore error = %v, want the sandbox asset failure", err)
	}
}
