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

// Package resident installs the resident pool demo: for each guest image a
// counter template with a resident pool and one without, and claimbench,
// which measures claims from both.
package resident

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/google"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"go.ate.dev/substrate/cmd/ate-setup/internal/config"
	"go.ate.dev/substrate/cmd/ate-setup/internal/demos"
	"go.ate.dev/substrate/cmd/ate-setup/internal/steps"
	"go.ate.dev/substrate/internal/resources"
)

const (
	// namespace is the pool's k8s namespace; it doubles as the atespace
	// holding the demo's ActorTemplates.
	namespace = "ate-demo-resident"
	template  = "demos/resident/resident-template.yaml.tmpl"

	counterImage = "ko://go.ate.dev/substrate/demos/counter"

	// defaultGuests spans a 10 MB distroless image to a 2.4 GB SWE-rebench
	// environment. The held memory grows with the image as a stand-in for
	// the working set such an environment carries, because a restore's cost
	// follows memory, not rootfs size.
	defaultGuests = "tiny|0|256|" + counterImage +
		";slim|64|512|docker.io/library/python:3.12-slim@sha256:05a994d2344eb364cc186910f4fc8786a38215f39f00d0acc25bff33a08f4177" +
		";python|192|768|docker.io/library/python:3.12@sha256:97dc788c1887c44354e700c624e7d0a7b61c6f5baf6e2ae4d767abfedc442435" +
		";cuda|384|1024|docker.io/nvidia/cuda:12.4.1-runtime-ubuntu22.04@sha256:cff3a0d82d2c2b47bab252d67fa9b34a20ef4c50781d98501b5c7367ea9afd10" +
		";swe-rebench|768|1536|us-docker.pkg.dev/gke-ai-eco-dev/hicard/mldataset_nongoogle_swe_rebench/swerebench/sweb.eval.x86_64.12rambau_1776_sepal_ui-411@sha256:89a8ffbc06872074357ab153aa6bb4386280e041d9d7e426d91d31dcc32a0fa6"
)

// guest is one image the demo claims from, with and without a pool.
type guest struct {
	label     string
	touchMiB  int
	memoryMiB int
	image     string
}

// parseGuests reads "label|touchMiB|memoryMiB|image;..." as the guests
// setting is written.
func parseGuests(spec string) ([]guest, error) {
	var guests []guest
	for _, entry := range strings.Split(spec, ";") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		fields := strings.SplitN(entry, "|", 4)
		if len(fields) != 4 {
			return nil, fmt.Errorf("guest %q: want label|touchMiB|memoryMiB|image", entry)
		}
		touch, err := strconv.Atoi(fields[1])
		if err != nil || touch < 0 {
			return nil, fmt.Errorf("guest %q: bad touchMiB %q", entry, fields[1])
		}
		memory, err := strconv.Atoi(fields[2])
		if err != nil || memory <= touch {
			return nil, fmt.Errorf("guest %q: memoryMiB %q must exceed touchMiB", entry, fields[2])
		}
		guests = append(guests, guest{label: fields[0], touchMiB: touch, memoryMiB: memory, image: fields[3]})
	}
	if len(guests) == 0 {
		return nil, fmt.Errorf("no guests in %q", spec)
	}
	return guests, nil
}

type demo struct {
	demos.Substrate
}

func (d *demo) Settings() []config.Setting {
	return []config.Setting{
		{
			Key: "demo.resident.guests", Env: "ATE_DEMO_RESIDENT_GUESTS",
			Flag: "guests", Kind: config.KindString, Default: defaultGuests,
			Usage: "Guests to claim from, as label|touchMiB|memoryMiB|image separated by ';'. Each gets a <label>-pooled and a <label>-plain template. image is digest-pinned or ko://; the counter binary is mounted into other images at /ate.",
		},
		{
			Key: "demo.resident.replicas", Env: "ATE_DEMO_RESIDENT_REPLICAS",
			Flag: "replicas", Kind: config.KindInt, Default: "4",
			Usage: "Residents each pooled template keeps running",
		},
	}
}

// templatesFor renders the one template manifest twice per guest.
func (d *demo) templatesFor(e *steps.Env) error {
	guests, err := parseGuests(e.Cfg.Resolved().String("demo.resident.guests"))
	if err != nil {
		return err
	}
	replicas := e.Cfg.Resolved().Int("demo.resident.replicas")
	d.Templates = d.Templates[:0]
	for _, g := range guests {
		for _, variant := range []string{"pooled", "plain"} {
			name := g.label + "-" + variant
			values := map[string]string{
				"NAME":                 name,
				"GUEST_IMAGE":          g.image,
				"COUNTER_COMMAND":      "/ko-app/counter",
				"ACTOR_MEMORY_BYTES":   strconv.Itoa(g.memoryMiB << 20),
				"RESIDENT_POOL":        "residentPool:\n  replicas: " + strconv.Itoa(replicas),
				"TOUCH_MEMORY_ARG":     "  - --touch-memory-mb=" + strconv.Itoa(g.touchMiB),
				"COUNTER_VOLUME_MOUNT": "  - name: counter-bin\n    mountPath: /ate",
				"COUNTER_VOLUME":       "- name: counter-bin\n  image:\n    reference: " + counterImage,
			}
			var drop []string
			if g.image == counterImage {
				drop = append(drop, "COUNTER_VOLUME_MOUNT", "COUNTER_VOLUME")
			} else {
				values["COUNTER_COMMAND"] = "/ate/ko-app/counter"
			}
			if g.touchMiB == 0 {
				drop = append(drop, "TOUCH_MEMORY_ARG")
			}
			if variant == "plain" {
				drop = append(drop, "RESIDENT_POOL")
			}
			d.Templates = append(d.Templates, demos.SubstrateTemplate{
				Manifest: template,
				Ref:      resources.ActorTemplateRef{Atespace: namespace, Name: name},
				Values:   values,
				Drop:     drop,
			})
		}
	}
	return nil
}

// guestSizes renders the --guest-sizes claimbench shows, "label=MB,...": the
// compressed size of each guest image, read here because the pod cannot read
// private registries. A ko:// guest is built later and reads as 0.
func guestSizes(ctx context.Context, guests []guest) string {
	var parts []string
	for _, g := range guests {
		var size int64
		if ref, err := name.ParseReference(g.image); err == nil {
			img, err := remote.Image(ref, remote.WithContext(ctx), remote.WithAuthFromKeychain(authn.NewMultiKeychain(authn.DefaultKeychain, google.Keychain)))
			if err == nil {
				if m, err := img.Manifest(); err == nil {
					for _, l := range m.Layers {
						size += l.Size
					}
				}
			}
		}
		parts = append(parts, g.label+"="+strconv.FormatInt(size/1e6, 10))
	}
	return strings.Join(parts, ",")
}

func (d *demo) renderValues(ctx context.Context, e *steps.Env) (map[string]string, error) {
	guests, err := parseGuests(e.Cfg.Resolved().String("demo.resident.guests"))
	if err != nil {
		return nil, err
	}
	return map[string]string{"GUEST_SIZES": guestSizes(ctx, guests)}, nil
}

func (d *demo) Deploy(ctx context.Context, e *steps.Env) error {
	if err := d.templatesFor(e); err != nil {
		return err
	}
	return d.Substrate.Deploy(ctx, e)
}

func (d *demo) Delete(ctx context.Context, e *steps.Env) ([]string, error) {
	if err := d.templatesFor(e); err != nil {
		return nil, err
	}
	return d.Substrate.Delete(ctx, e)
}

func init() {
	d := &demo{}
	d.Substrate = demos.Substrate{
		DemoName:           "demo-resident",
		Short:              "Claim latency with and without a resident pool, across guest images",
		WorkerPoolManifest: "demos/resident/resident.yaml.tmpl",
		Deployments:        []steps.TemplateRef{{Atespace: namespace, Name: "resident"}},
		// The largest default guest is a 2.4 GB image; its first pull and
		// golden build take longer than a demo usually does.
		GoldenTimeout: 20 * time.Minute,
		RenderValues:  d.renderValues,
	}
	demos.Register(d)
}
