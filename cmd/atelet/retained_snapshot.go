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
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"

	"github.com/agent-substrate/substrate/cmd/atelet/internal/ateletpath"
)

// retainedURIFileName is the file in RetainedSnapshotDir holding the exact
// snapshot URI string the retained files were uploaded under.
const retainedURIFileName = "uri"

// Values of the restore-source log attribute (restoreSourceLogKey): where
// Restore staged the checkpoint files from.
const (
	restoreSourceRetained = "retained"
	restoreSourceDownload = "download"
	restoreSourceLocal    = "local"
)

// retainUploadedSnapshot keeps a node-local copy of the snapshot just uploaded
// to uri, so a later Restore of that URI on this node stages it from disk
// instead of downloading it. rec is the uploaded manifest; its SnapshotFiles
// are taken from srcDir, by rename when move is set (the source is about to be
// wiped anyway) and by hard link otherwise. Both srcDir and the retained
// directory are under the actor directory, which confines every access.
//
// The copy is assembled in a staging directory and swapped into place, so a
// reader finds either the previous complete copy or the new one. The previous
// copy is removed in the background: that RemoveAll is the step that runs for
// seconds under disk contention, and nothing waits on it.
func retainUploadedSnapshot(ctx context.Context, actorUID, srcDir string, rec *sandboxAssetsRecord, uri string, move bool) (err error) {
	actorDir := ateletpath.ActorPath(actorUID)
	root, err := os.OpenRoot(actorDir)
	if err != nil {
		return fmt.Errorf("while opening actor directory: %w", err)
	}
	defer root.Close()
	src, err := filepath.Rel(actorDir, srcDir)
	if err != nil {
		return err
	}
	final, err := filepath.Rel(actorDir, ateletpath.RetainedSnapshotDir(actorUID))
	if err != nil {
		return err
	}
	staging, old := final+".tmp", final+".old"

	// Leftovers of an interrupted build.
	if err := root.RemoveAll(staging); err != nil {
		return fmt.Errorf("while clearing retained snapshot staging dir: %w", err)
	}
	if err := root.Mkdir(staging, 0o700); err != nil {
		return fmt.Errorf("while creating retained snapshot staging dir: %w", err)
	}
	defer func() {
		if err != nil {
			_ = root.RemoveAll(staging)
		}
	}()

	for _, name := range rec.SnapshotFiles {
		from, to := filepath.Join(src, name), filepath.Join(staging, name)
		if !move {
			if err := stageFile(ctx, root, from, to); err != nil {
				return err
			}
			continue
		}
		info, err := root.Lstat(from)
		if err != nil {
			return fmt.Errorf("while inspecting %s: %w", from, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", from)
		}
		if err := root.Rename(from, to); err != nil {
			return fmt.Errorf("while moving %s to %s: %w", from, to, err)
		}
	}
	manifest, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("while marshaling snapshot manifest: %w", err)
	}
	if err := root.WriteFile(filepath.Join(staging, sandboxManifestName), manifest, 0o600); err != nil {
		return fmt.Errorf("while writing retained snapshot manifest: %w", err)
	}
	if err := root.WriteFile(filepath.Join(staging, retainedURIFileName), []byte(uri), 0o600); err != nil {
		return fmt.Errorf("while writing retained snapshot uri: %w", err)
	}

	// Swap. The old copy may still be there if its background removal did not
	// finish (or atelet restarted); either way it has to leave the name.
	if err := root.RemoveAll(old); err != nil {
		return fmt.Errorf("while clearing previous retained snapshot: %w", err)
	}
	if err := root.Rename(final, old); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("while moving previous retained snapshot aside: %w", err)
	}
	if err := root.Rename(staging, final); err != nil {
		return fmt.Errorf("while installing retained snapshot: %w", err)
	}
	oldDir := filepath.Join(actorDir, old)
	go func() {
		if err := os.RemoveAll(oldDir); err != nil {
			slog.WarnContext(ctx, "failed to remove superseded retained snapshot", slog.String("path", oldDir), slog.Any("err", err))
		}
	}()
	return nil
}

// retainSnapshot is retainUploadedSnapshot as Checkpoint and
// UploadPausedCheckpoint call it: best-effort, because the upload already
// succeeded and the next Restore downloads if the copy is missing.
func (s *AteomHerder) retainSnapshot(ctx context.Context, actorUID, srcDir string, rec *sandboxAssetsRecord, uri string, move bool) {
	if !s.retainUploadedSnapshots {
		return
	}
	t := time.Now()
	if err := retainUploadedSnapshot(ctx, actorUID, srcDir, rec, uri, move); err != nil {
		slog.WarnContext(ctx, "failed to retain uploaded snapshot on node; the next resume here will download it",
			slog.String("actorUID", actorUID), slog.String("snapshot_uri", uri), slog.Any("err", err))
		return
	}
	slog.InfoContext(ctx, "retained uploaded snapshot on node",
		slog.String("actorUID", actorUID), slog.String("snapshot_uri", uri),
		slog.Int("files", len(rec.SnapshotFiles)), slog.Duration("took", time.Since(t)))
}

// lookupRetainedSnapshot returns the manifest of the actor's retained snapshot
// when it is the snapshot at uri and every file the manifest lists is present.
// Anything short of that is a miss, and the caller downloads as if nothing
// were retained.
func lookupRetainedSnapshot(ctx context.Context, actorUID, uri string) (*sandboxAssetsRecord, bool) {
	rec, err := retainedSnapshotFor(actorUID, uri)
	if err != nil {
		slog.InfoContext(ctx, "no retained snapshot for this URI; downloading",
			slog.String("actorUID", actorUID), slog.String("snapshot_uri", uri), slog.Any("reason", err))
		return nil, false
	}
	return rec, true
}

func retainedSnapshotFor(actorUID, uri string) (*sandboxAssetsRecord, error) {
	root, err := os.OpenRoot(ateletpath.RetainedSnapshotDir(actorUID))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	got, err := root.ReadFile(retainedURIFileName)
	if err != nil {
		return nil, fmt.Errorf("while reading retained snapshot uri: %w", err)
	}
	if string(got) != uri {
		return nil, fmt.Errorf("retained snapshot is %q", got)
	}
	manifest, err := root.ReadFile(sandboxManifestName)
	if err != nil {
		return nil, fmt.Errorf("while reading retained snapshot manifest: %w", err)
	}
	rec, err := unmarshalSandboxRecord(manifest)
	if err != nil {
		return nil, err
	}
	for _, name := range rec.SnapshotFiles {
		info, err := root.Lstat(name)
		if err != nil {
			return nil, fmt.Errorf("retained snapshot is incomplete: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("retained snapshot file %s is not a regular file", name)
		}
	}
	return rec, nil
}

// stageSnapshotFiles links files from srcDir into dstDir. Both must be inside
// actorDir, which confines every access.
func stageSnapshotFiles(ctx context.Context, actorDir, srcDir, dstDir string, files []string) error {
	root, err := os.OpenRoot(actorDir)
	if err != nil {
		return fmt.Errorf("while opening actor directory: %w", err)
	}
	defer root.Close()
	srcDir, err = filepath.Rel(actorDir, srcDir)
	if err != nil {
		return err
	}
	dstDir, err = filepath.Rel(actorDir, dstDir)
	if err != nil {
		return err
	}
	for _, fileName := range files {
		if ctx.Err() != nil {
			return fmt.Errorf("context cancelled: %w", ctx.Err())
		}
		if err := stageFile(ctx, root, filepath.Join(srcDir, fileName), filepath.Join(dstDir, fileName)); err != nil {
			return err
		}
	}
	return nil
}

// stageFile links src to dst within root, copying only across filesystems.
func stageFile(ctx context.Context, root *os.Root, src, dst string) error {
	// A link to a symlink would be followed later, outside the root.
	info, err := root.Lstat(src)
	if err != nil {
		return fmt.Errorf("while inspecting %s: %w", src, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	// Link rather than copy. The source lives under the same actor dir as the
	// destination, so this stages the memory image in constant time instead of
	// re-writing its whole working set. Nothing rewrites the shared inode: CH
	// demand-pages from the staged image read-only, rewriteSnapshotSocketPaths
	// renames its rewritten config.json into place rather than truncating, and
	// MergeDeltaIntoBase refuses its in-place overlay once the image carries a
	// second link.
	//
	// EXDEV alone falls back to copying, so an unexpected link failure surfaces
	// instead of silently reverting to the full copy this exists to remove. It
	// also keeps copyRootFile off a dst that is already a link to src, where its
	// O_TRUNC would empty both and report a successful copy of the old size.
	switch err := linkFile(root, src, dst); {
	case err == nil:
		return nil
	case !errors.Is(err, unix.EXDEV):
		return fmt.Errorf("failed to link %s to %s: %w", src, dst, err)
	}
	slog.WarnContext(ctx, "snapshot source and destination are on different filesystems; copying instead of linking",
		slog.String("src", src), slog.String("dst", dst))
	if _, err := copyRootFile(root, src, dst); err != nil {
		return fmt.Errorf("failed to copy %s to %s: %w", src, dst, err)
	}
	return nil
}
