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

// Command probe is the test-instrumentation sidecar of the openclaw-fleet
// demo. Substrate has no exec/cp path into a running actor, so the fleet test
// asserts lifecycle guarantees through this HTTP server instead: the in-memory
// boot ID and counter distinguish a FULL-snapshot resume (memory preserved)
// from a cold or data-only boot (memory reset), and the workspace endpoints
// read and write the actor's external CSI volume to prove it survives
// suspend/resume, template repoints, and never leaks across actors.
//
// It listens on a non-80 port (OpenClaw owns port 80, the primary actor port)
// and is reached through atenet-router's CONNECT ingress.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

func main() {
	defaultRev := os.Getenv("FLEET_TEMPLATE_REV")
	if defaultRev == "" {
		defaultRev = "v1"
	}
	port := flag.Int("port", 8080, "Port to listen on (non-80: port 80 belongs to OpenClaw)")
	workspace := flag.String("workspace", "/workspace", "Directory backed by the actor's external volume")
	rev := flag.String("rev", defaultRev, "Template revision marker, used to verify template repoints (default: $FLEET_TEMPLATE_REV)")
	flag.Parse()

	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		log.Fatalf("generating boot id: %v", err)
	}
	bootID := hex.EncodeToString(idBytes)
	bootTime := time.Now()
	var counter atomic.Int64

	mux := http.NewServeMux()

	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		var stat syscall.Statfs_t
		var totalBytes, freeBytes uint64
		writable := false
		if err := syscall.Statfs(*workspace, &stat); err == nil {
			totalBytes = stat.Blocks * uint64(stat.Bsize)
			freeBytes = stat.Bavail * uint64(stat.Bsize)
		}
		if f, err := os.CreateTemp(*workspace, ".probe-writable-*"); err == nil {
			writable = true
			f.Close()
			os.Remove(f.Name())
		}
		writeJSON(w, map[string]any{
			"boot_id":               bootID,
			"rev":                   *rev,
			"counter":               counter.Load(),
			"uptime_s":              int(time.Since(bootTime).Seconds()),
			"workspace_total_bytes": totalBytes,
			"workspace_free_bytes":  freeBytes,
			"workspace_writable":    writable,
		})
	})

	mux.HandleFunc("/bump", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		writeJSON(w, map[string]any{"counter": counter.Add(1)})
	})

	mux.HandleFunc("/workspace/write", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		path, ok := workspacePath(w, *workspace, r.URL.Query().Get("name"))
		if !ok {
			return
		}
		body := make([]byte, 0, 4096)
		buf := make([]byte, 4096)
		for {
			n, err := r.Body.Read(buf)
			body = append(body, buf[:n]...)
			if err != nil {
				break
			}
			if len(body) > 1<<20 {
				http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
				return
			}
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"written": len(body)})
	})

	mux.HandleFunc("/workspace/read", func(w http.ResponseWriter, r *http.Request) {
		path, ok := workspacePath(w, *workspace, r.URL.Query().Get("name"))
		if !ok {
			return
		}
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Write(data)
	})

	mux.HandleFunc("/workspace/list", func(w http.ResponseWriter, _ *http.Request) {
		entries, err := os.ReadDir(*workspace)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		writeJSON(w, map[string]any{"files": names})
	})

	addr := fmt.Sprintf(":%d", *port)
	log.Printf("probe listening on %s (boot_id=%s rev=%s workspace=%s)", addr, bootID, *rev, *workspace)
	log.Fatal(http.ListenAndServe(addr, mux))
}

// workspacePath rejects anything but a single, non-hidden path segment so the
// probe cannot be used to read or write outside the workspace volume.
func workspacePath(w http.ResponseWriter, workspace, name string) (string, bool) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		http.Error(w, "name must be a single non-hidden path segment", http.StatusBadRequest)
		return "", false
	}
	return filepath.Join(workspace, name), true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("encoding response: %v", err)
	}
}
