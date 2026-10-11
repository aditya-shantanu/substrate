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

package imagecache

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

// Lazy image loading with Nydus (RAFS).
//
// A Nydus-converted image (nydusify convert, "zran" or full RAFS) carries one
// small bootstrap layer — the whole filesystem's metadata — annotated
// containerd.io/snapshot/nydus-bootstrap, plus data blobs that nydusd fetches
// chunk by chunk on first access. For such an image the store does not
// unpack anything: it extracts the bootstrap into the cache and records
// where nydusd must fetch the data from. The privileged consumer
// (setupNydusRootfs) mounts the image through nydusd and overlays the
// bundle's upper on top, exactly where it would otherwise overlay the
// unpacked layers. This is the Substrate counterpart of GKE Image
// Streaming: a never-seen multi-GB image starts in seconds instead of after
// a full pull and unpack.
//
// The feature is off unless the store is given a backend URL
// (WithNydusBackend). Artifact Registry answers blob GETs with a redirect
// that nydusd's registry backend does not follow, and registry tokens
// expire under long-lived mounts, so the backend is normally a node-local
// registry proxy that injects credentials and follows redirects.

const (
	nydusBootstrapAnnotation = "containerd.io/snapshot/nydus-bootstrap"
	nydusDirName             = "nydus"
	nydusCacheDirName        = "nydus-cache"
	nydusBootstrapFileName   = "bootstrap"
)

// NydusSpec tells the consumer how to mount an image lazily.
type NydusSpec struct {
	// Bootstrap is the absolute path of the RAFS bootstrap (metadata) file.
	Bootstrap string `json:"bootstrap"`
	// Backend is the base URL of the registry (proxy) serving the data blobs,
	// e.g. "http://10.0.0.5:5000".
	Backend string `json:"backend"`
	// Repo is the repository path of the image within that registry.
	Repo string `json:"repo"`
	// CacheDir is the node-local chunk cache shared by every actor of every
	// image, so a second start of the same image reads locally.
	CacheDir string `json:"cacheDir"`
}

// nydusRecord is the persisted form of a lazily-loaded image.
type nydusRecord struct {
	Bootstrap string `json:"bootstrap"`
	Repo      string `json:"repo"`
}

// WithNydusBackend enables lazy loading of Nydus-converted images, with
// nydusd fetching blobs from the given registry base URL.
func WithNydusBackend(url string) Option {
	return func(s *Store) { s.nydusBackend = url }
}

func (s *Store) nydusDir(digest v1.Hash) string {
	return filepath.Join(s.root, nydusDirName, digest.Algorithm, digest.Hex)
}

func (s *Store) nydusCacheDir() string { return filepath.Join(s.root, nydusCacheDirName) }

func (s *Store) nydusSpec(rec *nydusRecord) *NydusSpec {
	return &NydusSpec{Bootstrap: rec.Bootstrap, Backend: s.nydusBackend, Repo: rec.Repo, CacheDir: s.nydusCacheDir()}
}

// nydusBootstrapLayer returns the index of the manifest layer carrying the
// RAFS bootstrap, or -1 when the image is a plain OCI image.
func nydusBootstrapLayer(m *v1.Manifest) int {
	if m == nil {
		return -1
	}
	for i, l := range m.Layers {
		if l.Annotations[nydusBootstrapAnnotation] == "true" {
			return i
		}
	}
	return -1
}

// pullNydus records a Nydus image: only its bootstrap layer is fetched.
func (s *Store) pullNydus(ctx context.Context, repo string, digest v1.Hash, img v1.Image, bootstrapIdx int) (*Image, error) {
	cfgFile, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("while reading image config: %w", err)
	}
	layers, err := img.Layers()
	if err != nil {
		return nil, fmt.Errorf("while listing image layers: %w", err)
	}
	if bootstrapIdx >= len(layers) {
		return nil, fmt.Errorf("nydus bootstrap layer index %d out of range (%d layers)", bootstrapIdx, len(layers))
	}
	dir := s.nydusDir(digest)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("while creating nydus dir: %w", err)
	}
	boot, err := fetchNydusBootstrap(ctx, layers[bootstrapIdx], dir)
	if err != nil {
		return nil, fmt.Errorf("while fetching nydus bootstrap: %w", err)
	}
	rec := imageRecord{Version: 1, Config: cfgFile.Config, Nydus: &nydusRecord{Bootstrap: boot, Repo: repo}}
	if err := s.writeRecord(digest, rec); err != nil {
		return nil, err
	}
	slog.InfoContext(ctx, "Nydus image recorded (lazy)", slog.String("digest", digest.String()), slog.String("bootstrap", boot))
	return &Image{Digest: digest, Config: cfgFile.Config, Nydus: s.nydusSpec(rec.Nydus)}, nil
}

// fetchNydusBootstrap extracts the bootstrap file (image/image.boot as
// written by nydusify) from the bootstrap layer into dir, atomically.
func fetchNydusBootstrap(ctx context.Context, layer v1.Layer, dir string) (string, error) {
	final := filepath.Join(dir, nydusBootstrapFileName)
	if st, err := os.Stat(final); err == nil && st.Size() > 0 {
		return final, nil
	}
	rc, err := layer.Uncompressed()
	if err != nil {
		return "", fmt.Errorf("while opening bootstrap layer: %w", err)
	}
	defer rc.Close()
	tr := tar.NewReader(rc)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return "", errors.New("bootstrap layer has no image.boot entry")
		}
		if err != nil {
			return "", fmt.Errorf("while reading bootstrap layer: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || path.Base(hdr.Name) != "image.boot" {
			continue
		}
		tmp, err := os.CreateTemp(dir, ".bootstrap.tmp-*")
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(tmp, tr); err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return "", fmt.Errorf("while writing bootstrap: %w", err)
		}
		if err := tmp.Close(); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		if err := os.Rename(tmp.Name(), final); err != nil {
			os.Remove(tmp.Name())
			return "", err
		}
		return final, nil
	}
}
