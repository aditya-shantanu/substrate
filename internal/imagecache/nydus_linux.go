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

//go:build linux

package imagecache

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"
	"golang.org/x/sys/unix"
)

const (
	fuseSuperMagic   = 0x65735546
	nydusMountDir    = "nydus"
	nydusdReadyLimit = 30 * time.Second
)

// nydusdDaemon is the nydusd binary; the consumer image carries it.
var nydusdDaemon = "nydusd"

var nydusdReapers errgroup.Group

// setupNydusRootfs is SetupBundleRootfs for a lazily-loaded image: start a
// nydusd serving the image at <bundle>/nydus (a FUSE mount backed by the
// registry proxy and the node-wide chunk cache) and overlay the bundle's
// private upper on top of it, so the workload sees the same rootfs it would
// over unpacked layers.
func setupNydusRootfs(bundlePath string, spec *OverlaySpec) error {
	n := spec.Nydus
	rootfs := filepath.Join(bundlePath, "rootfs")
	upper := filepath.Join(bundlePath, "upper")
	work := filepath.Join(bundlePath, "work")
	lower := filepath.Join(bundlePath, nydusMountDir)
	for _, d := range []string{rootfs, upper, work, lower, n.CacheDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("while creating %q: %w", d, err)
		}
	}
	_ = unix.Unmount(rootfs, unix.MNT_DETACH)
	_ = unix.Unmount(lower, unix.MNT_DETACH)
	killNydusd(bundlePath)

	backend, err := url.Parse(n.Backend)
	if err != nil || backend.Host == "" {
		return fmt.Errorf("invalid nydus backend URL %q", n.Backend)
	}
	cfg := map[string]any{
		"device": map[string]any{
			"backend": map[string]any{
				"type": "registry",
				"config": map[string]any{
					"scheme": backend.Scheme, "host": backend.Host, "repo": n.Repo,
					"timeout": 30, "connect_timeout": 5, "retry_limit": 2,
				},
			},
			"cache": map[string]any{"type": "blobcache", "config": map[string]any{"work_dir": n.CacheDir}},
		},
		"mode":            "direct",
		"digest_validate": false,
		"iostats_files":   false,
		"enable_xattr":    true,
		"fs_prefetch":     map[string]any{"enable": true, "threads_count": 2, "merging_size": 1048576},
	}
	cfgBytes, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(bundlePath, "nydusd.json")
	if err := os.WriteFile(cfgPath, cfgBytes, 0o600); err != nil {
		return fmt.Errorf("while writing nydusd config: %w", err)
	}
	logPath := filepath.Join(bundlePath, "nydusd.log")
	cmd := exec.Command(nydusdDaemon,
		"--config", cfgPath, "--mountpoint", lower, "--bootstrap", n.Bootstrap,
		"--apisock", filepath.Join(bundlePath, "nydusd.sock"), "--log-level", "warn", "--log-file", logPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("while starting nydusd: %w", err)
	}
	_ = os.WriteFile(filepath.Join(bundlePath, "nydusd.pid"), []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
	nydusdReapers.Go(func() error { _ = cmd.Wait(); return nil })

	deadline := time.Now().Add(nydusdReadyLimit)
	for {
		var st unix.Statfs_t
		if err := unix.Statfs(lower, &st); err == nil && st.Type == fuseSuperMagic {
			break
		}
		if time.Now().After(deadline) {
			killNydusd(bundlePath)
			tail, _ := os.ReadFile(logPath)
			if len(tail) > 800 {
				tail = tail[len(tail)-800:]
			}
			return fmt.Errorf("nydusd did not mount %q within %s: %s", lower, nydusdReadyLimit, strings.TrimSpace(string(tail)))
		}
		time.Sleep(50 * time.Millisecond)
	}

	if err := mountOverlay(rootfs, []string{lower}, upper, work); err != nil {
		killNydusd(bundlePath)
		return fmt.Errorf("while mounting overlay rootfs over nydus at %q: %w", rootfs, err)
	}
	if err := createExtraDirs(rootfs, spec.ExtraDirs); err != nil {
		return err
	}
	return setupImageVolumes(bundlePath, spec.ImageVolumes)
}

// killNydusd stops the nydusd recorded for bundlePath, if any. Unmounting
// the FUSE mount ends the session as well; this covers a daemon that never
// mounted or a mount already detached by UnmountAllUnder.
func killNydusd(bundlePath string) {
	b, err := os.ReadFile(filepath.Join(bundlePath, "nydusd.pid"))
	if err != nil {
		return
	}
	if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	_ = os.Remove(filepath.Join(bundlePath, "nydusd.pid"))
}
