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

// Package resident installs the resident pool demo: two counter templates,
// one with a resident pool, and claimbench, which measures claims from both.
package resident

import (
	"go.ate.dev/substrate/cmd/ate-setup/internal/demos"
	"go.ate.dev/substrate/cmd/ate-setup/internal/steps"
	"go.ate.dev/substrate/internal/resources"
)

// namespace is the pool's k8s namespace; it doubles as the atespace holding
// the demo's ActorTemplates.
const namespace = "ate-demo-resident"

func init() {
	demos.Register(&demos.Substrate{
		DemoName:           "demo-resident",
		Short:              "Claim latency with and without a resident pool",
		WorkerPoolManifest: "demos/resident/resident.yaml.tmpl",
		Deployments:        []steps.TemplateRef{{Atespace: namespace, Name: "resident"}},
		Templates: []demos.SubstrateTemplate{{
			Manifest: "demos/resident/resident-pooled-template.yaml.tmpl",
			Ref:      resources.ActorTemplateRef{Atespace: namespace, Name: "counter-pooled"},
		}, {
			Manifest: "demos/resident/resident-plain-template.yaml.tmpl",
			Ref:      resources.ActorTemplateRef{Atespace: namespace, Name: "counter-plain"},
		}},
	})
}
