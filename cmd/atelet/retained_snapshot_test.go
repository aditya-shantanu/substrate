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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/agent-substrate/substrate/cmd/atelet/internal/ateletpath"
	"github.com/agent-substrate/substrate/internal/ateattr"
	"github.com/agent-substrate/substrate/internal/proto/ateletpb"
)

// suspendedActorFixture is an actor that was run and then suspended (an
// EXTERNAL checkpoint under testSnapshotURI) on this node, with the request
// that resumes it.
type suspendedActorFixture struct {
	herder    *AteomHerder
	snapshots *recordingObjectStorage
	ateom     *fakeAteom
	restore   *ateletpb.RestoreRequest
	terminate *ateletpb.TerminateRequest
	logs      *bytes.Buffer
}

func newSuspendedActorFixture(t *testing.T, retain bool) suspendedActorFixture {
	t.Helper()
	useTempNodeDirs(t)
	ctx := t.Context()

	const (
		atespace  = "ate-demo"
		actorName = "counter-1"
		actorUID  = snapshotOwnerUID
		ateomUID  = "ateom-uid-1"
	)

	ateom := &fakeAteom{snapshotFiles: map[string]string{"checkpoint.img": "guest-memory", "pages.img": "guest-pages"}}
	serveFakeAteom(t, ateom)

	host := imageVolumeTestRegistry(t)
	image := host + "/actor:v1"
	pushTestImage(t, image, singleFileLayer(t, "bin/app", "app"))
	pause := host + "/pause:v1"
	pushTestImage(t, pause, singleFileLayer(t, "pause", "pause-v1"))

	// The restore source is asserted from the timing record, the way an
	// operator would read the hit rate.
	logs := &bytes.Buffer{}
	orig := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(orig) })

	runsc := []byte("runsc binary")
	snapshots := &recordingObjectStorage{}
	s := &AteomHerder{
		ateomDialer:             newAteomDialer(1),
		imageCache:              newImageVolumeStore(t),
		gcsClient:               snapshots,
		anonGCSClient:           fakeObjectStorage{data: runsc},
		systemInfoVolumes:       newSystemInfoVolumeRefresher(nil, nil),
		retainUploadedSnapshots: retain,
	}
	sandboxAssets := &ateletpb.SandboxAssets{
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
	if _, err := s.Checkpoint(ctx, &ateletpb.CheckpointRequest{
		Atespace:              atespace,
		ActorName:             actorName,
		ActorUid:              actorUID,
		ActorTemplateAtespace: "default",
		ActorTemplateName:     "counter",
		TargetAteomUid:        ateomUID,
		Spec:                  spec,
		Scope:                 ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
		Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_EXTERNAL,
		Config: &ateletpb.CheckpointRequest_ExternalConfig{
			ExternalConfig: &ateletpb.ExternalCheckpointConfiguration{SnapshotUri: testSnapshotURI},
		},
	}); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if n := snapshots.getCount(); n != 0 {
		t.Fatalf("suspend performed %d GETs, want none", n)
	}

	return suspendedActorFixture{
		herder:    s,
		snapshots: snapshots,
		ateom:     ateom,
		logs:      logs,
		restore: &ateletpb.RestoreRequest{
			Atespace:              atespace,
			ActorName:             actorName,
			ActorUid:              actorUID,
			ActorTemplateAtespace: "default",
			ActorTemplateName:     "counter",
			TargetAteomUid:        ateomUID,
			SandboxAssets:         sandboxAssets,
			Spec:                  spec,
			Scope:                 ateletpb.SnapshotScope_SNAPSHOT_SCOPE_FULL,
			Type:                  ateletpb.CheckpointType_CHECKPOINT_TYPE_EXTERNAL,
			Config: &ateletpb.RestoreRequest_ExternalConfig{
				ExternalConfig: &ateletpb.ExternalRestoreConfiguration{SnapshotUri: testSnapshotURI},
			},
		},
		terminate: &ateletpb.TerminateRequest{
			Atespace:              atespace,
			ActorName:             actorName,
			ActorUid:              actorUID,
			ActorTemplateAtespace: "default",
			ActorTemplateName:     "counter",
			TargetAteomUid:        ateomUID,
			Spec:                  spec,
		},
	}
}

// restoreSourceLogged returns the restore-source attribute of the last
// "Restore timing breakdown" record.
func (f suspendedActorFixture) restoreSourceLogged(t *testing.T) string {
	t.Helper()
	var source string
	for _, line := range strings.Split(f.logs.String(), "\n") {
		if !strings.Contains(line, "Restore timing breakdown") {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("parsing log line %q: %v", line, err)
		}
		source, _ = rec[restoreSourceLogKey].(string)
	}
	return source
}

func inodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Sys().(*syscall.Stat_t).Ino
}

// checkRetainedSnapshot asserts the actor's retained copy is complete: files
// with their checkpoint contents, the manifest listing them, and the URI.
func checkRetainedSnapshot(t *testing.T, actorUID, uri string, files map[string]string) {
	t.Helper()
	dir := ateletpath.RetainedSnapshotDir(actorUID)
	for name, want := range files {
		if got, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(got) != want {
			t.Errorf("retained %s = %q, %v; want %q", name, got, err, want)
		}
	}
	if got, err := os.ReadFile(filepath.Join(dir, retainedURIFileName)); err != nil || string(got) != uri {
		t.Errorf("retained uri = %q, %v; want %q", got, err, uri)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, sandboxManifestName))
	if err != nil {
		t.Fatalf("reading retained manifest: %v", err)
	}
	rec, err := unmarshalSandboxRecord(manifest)
	if err != nil {
		t.Fatalf("parsing retained manifest: %v", err)
	}
	if len(rec.SnapshotFiles) != len(files) {
		t.Errorf("retained manifest lists %v, want the %d checkpoint files", rec.SnapshotFiles, len(files))
	}
	for _, name := range rec.SnapshotFiles {
		if _, ok := files[name]; !ok {
			t.Errorf("retained manifest lists %q, which was not checkpointed", name)
		}
	}
}

// A suspend leaves the uploaded snapshot on the node, outside the directories
// the next activation wipes, and the resume there stages it without a single
// GET, by linking: the retained copy stays for a later resume.
func TestRestoreUsesRetainedSnapshot(t *testing.T) {
	f := newSuspendedActorFixture(t, true)
	actorUID := f.restore.GetActorUid()
	checkRetainedSnapshot(t, actorUID, testSnapshotURI, f.ateom.snapshotFiles)

	// The files were moved, not copied: nothing is left for resetActorDirs to
	// delete, and what it does delete leaves the retained copy alone.
	if entries, err := os.ReadDir(ateletpath.CheckpointStateDir(actorUID)); err != nil || len(entries) != 0 {
		t.Errorf("checkpoint-state after suspend has %d entries (err %v), want none", len(entries), err)
	}
	if err := resetActorDirs(actorUID); err != nil {
		t.Fatal(err)
	}
	checkRetainedSnapshot(t, actorUID, testSnapshotURI, f.ateom.snapshotFiles)

	if _, err := f.herder.Restore(t.Context(), f.restore); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if n := f.snapshots.getCount(); n != 0 {
		t.Errorf("resume on the uploading node performed %d GETs, want none", n)
	}
	for name, want := range f.ateom.snapshotFiles {
		if got := f.ateom.restored[name]; got != want {
			t.Errorf("restore staged %q for %s, want %q", got, name, want)
		}
		staged := filepath.Join(ateletpath.RestoreStateDir(actorUID), name)
		retained := filepath.Join(ateletpath.RetainedSnapshotDir(actorUID), name)
		if inodeOf(t, staged) != inodeOf(t, retained) {
			t.Errorf("%s was copied into restore-state, want a link to the retained file", name)
		}
	}
	if got := f.restoreSourceLogged(t); got != restoreSourceRetained {
		t.Errorf("logged %s = %q, want %q", restoreSourceLogKey, got, restoreSourceRetained)
	}
	checkRetainedSnapshot(t, actorUID, testSnapshotURI, f.ateom.snapshotFiles)

	if _, err := f.herder.Terminate(t.Context(), f.terminate); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if _, err := os.Stat(ateletpath.ActorPath(actorUID)); !os.IsNotExist(err) {
		leaked, _ := filepath.Glob(filepath.Join(ateletpath.ActorPath(actorUID), "*"))
		t.Errorf("actor dir survived terminate (stat err = %v), leaked: %v", err, leaked)
	}
}

func TestRestoreMissesRetainedSnapshot(t *testing.T) {
	tests := []struct {
		name string
		// spoil makes the retained copy unusable for the restore and returns
		// the URI to restore.
		spoil func(t *testing.T, f suspendedActorFixture) string
	}{
		{
			name: "a different URI",
			spoil: func(t *testing.T, f suspendedActorFixture) string {
				// The same snapshot under another name, as a later suspend of the
				// same actor would be.
				const other = testSnapshotURI + "-2"
				f.snapshots.mu.Lock()
				for key, body := range f.snapshots.objects {
					if rel, ok := strings.CutPrefix(key, testSnapshotPath+"/"); ok {
						f.snapshots.objects[testSnapshotPath+"-2/"+rel] = body
					}
				}
				f.snapshots.mu.Unlock()
				return other
			},
		},
		{
			name: "a retained file missing",
			spoil: func(t *testing.T, f suspendedActorFixture) string {
				if err := os.Remove(filepath.Join(ateletpath.RetainedSnapshotDir(f.restore.GetActorUid()), "pages.img")); err != nil {
					t.Fatal(err)
				}
				return testSnapshotURI
			},
		},
		{
			name: "nothing retained",
			spoil: func(t *testing.T, f suspendedActorFixture) string {
				if err := os.RemoveAll(ateletpath.RetainedSnapshotDir(f.restore.GetActorUid())); err != nil {
					t.Fatal(err)
				}
				return testSnapshotURI
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSuspendedActorFixture(t, true)
			uri := tc.spoil(t, f)
			f.restore.Config = &ateletpb.RestoreRequest_ExternalConfig{
				ExternalConfig: &ateletpb.ExternalRestoreConfiguration{SnapshotUri: uri},
			}

			if _, err := f.herder.Restore(t.Context(), f.restore); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			// The manifest and every file.
			if want := 1 + len(f.ateom.snapshotFiles); f.snapshots.getCount() != want {
				t.Errorf("miss performed %d GETs, want %d", f.snapshots.getCount(), want)
			}
			for name, want := range f.ateom.snapshotFiles {
				if got := f.ateom.restored[name]; got != want {
					t.Errorf("restore staged %q for %s, want %q", got, name, want)
				}
			}
			if got := f.restoreSourceLogged(t); got != restoreSourceDownload {
				t.Errorf("logged %s = %q, want %q", restoreSourceLogKey, got, restoreSourceDownload)
			}
		})
	}
}

// With retention off, a suspend keeps nothing; a copy that is already there is
// still used, so turning the flag off does not strand disk or force downloads.
func TestRetainUploadedSnapshotsFlag(t *testing.T) {
	t.Run("off keeps nothing and downloads", func(t *testing.T) {
		f := newSuspendedActorFixture(t, false)
		if _, err := os.Stat(ateletpath.RetainedSnapshotDir(f.restore.GetActorUid())); !os.IsNotExist(err) {
			t.Errorf("retained snapshot dir exists with retention off (stat err = %v)", err)
		}
		if _, err := f.herder.Restore(t.Context(), f.restore); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if f.snapshots.getCount() == 0 {
			t.Error("Restore performed no GETs although nothing was retained")
		}
	})
	t.Run("off still restores from an existing copy", func(t *testing.T) {
		f := newSuspendedActorFixture(t, true)
		f.herder.retainUploadedSnapshots = false
		if _, err := f.herder.Restore(t.Context(), f.restore); err != nil {
			t.Fatalf("Restore: %v", err)
		}
		if n := f.snapshots.getCount(); n != 0 {
			t.Errorf("Restore performed %d GETs with a retained copy present, want none", n)
		}
	})
}

// Suspending a paused actor uploads its pause snapshot; that copy is retained
// too, by linking, since the pause snapshot is pruned separately.
func TestUploadPausedCheckpointRetainsSnapshot(t *testing.T) {
	useTempNodeDirs(t)
	req := validUploadPausedCheckpointRequest()
	files := map[string]string{"checkpoint.img": "guest-memory", "pages.img": "guest-pages"}
	writeLocalSnapshot(t, ateletpath.LocalSnapshotDir(req.GetActorUid(), req.GetLocalSnapshotName()), sandboxAssetsRecord{
		SandboxClass:  "gvisor",
		PauseImage:    testPauseImage,
		Scope:         ateattr.SnapshotScopeFull,
		SnapshotFiles: []string{"checkpoint.img", "pages.img"},
	}, files)

	store := &recordingObjectStorage{}
	s := &AteomHerder{gcsClient: store, retainUploadedSnapshots: true}
	if _, err := s.UploadPausedCheckpoint(t.Context(), req); err != nil {
		t.Fatalf("UploadPausedCheckpoint: %v", err)
	}
	checkRetainedSnapshot(t, req.GetActorUid(), req.GetDestinationSnapshotUri(), files)
	if _, err := os.Stat(ateletpath.LocalCheckpointsDir(req.GetActorUid())); !os.IsNotExist(err) {
		t.Errorf("local checkpoints survived the upload (stat err = %v)", err)
	}
}

// writeRetainedFixture lays out an actor dir with a source directory holding
// files, for driving retainUploadedSnapshot directly.
func writeRetainedFixture(t *testing.T, actorUID, srcName string, files map[string]string) string {
	t.Helper()
	src := filepath.Join(ateletpath.ActorPath(actorUID), srcName)
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return src
}

func retainedRec(files map[string]string) *sandboxAssetsRecord {
	rec := &sandboxAssetsRecord{SandboxClass: "gvisor", PauseImage: testPauseImage}
	for name := range files {
		rec.SnapshotFiles = append(rec.SnapshotFiles, name)
	}
	return rec
}

func TestRetainUploadedSnapshot(t *testing.T) {
	const actorUID = "actor-uid-1"
	ctx := context.Background()

	t.Run("replaces the previous copy and cleans up after itself", func(t *testing.T) {
		useTempNodeDirs(t)
		first := map[string]string{"checkpoint.img": "v1"}
		src := writeRetainedFixture(t, actorUID, "checkpoint-state", first)
		if err := retainUploadedSnapshot(ctx, actorUID, src, retainedRec(first), testSnapshotURI, true); err != nil {
			t.Fatalf("first retain: %v", err)
		}
		checkRetainedSnapshot(t, actorUID, testSnapshotURI, first)
		if _, err := os.Stat(filepath.Join(src, "checkpoint.img")); !os.IsNotExist(err) {
			t.Errorf("moved source still present (stat err = %v)", err)
		}

		second := map[string]string{"checkpoint.img": "v2", "pages.img": "p2"}
		src = writeRetainedFixture(t, actorUID, "checkpoint-state", second)
		if err := retainUploadedSnapshot(ctx, actorUID, src, retainedRec(second), testSnapshotURI+"-2", true); err != nil {
			t.Fatalf("second retain: %v", err)
		}
		checkRetainedSnapshot(t, actorUID, testSnapshotURI+"-2", second)

		// The superseded copy goes in the background; the staging dir never
		// outlives the call.
		actorDir := ateletpath.ActorPath(actorUID)
		if _, err := os.Stat(filepath.Join(actorDir, "retained-snapshot.tmp")); !os.IsNotExist(err) {
			t.Errorf("staging dir left behind (stat err = %v)", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(actorDir, "retained-snapshot.old")); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("superseded retained snapshot was not removed")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})

	t.Run("links when not moving", func(t *testing.T) {
		useTempNodeDirs(t)
		files := map[string]string{"checkpoint.img": "v1"}
		src := writeRetainedFixture(t, actorUID, filepath.Join("local-checkpoint", "pause-1"), files)
		if err := retainUploadedSnapshot(ctx, actorUID, src, retainedRec(files), testSnapshotURI, false); err != nil {
			t.Fatalf("retain: %v", err)
		}
		checkRetainedSnapshot(t, actorUID, testSnapshotURI, files)
		if inodeOf(t, filepath.Join(src, "checkpoint.img")) != inodeOf(t, filepath.Join(ateletpath.RetainedSnapshotDir(actorUID), "checkpoint.img")) {
			t.Error("retained file is a copy, want a link to the pause snapshot")
		}
	})

	t.Run("a failure leaves the previous copy in place", func(t *testing.T) {
		useTempNodeDirs(t)
		first := map[string]string{"checkpoint.img": "v1"}
		src := writeRetainedFixture(t, actorUID, "checkpoint-state", first)
		if err := retainUploadedSnapshot(ctx, actorUID, src, retainedRec(first), testSnapshotURI, true); err != nil {
			t.Fatalf("first retain: %v", err)
		}
		// The manifest names a file the source does not have.
		src = writeRetainedFixture(t, actorUID, "checkpoint-state", map[string]string{"checkpoint.img": "v2"})
		rec := retainedRec(map[string]string{"checkpoint.img": "", "pages.img": ""})
		if err := retainUploadedSnapshot(ctx, actorUID, src, rec, testSnapshotURI+"-2", true); err == nil {
			t.Fatal("retain succeeded with a source file missing")
		}
		checkRetainedSnapshot(t, actorUID, testSnapshotURI, first)
		if _, err := os.Stat(filepath.Join(ateletpath.ActorPath(actorUID), "retained-snapshot.tmp")); !os.IsNotExist(err) {
			t.Errorf("staging dir left behind after failure (stat err = %v)", err)
		}
	})
}

func TestRetainedSnapshotFor(t *testing.T) {
	const actorUID = "actor-uid-1"
	files := map[string]string{"checkpoint.img": "v1", "pages.img": "p1"}
	tests := []struct {
		name    string
		prepare func(t *testing.T, dir string)
		uri     string
		wantHit bool
	}{
		{name: "complete and same URI", uri: testSnapshotURI, wantHit: true},
		{name: "other URI", uri: testSnapshotURI + "-2"},
		{
			name: "file missing",
			prepare: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "pages.img")); err != nil {
					t.Fatal(err)
				}
			},
			uri: testSnapshotURI,
		},
		{
			name: "file is a symlink",
			prepare: func(t *testing.T, dir string) {
				if err := os.Remove(filepath.Join(dir, "pages.img")); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/etc/hostname", filepath.Join(dir, "pages.img")); err != nil {
					t.Fatal(err)
				}
			},
			uri: testSnapshotURI,
		},
		{
			name: "manifest unreadable",
			prepare: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, sandboxManifestName), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			uri: testSnapshotURI,
		},
		{
			name: "no retained dir",
			prepare: func(t *testing.T, dir string) {
				if err := os.RemoveAll(dir); err != nil {
					t.Fatal(err)
				}
			},
			uri: testSnapshotURI,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			useTempNodeDirs(t)
			src := writeRetainedFixture(t, actorUID, "checkpoint-state", files)
			if err := retainUploadedSnapshot(context.Background(), actorUID, src, retainedRec(files), testSnapshotURI, true); err != nil {
				t.Fatal(err)
			}
			if tc.prepare != nil {
				tc.prepare(t, ateletpath.RetainedSnapshotDir(actorUID))
			}
			rec, err := retainedSnapshotFor(actorUID, tc.uri)
			if (err == nil) != tc.wantHit {
				t.Fatalf("retainedSnapshotFor() err = %v, want hit %v", err, tc.wantHit)
			}
			if tc.wantHit && len(rec.SnapshotFiles) != len(files) {
				t.Errorf("hit returned manifest files %v, want %d files", rec.SnapshotFiles, len(files))
			}
		})
	}
}
