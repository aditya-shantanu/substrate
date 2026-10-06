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

package serverboot

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strconv"
	"strings"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	oteltrace "go.opentelemetry.io/otel/trace"
)

const (
	tracesSamplerEnv    = "OTEL_TRACES_SAMPLER"
	tracesSamplerArgEnv = "OTEL_TRACES_SAMPLER_ARG"
)

// ControlPlaneTraceRatio is the default root sampling ratio for the control
// plane binaries: low volume, and their traces are what lifecycle debugging
// needs, so the default leans generous.
const ControlPlaneTraceRatio = 0.1

// TraceSampling is a resolved sampling policy. The root fraction is kept
// alongside the sampler because the atenet router must mirror it into Envoy's
// RandomSampling percent, which roots the data plane traces.
type TraceSampling struct {
	sampler   sdktrace.Sampler
	rootRatio float64
}

func (s TraceSampling) Sampler() sdktrace.Sampler { return s.sampler }

// RootSamplingPercent is the percentage (0 to 100) of parentless requests
// sampled at the root.
func (s TraceSampling) RootSamplingPercent() float64 { return s.rootRatio * 100 }

// ParentRatioSampling is ParentBased(TraceIDRatioBased(ratio)), clamped to [0, 1].
func ParentRatioSampling(ratio float64) TraceSampling {
	ratio = min(max(ratio, 0), 1)
	return TraceSampling{
		sampler:   sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio)),
		rootRatio: ratio,
	}
}

// ParentNeverSampling is ParentBased(NeverSample()).
func ParentNeverSampling() TraceSampling {
	return TraceSampling{sampler: sdktrace.ParentBased(sdktrace.NeverSample())}
}

// ResolveTraceSampling applies OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG on
// top of the component default. serverboot resolves the env itself because an
// explicit WithSampler silences the SDK's env handling, and the SDK falls open
// to 100% sampling on invalid values where this keeps the default and logs.
// That includes a ratio sampler without an arg, which the spec reads as 1.0.
func ResolveTraceSampling(ctx context.Context, def TraceSampling) TraceSampling {
	name, nameSet := os.LookupEnv(tracesSamplerEnv)
	arg, argSet := os.LookupEnv(tracesSamplerArgEnv)
	resolved, err := resolveTraceSampling(name, nameSet, arg, argSet, def)
	if err != nil {
		slog.WarnContext(ctx, "Invalid trace sampler environment, keeping the component default",
			slog.String("sampler", name),
			slog.String("arg", arg),
			slog.String("default", def.description()),
			slog.Any("err", err))
	}
	return resolved
}

func (s TraceSampling) description() string {
	if s.sampler == nil {
		return "<unset>"
	}
	return s.sampler.Description()
}

func isRatioSampler(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "traceidratio", "parentbased_traceidratio":
		return true
	}
	return false
}

// resolveTraceSampling accepts the sampler names the OTel SDK understands,
// minus the remote ones substrate does not vendor. Any error means def was kept.
func resolveTraceSampling(name string, nameSet bool, arg string, argSet bool, def TraceSampling) (TraceSampling, error) {
	if !nameSet {
		return def, nil
	}
	name = strings.ToLower(strings.TrimSpace(name))
	// Unlike the SDK, treat set-but-empty as unset: templated manifests can
	// render empty env vars.
	if name == "" {
		return def, nil
	}

	var ratio float64
	if isRatioSampler(name) {
		trimmed := strings.TrimSpace(arg)
		if !argSet || trimmed == "" {
			return def, fmt.Errorf("%s %q requires %s to be a ratio in [0, 1]", tracesSamplerEnv, name, tracesSamplerArgEnv)
		}
		parsed, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return def, fmt.Errorf("parse %s %q: %w", tracesSamplerArgEnv, arg, err)
		}
		if parsed < 0 || parsed > 1 {
			return def, fmt.Errorf("%s %q outside [0, 1]", tracesSamplerArgEnv, arg)
		}
		ratio = parsed
	}

	switch name {
	case "always_on":
		return TraceSampling{sampler: sdktrace.AlwaysSample(), rootRatio: 1}, nil
	case "always_off":
		return TraceSampling{sampler: sdktrace.NeverSample()}, nil
	case "traceidratio":
		return TraceSampling{sampler: sdktrace.TraceIDRatioBased(ratio), rootRatio: ratio}, nil
	case "parentbased_always_on":
		return TraceSampling{sampler: sdktrace.ParentBased(sdktrace.AlwaysSample()), rootRatio: 1}, nil
	case "parentbased_always_off":
		return ParentNeverSampling(), nil
	case "parentbased_traceidratio":
		return ParentRatioSampling(ratio), nil
	}
	return def, fmt.Errorf("unsupported %s %q", tracesSamplerEnv, name)
}

// AlwaysSampleSpanNames returns s with its sampler wrapped so that a span
// whose name is in names is always recorded and sampled, whatever its parent
// says, and every other span keeps s's decision. It is how a component roots
// a new, complete trace for a rare and expensive event (an actor wake) while
// the requests around it stay at the head ratio.
func AlwaysSampleSpanNames(s TraceSampling, names ...string) TraceSampling {
	if len(names) == 0 || s.sampler == nil {
		return s
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return TraceSampling{sampler: namedSampler{inner: s.sampler, names: set}, rootRatio: s.rootRatio}
}

type namedSampler struct {
	inner sdktrace.Sampler
	names map[string]struct{}
}

func (n namedSampler) ShouldSample(p sdktrace.SamplingParameters) sdktrace.SamplingResult {
	if _, ok := n.names[p.Name]; ok {
		return sdktrace.SamplingResult{
			Decision:   sdktrace.RecordAndSample,
			Tracestate: oteltrace.SpanContextFromContext(p.ParentContext).TraceState(),
		}
	}
	return n.inner.ShouldSample(p)
}

func (n namedSampler) Description() string {
	names := make([]string, 0, len(n.names))
	for name := range n.names {
		names = append(names, name)
	}
	sort.Strings(names)
	return fmt.Sprintf("AlwaysSampleSpanNames{%s}+%s", strings.Join(names, ","), n.inner.Description())
}
