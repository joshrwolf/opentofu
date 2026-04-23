// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0

// This file is self-contained: it owns the synthetic topology definitions,
// the on-disk generator, the JSON export format, the bench-specific span
// query helpers, the benchmarks themselves, and the end-to-end sanity tests
// exercised by `go test`. Runner.Run (run.go) is shared with txtar_test.go;
// everything else here is bench-only.

package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"maps"
	"os"
	"path/filepath"
	"runtime/pprof"
	"runtime/trace"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/opentofu/opentofu/internal/build/catalog"
	"github.com/opentofu/opentofu/internal/modsdir"
)

// -----------------------------------------------------------------------------
// Topology
// -----------------------------------------------------------------------------

type Topology struct {
	Images                      int  `json:"images"`
	Charts                      int  `json:"charts"`
	PublicImages                int  `json:"public_images"`
	RepeatKeys                  int  `json:"repeat_keys"`
	RepeatKeyJitter             int  `json:"repeat_key_jitter"`
	SupportedResourcesPerBuild  int  `json:"supported_resources_per_build"`
	SupportedDataSources        int  `json:"supported_data_sources"`
	ExternalDataSourcesPerBuild int  `json:"external_data_sources_per_build"`
	UnsupportedResources        int  `json:"unsupported_resources"`
	QuerySafeCalls              int  `json:"query_safe_calls"`
	EffectfulCalls              int  `json:"effectful_calls"`
	AliasModuleEvery            int  `json:"alias_module_every"`
	RootSummaryOutputs          bool `json:"root_summary_outputs"`
}

// TinyTopology, FastTopology, and RealTopology share the same per-leaf shape;
// only the leaf count varies. Fast is the iteration target for perf work; Real
// is ground truth calibrated against images-private.
func baseTopology() Topology {
	return Topology{
		RepeatKeys:                 16,
		RepeatKeyJitter:            4,
		SupportedResourcesPerBuild: 2,
		SupportedDataSources:       1,
		UnsupportedResources:       1,
		QuerySafeCalls:             2,
		EffectfulCalls:             1,
		AliasModuleEvery:           256,
		RootSummaryOutputs:         true,
	}
}

func TinyTopology() Topology {
	t := baseTopology()
	t.Images, t.Charts, t.PublicImages = 8, 1, 1
	return t
}

func FastTopology() Topology {
	t := baseTopology()
	t.Images, t.Charts, t.PublicImages = 181, 13, 6
	return t
}

func MediumTopology() Topology {
	t := baseTopology()
	t.Images, t.Charts, t.PublicImages = 453, 33, 14
	return t
}

func RealTopology() Topology {
	t := baseTopology()
	t.Images, t.Charts, t.PublicImages = 1741, 125, 54
	return t
}

func (t Topology) Validate() error {
	switch {
	case t.Images < 0:
		return fmt.Errorf("images must be non-negative")
	case t.Charts < 0:
		return fmt.Errorf("charts must be non-negative")
	case t.PublicImages < 0:
		return fmt.Errorf("public images must be non-negative")
	case t.RepeatKeys < 1:
		return fmt.Errorf("repeat keys must be at least 1")
	case t.RepeatKeyJitter < 0:
		return fmt.Errorf("repeat key jitter must be non-negative")
	case t.SupportedResourcesPerBuild < 0:
		return fmt.Errorf("supported resources per build must be non-negative")
	case t.SupportedDataSources < 0:
		return fmt.Errorf("supported data sources must be non-negative")
	case t.ExternalDataSourcesPerBuild < 0:
		return fmt.Errorf("external data sources per build must be non-negative")
	case t.UnsupportedResources < 0:
		return fmt.Errorf("unsupported resources must be non-negative")
	case t.QuerySafeCalls < 0:
		return fmt.Errorf("query-safe calls must be non-negative")
	case t.EffectfulCalls < 0:
		return fmt.Errorf("effectful calls must be non-negative")
	case t.AliasModuleEvery < 0:
		return fmt.Errorf("alias module interval must be non-negative")
	}
	if t.ModuleCount() == 0 {
		return fmt.Errorf("topology must declare at least one root module")
	}
	return nil
}

func (t Topology) ModuleCount() int {
	return t.Images + t.Charts + t.PublicImages
}

type moduleDescriptor struct {
	Index int
	Name  string
	Dir   string
	Alias bool
}

func (t Topology) moduleDescriptors() []moduleDescriptor {
	ret := make([]moduleDescriptor, 0, t.ModuleCount())
	index := 0
	for i := range t.Images {
		ret = append(ret, moduleDescriptor{
			Index: index,
			Name:  fmt.Sprintf("image_%04d", i),
			Dir:   fmt.Sprintf("images/image_%04d", i),
			Alias: t.aliasForIndex(index),
		})
		index++
	}
	for i := range t.Charts {
		ret = append(ret, moduleDescriptor{
			Index: index,
			Name:  fmt.Sprintf("chart_%04d", i),
			Dir:   fmt.Sprintf("charts/chart_%04d", i),
			Alias: t.aliasForIndex(index),
		})
		index++
	}
	for i := range t.PublicImages {
		ret = append(ret, moduleDescriptor{
			Index: index,
			Name:  fmt.Sprintf("public_%04d", i),
			Dir:   fmt.Sprintf("public/images/public_%04d", i),
			Alias: t.aliasForIndex(index),
		})
		index++
	}
	return ret
}

func (t Topology) aliasForIndex(index int) bool {
	return t.AliasModuleEvery > 0 && index%t.AliasModuleEvery == 0
}

func (t Topology) repeatKeysForDescriptor(descriptor moduleDescriptor) int {
	keys := t.RepeatKeys
	if t.RepeatKeyJitter == 0 {
		return keys
	}

	hasher := fnv.New32a()
	_, _ = hasher.Write([]byte(descriptor.Name))
	_, _ = hasher.Write([]byte{0})
	_, _ = hasher.Write([]byte(descriptor.Dir))

	span := 2*t.RepeatKeyJitter + 1
	delta := int(hasher.Sum32()%uint32(span)) - t.RepeatKeyJitter
	keys += delta
	if keys < 1 {
		return 1
	}
	return keys
}

// -----------------------------------------------------------------------------
// Synthetic project generator
// -----------------------------------------------------------------------------

func Generate(rootDir string, topology Topology) error {
	if err := topology.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(rootDir, 0o755); err != nil {
		return fmt.Errorf("create root dir: %w", err)
	}

	descriptors := topology.moduleDescriptors()
	if err := writeFile(rootDir, "providers.tf", rootProvidersTF()); err != nil {
		return err
	}
	if err := writeFile(rootDir, "generated.tf", rootGeneratedTF(topology, descriptors)); err != nil {
		return err
	}
	if err := writeManifest(rootDir, descriptors); err != nil {
		return err
	}

	for _, descriptor := range descriptors {
		if err := generateLeafModule(rootDir, descriptor, topology); err != nil {
			return err
		}
	}

	return nil
}

func rootProvidersTF() string {
	return strings.TrimSpace(`
terraform {
  required_providers {
    test = {
      source = "hashicorp/test"
    }
    null = {
      source = "hashicorp/null"
    }
    oci = {
      source = "chainguard-dev/oci"
    }
  }
}

provider "test" {}

provider "test" {
  alias = "alt"
}
`)
}

func rootGeneratedTF(topology Topology, descriptors []moduleDescriptor) string {
	var b strings.Builder
	b.WriteString("# generated by internal/build/bench\n\n")
	for _, descriptor := range descriptors {
		fmt.Fprintf(&b, "module %q {\n", descriptor.Name)
		if descriptor.Alias {
			b.WriteString("  providers = {\n")
			b.WriteString("    test.alt = test.alt\n")
			b.WriteString("  }\n")
		}
		fmt.Fprintf(&b, "  source             = %q\n", "./"+filepath.ToSlash(descriptor.Dir))
		fmt.Fprintf(&b, "  target_repository  = %q\n", "example.invalid/target/"+descriptor.Name)
		fmt.Fprintf(&b, "  test_repository    = %q\n", "cgr.dev/example")
		fmt.Fprintf(&b, "  scratch_repository = %q\n", "example.invalid/scratch")
		b.WriteString("}\n\n")
	}
	if topology.RootSummaryOutputs {
		for _, descriptor := range descriptors {
			fmt.Fprintf(&b, "output %q {\n", "summary_"+descriptor.Name)
			fmt.Fprintf(&b, "  value = module.%s.summary\n", descriptor.Name)
			b.WriteString("}\n\n")
		}
	}
	return strings.TrimSpace(b.String()) + "\n"
}

func generateLeafModule(rootDir string, descriptor moduleDescriptor, topology Topology) error {
	leafDir := filepath.Join(rootDir, filepath.FromSlash(descriptor.Dir))
	if err := os.MkdirAll(leafDir, 0o755); err != nil {
		return fmt.Errorf("create leaf dir %s: %w", descriptor.Dir, err)
	}

	if err := writeFile(rootDir, filepath.Join(descriptor.Dir, "main.tf"), leafMainTF(descriptor, topology)); err != nil {
		return err
	}
	locksJSON, err := leafLocksJSON(descriptor, topology)
	if err != nil {
		return err
	}
	if err := writeFile(rootDir, filepath.Join(descriptor.Dir, "locks.json"), locksJSON); err != nil {
		return err
	}
	if err := writeFile(rootDir, filepath.Join(descriptor.Dir, "build", "main.tf"), buildModuleTF(topology)); err != nil {
		return err
	}
	if err := writeFile(rootDir, filepath.Join(descriptor.Dir, "tests", "main.tf"), testsModuleTF()); err != nil {
		return err
	}
	if err := writeFile(rootDir, filepath.Join(descriptor.Dir, "tagger", "main.tf"), taggerModuleTF()); err != nil {
		return err
	}

	return nil
}

func leafMainTF(descriptor moduleDescriptor, topology Topology) string {
	var b strings.Builder
	b.WriteString("terraform {\n")
	b.WriteString("  required_providers {\n")
	b.WriteString("    test = {\n")
	b.WriteString("      source = \"hashicorp/test\"\n")
	if descriptor.Alias {
		b.WriteString("      configuration_aliases = [test.alt]\n")
	}
	b.WriteString("    }\n")
	b.WriteString("    null = {\n")
	b.WriteString("      source = \"hashicorp/null\"\n")
	b.WriteString("    }\n")
	b.WriteString("    oci = {\n")
	b.WriteString("      source = \"chainguard-dev/oci\"\n")
	b.WriteString("    }\n")
	b.WriteString("  }\n")
	b.WriteString("}\n\n")

	b.WriteString(strings.TrimSpace(`
variable "target_repository" {
  type = string
}

variable "test_repository" {
  type = string
}

variable "scratch_repository" {
  type = string
}

locals {
  locked_configs = jsondecode(file("${path.module}/locks.json")).imageLocks
}
`))
	b.WriteString("\n\n")

	b.WriteString("module \"build\" {\n")
	if descriptor.Alias {
		b.WriteString("  providers = {\n")
		b.WriteString("    test = test.alt\n")
		b.WriteString("  }\n")
	}
	b.WriteString("  for_each        = local.locked_configs\n")
	b.WriteString("  source          = \"./build\"\n")
	b.WriteString("  value           = each.value.value\n")
	b.WriteString("  repository      = each.value.repo\n")
	b.WriteString("  test_repository = var.test_repository\n")
	b.WriteString("}\n\n")

	b.WriteString(strings.TrimSpace(`
module "test" {
  for_each = local.locked_configs
  source   = "./tests"
  value    = module.build[each.key].summary.value
}

module "tagger" {
  source     = "./tagger"
  depends_on = [module.test]
  values     = { for key, value in module.build : key => value.summary.value }
}

output "summary" {
  value = { for key, value in module.build : key => value.summary }
}
`))
	if topology.EffectfulCalls > 0 {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(`
output "remote_refs" {
  value = { for key, value in module.build : key => value.remote_refs }
}
`))
	}

	return b.String() + "\n"
}

func leafLocksJSON(descriptor moduleDescriptor, topology Topology) (string, error) {
	type lockEntry struct {
		Repo  string `json:"repo"`
		Value string `json:"value"`
	}
	repeatKeys := topology.repeatKeysForDescriptor(descriptor)
	payload := struct {
		ImageLocks map[string]lockEntry `json:"imageLocks"`
	}{
		ImageLocks: make(map[string]lockEntry, repeatKeys),
	}
	for i := range repeatKeys {
		payload.ImageLocks[fmt.Sprintf("%s_lock_%04d", descriptor.Name, i)] = lockEntry{
			Repo:  fmt.Sprintf("%s-repo-%04d", descriptor.Name, i),
			Value: fmt.Sprintf("%s-value-%04d", descriptor.Name, i),
		}
	}
	src, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal locks for %s: %w", descriptor.Name, err)
	}
	return string(src), nil
}

func buildModuleTF(topology Topology) string {
	var b strings.Builder
	b.WriteString("terraform {\n")
	b.WriteString("  required_providers {\n")
	b.WriteString("    test = {\n")
	b.WriteString("      source = \"hashicorp/test\"\n")
	b.WriteString("    }\n")
	b.WriteString("    null = {\n")
	b.WriteString("      source = \"hashicorp/null\"\n")
	b.WriteString("    }\n")
	b.WriteString("    oci = {\n")
	b.WriteString("      source = \"chainguard-dev/oci\"\n")
	b.WriteString("    }\n")
	if topology.ExternalDataSourcesPerBuild > 0 {
		b.WriteString("    external = {\n")
		b.WriteString("      source = \"hashicorp/external\"\n")
		b.WriteString("    }\n")
	}
	b.WriteString("  }\n")
	b.WriteString("}\n\n")

	b.WriteString(strings.TrimSpace(`
variable "value" {
  type = string
}

variable "repository" {
  type = string
}

variable "test_repository" {
  type = string
}
`))
	b.WriteString("\n\nlocals {\n")
	for i := range max(1, topology.QuerySafeCalls) {
		fmt.Fprintf(&b, "  parsed_%03d = provider::oci::parse(\"cgr.dev/example/%s@sha256:%064x\")\n", i, "${var.repository}", i+1)
	}
	for i := range topology.EffectfulCalls {
		fmt.Fprintf(&b, "  fetched_%03d = provider::oci::get(\"${var.test_repository}/${var.repository}:%s\")\n", i, effectfulTag(i))
	}
	b.WriteString("}\n\n")

	for i := range topology.ExternalDataSourcesPerBuild {
		fmt.Fprintf(&b, "data \"external\" \"cache_%03d\" {\n", i)
		b.WriteString("  program = [\"sh\", \"-c\", \"printf '{\\\"image_ref\\\":\\\"\\\"}'\"]\n")
		b.WriteString("  query = {\n")
		b.WriteString("    cache_key  = sha256(var.repository)\n")
		b.WriteString("    repository = var.repository\n")
		fmt.Fprintf(&b, "    slot       = %q\n", fmt.Sprintf("%03d", i))
		b.WriteString("  }\n")
		b.WriteString("}\n\n")
	}

	for i := range topology.SupportedResourcesPerBuild {
		fmt.Fprintf(&b, "resource \"test_resource\" \"build_%03d\" {\n", i)
		fmt.Fprintf(&b, "  value = \"${var.value}-build-%03d\"\n", i)
		b.WriteString("}\n\n")
	}

	for i := range topology.EffectfulCalls {
		fmt.Fprintf(&b, "resource \"test_resource\" \"remote_%03d\" {\n", i)
		fmt.Fprintf(&b, "  value = local.fetched_%03d.full_ref\n", i)
		b.WriteString("}\n\n")
	}

	for i := range topology.SupportedDataSources {
		fmt.Fprintf(&b, "data \"test_data_source\" \"lookup_%03d\" {\n", i)
		fmt.Fprintf(&b, "  id = \"lookup-${var.repository}-%03d\"\n", i)
		b.WriteString("}\n\n")
	}

	for i := range topology.UnsupportedResources {
		fmt.Fprintf(&b, "resource \"null_resource\" \"legacy_%03d\" {\n", i)
		b.WriteString("  triggers = {\n")
		fmt.Fprintf(&b, "    id = \"legacy-${var.repository}-%03d\"\n", i)
		b.WriteString("  }\n")
		b.WriteString("  provisioner \"local-exec\" {\n")
		fmt.Fprintf(&b, "    command = \"echo legacy-${var.repository}-%03d\"\n", i)
		b.WriteString("  }\n")
		b.WriteString("}\n\n")
	}

	b.WriteString("output \"summary\" {\n")
	b.WriteString("  value = {\n")
	b.WriteString("    value = var.value\n")
	for i := range topology.SupportedResourcesPerBuild {
		fmt.Fprintf(&b, "    build_%03d = test_resource.build_%03d.value\n", i, i)
	}
	for i := range topology.QuerySafeCalls {
		fmt.Fprintf(&b, "    parsed_%03d = local.parsed_%03d.repo\n", i, i)
	}
	b.WriteString("  }\n")
	b.WriteString("}\n")
	if topology.EffectfulCalls > 0 {
		b.WriteString("\noutput \"remote_refs\" {\n")
		b.WriteString("  value = {\n")
		for i := range topology.EffectfulCalls {
			fmt.Fprintf(&b, "    ref_%03d = local.fetched_%03d.full_ref\n", i, i)
		}
		b.WriteString("  }\n")
		b.WriteString("}\n")
	}

	return b.String()
}

func testsModuleTF() string {
	return strings.TrimSpace(`
variable "value" {
  type = string
}

output "value" {
  value = var.value
}
`) + "\n"
}

func taggerModuleTF() string {
	return strings.TrimSpace(`
variable "values" {
  type = map(string)
}

output "summary" {
  value = var.values
}
`) + "\n"
}

func writeManifest(rootDir string, descriptors []moduleDescriptor) error {
	modulesDir := filepath.Join(rootDir, ".terraform", "modules")
	if err := os.MkdirAll(modulesDir, 0o755); err != nil {
		return fmt.Errorf("create modules dir: %w", err)
	}

	manifest := modsdir.Manifest{
		"": {
			Key: "",
			Dir: rootDir,
		},
	}
	for _, descriptor := range descriptors {
		moduleDir := filepath.Join(rootDir, filepath.FromSlash(descriptor.Dir))
		manifest[descriptor.Name] = modsdir.Record{
			Key:        descriptor.Name,
			SourceAddr: "./" + filepath.ToSlash(descriptor.Dir),
			Dir:        moduleDir,
		}
		for childName, childDir := range map[string]string{
			"build":  "build",
			"test":   "tests",
			"tagger": "tagger",
		} {
			key := descriptor.Name + "." + childName
			manifest[key] = modsdir.Record{
				Key:        key,
				SourceAddr: "./" + childDir,
				Dir:        filepath.Join(moduleDir, childDir),
			}
		}
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		return fmt.Errorf("write modules manifest: %w", err)
	}
	return nil
}

func writeFile(rootDir, relPath, content string) error {
	path := filepath.Join(rootDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent dir for %s: %w", relPath, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", relPath, err)
	}
	return nil
}

func effectfulTag(index int) string {
	if index == 0 {
		return "latest"
	}
	return fmt.Sprintf("v%d", index)
}

// -----------------------------------------------------------------------------
// Span query helpers (bench-only)
// -----------------------------------------------------------------------------

func FindSpan(spans tracetest.SpanStubs, name string) (tracetest.SpanStub, bool) {
	for _, s := range spans {
		if s.Name == name {
			return s, true
		}
	}
	return tracetest.SpanStub{}, false
}

func SpanDuration(spans tracetest.SpanStubs, name string) time.Duration {
	if s, ok := FindSpan(spans, name); ok {
		return s.EndTime.Sub(s.StartTime)
	}
	return 0
}

func SpanAttrInt64(spans tracetest.SpanStubs, spanName, key string) int64 {
	if s, ok := FindSpan(spans, spanName); ok {
		for _, attr := range s.Attributes {
			if string(attr.Key) == key {
				return attr.Value.AsInt64()
			}
		}
	}
	return 0
}

func FirstEventTime(spans tracetest.SpanStubs, eventName string, start time.Time) (time.Duration, bool) {
	return firstEventTimeMatching(spans, eventName, start, nil)
}

func FirstProviderEventTime(spans tracetest.SpanStubs, eventName string, start time.Time) (time.Duration, bool) {
	return firstEventTimeMatching(spans, eventName, start, isProviderAttr)
}

func FirstSpanStartTime(spans tracetest.SpanStubs, spanName string, start time.Time) (time.Duration, bool) {
	return firstSpanStartMatching(spans, spanName, start, nil)
}

func FirstProviderSpanStartTime(spans tracetest.SpanStubs, spanName string, start time.Time) (time.Duration, bool) {
	return firstSpanStartMatching(spans, spanName, start, isProviderAttr)
}

func firstEventTimeMatching(spans tracetest.SpanStubs, eventName string, start time.Time, match func([]attribute.KeyValue) bool) (time.Duration, bool) {
	var earliest time.Time
	found := false
	for _, s := range spans {
		for _, ev := range s.Events {
			if ev.Name != eventName {
				continue
			}
			if match != nil && !match(ev.Attributes) {
				continue
			}
			if !found || ev.Time.Before(earliest) {
				earliest = ev.Time
				found = true
			}
		}
	}
	if !found {
		return 0, false
	}
	return earliest.Sub(start), true
}

func firstSpanStartMatching(spans tracetest.SpanStubs, spanName string, start time.Time, match func([]attribute.KeyValue) bool) (time.Duration, bool) {
	var earliest time.Time
	found := false
	for _, s := range spans {
		if s.Name != spanName {
			continue
		}
		if match != nil && !match(s.Attributes) {
			continue
		}
		if !found || s.StartTime.Before(earliest) {
			earliest = s.StartTime
			found = true
		}
	}
	if !found {
		return 0, false
	}
	return earliest.Sub(start), true
}

func isProviderAttr(attrs []attribute.KeyValue) bool {
	for _, attr := range attrs {
		if string(attr.Key) != "build.action.runner" {
			continue
		}
		switch catalog.RunnerKind(attr.Value.AsString()) {
		case catalog.RunnerKindProviderFunction, catalog.RunnerKindProviderResource, catalog.RunnerKindProviderData:
			return true
		}
	}
	return false
}

func InvalidationRounds(spans tracetest.SpanStubs) int {
	rounds := 0
	for _, s := range spans {
		for _, ev := range s.Events {
			if ev.Name == "build.invalidate" {
				rounds++
			}
		}
	}
	return rounds
}

// -----------------------------------------------------------------------------
// Export (JSON summary written per sub-benchmark when TOFU_BUILD_BENCH_EXPORT_DIR is set)
// -----------------------------------------------------------------------------

const exportVersion = "build-bench-summary-v4"

type Export struct {
	Version             string            `json:"version"`
	Benchmark           string            `json:"benchmark"`
	TopologyLabel       string            `json:"topology_label"`
	Topology            Topology          `json:"topology"`
	Scenario            ExportScenario    `json:"scenario"`
	Timings             ExportTimings     `json:"timings"`
	Counts              ExportCounts      `json:"counts"`
	Execution           ExportExecution   `json:"execution"`
	Dice                []ExportDiceQuery `json:"dice,omitempty"`
	Heap                ExportHeap        `json:"heap"`
	DiagnosticSummaries map[string]int    `json:"diagnostic_summaries,omitempty"`
}

type ExportScenario struct {
	Name      string   `json:"name"`
	Selectors []string `json:"selectors,omitempty"`
}

type ExportTimings struct {
	TotalNS                 int64 `json:"total_ns"`
	LoadNS                  int64 `json:"load_ns"`
	NewEngineNS             int64 `json:"new_engine_ns"`
	NewInspectorNS          int64 `json:"new_inspector_ns,omitempty"`
	ExecuteNS               int64 `json:"execute_ns"`
	FirstSubmitNS           int64 `json:"first_submit_ns,omitempty"`
	SawSubmit               bool  `json:"saw_submit"`
	FirstProviderSubmitNS   int64 `json:"first_provider_submit_ns,omitempty"`
	SawProviderSubmit       bool  `json:"saw_provider_submit"`
	FirstDispatchNS         int64 `json:"first_dispatch_ns,omitempty"`
	SawDispatch             bool  `json:"saw_dispatch"`
	FirstProviderDispatchNS int64 `json:"first_provider_dispatch_ns,omitempty"`
	SawProviderDispatch     bool  `json:"saw_provider_dispatch"`
	FirstRunnerNS           int64 `json:"first_runner_ns,omitempty"`
	SawRunner               bool  `json:"saw_runner"`
	FirstProviderRunnerNS   int64 `json:"first_provider_runner_ns,omitempty"`
	SawProviderRunner       bool  `json:"saw_provider_runner"`
}

type ExportDiceQuery struct {
	Name      string `json:"name"`
	Entries   int    `json:"entries"`
	Gets      int64  `json:"gets"`
	FastPath  int64  `json:"fast_path"`
	
	Computed  int64  `json:"computed"`
	Coalesced int64  `json:"coalesced"`
}

type ExportHeap struct {
	HeapAllocBytes uint64 `json:"heap_alloc_bytes"`
	HeapObjects    uint64 `json:"heap_objects"`
	TotalAlloc     uint64 `json:"total_alloc"`
	Mallocs        uint64 `json:"mallocs"`
	GCCycles       uint32 `json:"gc_cycles"`
	GCPauseNs      uint64 `json:"gc_pause_ns"`
}

type ExportCounts struct {
	SelectedTargets    int `json:"selected_targets"`
	SelectedOutputs    int `json:"selected_outputs"`
	Submitted          int `json:"submitted"`
	Diagnostics        int `json:"diagnostics"`
	ErrorDiagnostics   int `json:"error_diagnostics"`
	WarningDiagnostics int `json:"warning_diagnostics"`
}

type ExportExecution struct {
	Submitted          int `json:"submitted"`
	Executed           int `json:"executed"`
	Cached             int `json:"cached"`
	Failed             int `json:"failed"`
	InvalidationRounds int `json:"invalidation_rounds"`
}

func ExportDirFromEnv() string {
	return os.Getenv("TOFU_BUILD_BENCH_EXPORT_DIR")
}

var benchNameReplacer = strings.NewReplacer(
	"/", "__",
	"\\", "__",
	" ", "_",
	":", "_",
)

func WriteExport(dir string, benchmark string, topologyLabel string, topology Topology, result Result) error {
	if dir == "" {
		return nil
	}

	firstSubmitNS, sawSubmit := timingNS(FirstEventTime(result.Spans, "build.submit", result.Start))
	firstProviderSubmitNS, sawProviderSubmit := timingNS(FirstProviderEventTime(result.Spans, "build.submit", result.Start))
	firstDispatchNS, sawDispatch := timingNS(FirstSpanStartTime(result.Spans, "dispatch.execute", result.Start))
	firstProviderDispatchNS, sawProviderDispatch := timingNS(FirstProviderSpanStartTime(result.Spans, "dispatch.execute", result.Start))
	firstRunnerNS, sawRunner := timingNS(FirstSpanStartTime(result.Spans, "build.Runner.Run", result.Start))
	firstProviderRunnerNS, sawProviderRunner := timingNS(FirstProviderSpanStartTime(result.Spans, "build.Runner.Run", result.Start))

	submitted, executed, cached, failed := ActionCounts(result.Spans)

	summary := Export{
		Version:       exportVersion,
		Benchmark:     benchmark,
		TopologyLabel: topologyLabel,
		Topology:      topology,
		Scenario: ExportScenario{
			Name:      result.Scenario,
			Selectors: append([]string(nil), result.Selectors...),
		},
		Timings: ExportTimings{
			TotalNS:                 result.Total.Nanoseconds(),
			LoadNS:                  SpanDuration(result.Spans, "build.Load").Nanoseconds(),
			NewEngineNS:             SpanDuration(result.Spans, "build.NewEngine").Nanoseconds(),
			NewInspectorNS:          SpanDuration(result.Spans, "build.NewInspector").Nanoseconds(),
			ExecuteNS:               SpanDuration(result.Spans, "build.Execute").Nanoseconds(),
			FirstSubmitNS:           firstSubmitNS,
			SawSubmit:               sawSubmit,
			FirstProviderSubmitNS:   firstProviderSubmitNS,
			SawProviderSubmit:       sawProviderSubmit,
			FirstDispatchNS:         firstDispatchNS,
			SawDispatch:             sawDispatch,
			FirstProviderDispatchNS: firstProviderDispatchNS,
			SawProviderDispatch:     sawProviderDispatch,
			FirstRunnerNS:           firstRunnerNS,
			SawRunner:               sawRunner,
			FirstProviderRunnerNS:   firstProviderRunnerNS,
			SawProviderRunner:       sawProviderRunner,
		},
		Counts: ExportCounts{
			SelectedTargets:    SpanAttrInt(result.Spans, "build.Execute", "build.selected_targets"),
			SelectedOutputs:    SpanAttrInt(result.Spans, "build.Execute", "build.selected_outputs"),
			Submitted:          int(SpanAttrInt64(result.Spans, "build.Execute", "build.submitted")),
			Diagnostics:        result.Diagnostics,
			ErrorDiagnostics:   result.ErrorDiagnostics,
			WarningDiagnostics: result.WarningDiagnostics,
		},
		Execution: ExportExecution{
			Submitted:          submitted,
			Executed:           executed,
			Cached:             cached,
			Failed:             failed,
			InvalidationRounds: InvalidationRounds(result.Spans),
		},
		Dice: exportDiceStats(result),
		Heap: ExportHeap{
			HeapAllocBytes: result.HeapStats.HeapAllocBytes,
			HeapObjects:    result.HeapStats.HeapObjects,
			TotalAlloc:     result.HeapStats.TotalAlloc,
			Mallocs:        result.HeapStats.Mallocs,
			GCCycles:       result.HeapStats.GCCycles,
			GCPauseNs:      result.HeapStats.GCPauseNs,
		},
		DiagnosticSummaries: maps.Clone(result.DiagnosticSummaries),
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create benchmark export dir: %w", err)
	}

	path := filepath.Join(dir, benchNameReplacer.Replace(benchmark)+".json")
	src, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal benchmark summary: %w", err)
	}
	src = append(src, '\n')
	if err := os.WriteFile(path, src, 0o644); err != nil {
		return fmt.Errorf("write benchmark summary %s: %w", path, err)
	}
	return nil
}

func timingNS(dur time.Duration, ok bool) (int64, bool) {
	if !ok {
		return 0, false
	}
	return dur.Nanoseconds(), true
}

func exportDiceStats(result Result) []ExportDiceQuery {
	if len(result.DiceStats) == 0 {
		return nil
	}
	out := make([]ExportDiceQuery, len(result.DiceStats))
	for i, qs := range result.DiceStats {
		out[i] = ExportDiceQuery{
			Name:      qs.Name,
			Entries:   qs.Entries,
			Gets:      qs.Gets(),
			FastPath:  qs.FastPath,
			Computed:  qs.Computed,
			Coalesced: qs.Coalesced,
		}
	}
	return out
}

// -----------------------------------------------------------------------------
// Sanity tests — exercise the runner end-to-end on tiny shapes so failures
// surface as plain test errors rather than only as benchmark regressions.
// -----------------------------------------------------------------------------

func TestGenerateAndRunTinyScenario(t *testing.T) {
	topology := TinyTopology()
	scenario := Scenario{
		Name:      "build",
		Selectors: []string{"output.*"},
	}

	rootDir := t.TempDir()
	if err := Generate(rootDir, topology); err != nil {
		t.Fatalf("generate: %s", err)
	}

	result, err := NewRunner(Options{}).Run(t.Context(), rootDir, scenario)
	if err != nil {
		t.Fatalf("run: %s", err)
	}

	if result.ErrorDiagnostics > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
	submitted, executed, _, failed := ActionCounts(result.Spans)
	if submitted == 0 || executed == 0 || failed != 0 {
		t.Fatalf("expected successful actions: submitted=%d executed=%d failed=%d summaries=%v errors=%v", submitted, executed, failed, result.DiagnosticSummaries, result.Errors)
	}
}

func TestInspectorTinyScenario(t *testing.T) {
	topology := TinyTopology()
	scenario := Scenario{
		Name:        "query",
		Selectors:   []string{"**"},
		InspectOnly: true,
	}

	rootDir := t.TempDir()
	if err := Generate(rootDir, topology); err != nil {
		t.Fatalf("generate: %s", err)
	}

	result, err := NewRunner(Options{}).Run(t.Context(), rootDir, scenario)
	if err != nil {
		t.Fatalf("run: %s", err)
	}

	if _, ok := FindSpan(result.Spans, "build.NewInspector"); !ok {
		t.Fatalf("expected build.NewInspector span: spans=%d", len(result.Spans))
	}
	if result.ErrorDiagnostics > 0 {
		t.Fatalf("unexpected errors: %v", result.Errors)
	}
}

func TestGenerateUsesUniqueLeafLockfiles(t *testing.T) {
	topology := Topology{
		Images:          2,
		RepeatKeys:      4,
		RepeatKeyJitter: 1,
	}

	rootDir := t.TempDir()
	if err := Generate(rootDir, topology); err != nil {
		t.Fatalf("generate: %s", err)
	}

	first, err := os.ReadFile(filepath.Join(rootDir, "images", "image_0000", "locks.json"))
	if err != nil {
		t.Fatalf("read first lockfile: %s", err)
	}
	second, err := os.ReadFile(filepath.Join(rootDir, "images", "image_0001", "locks.json"))
	if err != nil {
		t.Fatalf("read second lockfile: %s", err)
	}
	if string(first) == string(second) {
		t.Fatal("expected unique lockfile contents per leaf module")
	}
}

func TestRunHandlesDeferredModuleExpansion(t *testing.T) {
	rootDir := t.TempDir()
	writeBenchTestFile(t, filepath.Join(rootDir, "providers.tf"), `
terraform {
  required_providers {
    test = {
      source = "terraform.io/builtin/test"
    }
  }
}

provider "test" {}
`)
	writeBenchTestFile(t, filepath.Join(rootDir, "main.tf"), `
resource "test_resource" "base" {
  value = "x"
}

module "child" {
  source   = "./child"
  for_each = test_resource.base.id != "" ? { only = "x" } : {}
  value    = each.value
}
`)
	writeBenchTestFile(t, filepath.Join(rootDir, "child", "main.tf"), `
terraform {
  required_providers {
    test = {
      source = "terraform.io/builtin/test"
    }
  }
}

variable "value" {
  type = string
}

resource "test_resource" "build" {
  value = var.value
}

output "value" {
  value = test_resource.build.value
}
`)

	modulesDir := filepath.Join(rootDir, ".terraform", "modules")
	if err := os.MkdirAll(modulesDir, 0o755); err != nil {
		t.Fatalf("create modules dir: %s", err)
	}
	manifest := modsdir.Manifest{
		"": {
			Key: "",
			Dir: rootDir,
		},
		"child": {
			Key:        "child",
			SourceAddr: "./child",
			Dir:        filepath.Join(rootDir, "child"),
		},
	}
	if err := manifest.WriteSnapshotToDir(modulesDir); err != nil {
		t.Fatalf("write modules manifest: %s", err)
	}

	result, err := NewRunner(Options{}).Run(t.Context(), rootDir, Scenario{
		Name:      "deferred-module",
		Selectors: []string{"module.child.**"},
	})
	if err != nil {
		t.Fatalf("run: %s", err)
	}

	if result.Diagnostics == 0 {
		t.Fatalf("expected diagnostics for deferred module with unsupported targets: diags=%d", result.Diagnostics)
	}
}

func TestRunFailsOnValidationErrors(t *testing.T) {
	topology := Topology{
		Images:             1,
		RepeatKeys:         1,
		RootSummaryOutputs: true,
	}
	rootDir := t.TempDir()
	if err := Generate(rootDir, topology); err != nil {
		t.Fatalf("generate: %s", err)
	}
	writeBenchTestFile(t, filepath.Join(rootDir, "invalid.tf"), `
resource "test_resource" "base" {
  value = "x"
}

check "unsupported" {
  assert {
    condition     = test_resource.base.id != ""
    error_message = "never"
  }
}
`)

	result, err := NewRunner(Options{}).Run(t.Context(), rootDir, Scenario{
		Name:      "invalid",
		Selectors: []string{"**"},
	})
	if err == nil {
		t.Fatalf("expected validation failure: diags=%d summaries=%v", result.Diagnostics, result.DiagnosticSummaries)
	}
	if got := result.DiagnosticSummaries["Unsupported syntax in build mode"]; got == 0 {
		t.Fatalf("expected validation diagnostics: diags=%d summaries=%v", result.Diagnostics, result.DiagnosticSummaries)
	}
}

func TestWriteExport(t *testing.T) {
	dir := t.TempDir()

	topology := Topology{
		Images:                     1,
		RepeatKeys:                 1,
		SupportedResourcesPerBuild: 1,
		QuerySafeCalls:             1,
		RootSummaryOutputs:         true,
	}
	rootDir := t.TempDir()
	if err := Generate(rootDir, topology); err != nil {
		t.Fatalf("generate: %s", err)
	}

	result, err := NewRunner(Options{}).Run(t.Context(), rootDir, Scenario{
		Name:      "build",
		Selectors: []string{"**"},
	})
	if err != nil {
		t.Fatalf("run: %s", err)
	}

	if err := WriteExport(dir, "BenchmarkBuild/tiny", "tiny", topology, result); err != nil {
		t.Fatalf("write export: %s", err)
	}

	path := filepath.Join(dir, "BenchmarkBuild__tiny.json")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read export: %s", err)
	}

	var got Export
	if err := json.Unmarshal(src, &got); err != nil {
		t.Fatalf("unmarshal export: %s", err)
	}

	if got.Version != exportVersion {
		t.Fatalf("wrong version: %s", got.Version)
	}
	if got.Timings.TotalNS == 0 {
		t.Fatal("expected non-zero total timing")
	}
	if got.Timings.NewEngineNS == 0 {
		t.Fatal("expected non-zero NewEngine timing")
	}
	if got.Timings.ExecuteNS == 0 {
		t.Fatal("expected non-zero Execute timing")
	}
	if got.Counts.SelectedTargets+got.Counts.SelectedOutputs == 0 {
		t.Fatal("expected non-zero selections")
	}
}

func writeBenchTestFile(t *testing.T, path string, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create dir: %s", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file: %s", err)
	}
}

// -----------------------------------------------------------------------------
// Benchmarks
// -----------------------------------------------------------------------------
//
// BenchmarkBuild exercises the full execute path at four sizes sharing a
// single topology shape:
//   - tiny:   ~10 leaves, smoke tier (also covered by unit tests).
//   - fast:   ~200 leaves, iteration tier for perf work.
//   - medium: ~500 leaves, interstitial tier for pinning the scaling exponent.
//   - real:   ~1920 leaves, ground truth calibrated to images-private.
//
// BenchmarkQuery runs the same sizes through the inspector-only path.
//
// To profile a single size:
//
//	go test ./internal/build/bench \
//	  -run '^$' \
//	  -bench 'BenchmarkBuild/fast' \
//	  -benchtime=1x \
//	  -benchmem \
//	  -cpuprofile cpu.out \
//	  -memprofile mem.out \
//	  -trace trace.out
//
// Set TOFU_BUILD_BENCH_EXPORT_DIR to emit one JSON summary per sub-benchmark.

var benchSizes = []struct {
	label    string
	topology Topology
}{
	{"tiny", TinyTopology()},
	{"fast", FastTopology()},
	{"medium", MediumTopology()},
	{"real", RealTopology()},
}

func BenchmarkBuild(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.label, func(b *testing.B) {
			rootDir := b.TempDir()
			if err := Generate(rootDir, size.topology); err != nil {
				b.Fatalf("generate: %s", err)
			}
			scenario := Scenario{Name: "build", Selectors: []string{"output.*"}}
			runner := NewRunner(Options{})

			var result Result
			b.ResetTimer()
			for b.Loop() {
				labels := pprof.Labels("size", size.label, "scenario", "build")
				pprof.Do(b.Context(), labels, func(ctx context.Context) {
					trace.WithRegion(ctx, "bench.build/"+size.label, func() {
						var err error
						result, err = runner.Run(ctx, rootDir, scenario)
						if err != nil {
							b.Fatalf("run: %s", err)
						}
					})
				})
			}

			submitted, executed, cached, _ := ActionCounts(result.Spans)
			b.ReportMetric(float64(SpanDuration(result.Spans, "build.Load").Nanoseconds()), "ns/load")
			b.ReportMetric(float64(SpanDuration(result.Spans, "build.NewEngine").Nanoseconds()), "ns/new_engine")
			b.ReportMetric(float64(SpanDuration(result.Spans, "build.Execute").Nanoseconds()), "ns/execute")
			b.ReportMetric(float64(SpanAttrInt(result.Spans, "build.Execute", "build.selected_targets")), "selected_targets")
			b.ReportMetric(float64(SpanAttrInt(result.Spans, "build.Execute", "build.selected_outputs")), "selected_outputs")
			b.ReportMetric(float64(submitted), "submitted")
			b.ReportMetric(float64(executed), "executed")
			b.ReportMetric(float64(cached), "cached")
			b.ReportMetric(float64(InvalidationRounds(result.Spans)), "invalidation_rounds")
			b.ReportMetric(float64(result.Diagnostics), "diagnostics")

			if dur, ok := FirstEventTime(result.Spans, "build.submit", result.Start); ok {
				b.ReportMetric(float64(dur.Nanoseconds()), "ns/first_submit")
			}
			if dur, ok := FirstProviderEventTime(result.Spans, "build.submit", result.Start); ok {
				b.ReportMetric(float64(dur.Nanoseconds()), "ns/first_provider_submit")
			}
			if dur, ok := FirstSpanStartTime(result.Spans, "dispatch.execute", result.Start); ok {
				b.ReportMetric(float64(dur.Nanoseconds()), "ns/first_dispatch")
			}
			if dur, ok := FirstProviderSpanStartTime(result.Spans, "dispatch.execute", result.Start); ok {
				b.ReportMetric(float64(dur.Nanoseconds()), "ns/first_provider_dispatch")
			}
			if dur, ok := FirstSpanStartTime(result.Spans, "build.Runner.Run", result.Start); ok {
				b.ReportMetric(float64(dur.Nanoseconds()), "ns/first_runner")
			}
			if dur, ok := FirstProviderSpanStartTime(result.Spans, "build.Runner.Run", result.Start); ok {
				b.ReportMetric(float64(dur.Nanoseconds()), "ns/first_provider_runner")
			}

			var diceEntries int
			var diceGets, diceFast, diceComputed, diceCoalesced int64
			for _, qs := range result.DiceStats {
				diceEntries += qs.Entries
				diceGets += qs.Gets()
				diceFast += qs.FastPath
				diceComputed += qs.Computed
				diceCoalesced += qs.Coalesced
			}
			b.ReportMetric(float64(diceEntries), "dice_entries")
			b.ReportMetric(float64(diceGets), "dice_gets")
			b.ReportMetric(float64(diceFast), "dice_fast_path")
			b.ReportMetric(float64(diceComputed), "dice_computed")
			b.ReportMetric(float64(diceCoalesced), "dice_coalesced")

			b.ReportMetric(float64(result.HeapStats.HeapAllocBytes), "heap_alloc_bytes")
			b.ReportMetric(float64(result.HeapStats.HeapObjects), "heap_objects")
			b.ReportMetric(float64(result.HeapStats.TotalAlloc), "total_alloc_bytes")
			b.ReportMetric(float64(result.HeapStats.Mallocs), "mallocs")
			b.ReportMetric(float64(result.HeapStats.GCCycles), "gc_cycles")
			b.ReportMetric(float64(result.HeapStats.GCPauseNs), "ns/gc_pause")

			if err := WriteExport(ExportDirFromEnv(), b.Name(), size.label, size.topology, result); err != nil {
				b.Fatalf("write export: %s", err)
			}
		})
	}
}

func BenchmarkQuery(b *testing.B) {
	for _, size := range benchSizes {
		b.Run(size.label, func(b *testing.B) {
			rootDir := b.TempDir()
			if err := Generate(rootDir, size.topology); err != nil {
				b.Fatalf("generate: %s", err)
			}
			scenario := Scenario{Name: "query", Selectors: []string{"**"}, InspectOnly: true}
			runner := NewRunner(Options{})

			var result Result
			b.ResetTimer()
			for b.Loop() {
				labels := pprof.Labels("size", size.label, "scenario", "query")
				pprof.Do(b.Context(), labels, func(ctx context.Context) {
					trace.WithRegion(ctx, "bench.query/"+size.label, func() {
						var err error
						result, err = runner.Run(ctx, rootDir, scenario)
						if err != nil {
							b.Fatalf("run: %s", err)
						}
					})
				})
			}

			b.ReportMetric(float64(SpanDuration(result.Spans, "build.Load").Nanoseconds()), "ns/load")
			b.ReportMetric(float64(SpanDuration(result.Spans, "build.NewInspector").Nanoseconds()), "ns/new_inspector")
			b.ReportMetric(float64(result.Diagnostics), "diagnostics")

			var diceEntries int
			var diceGets, diceFast, diceComputed, diceCoalesced int64
			for _, qs := range result.DiceStats {
				diceEntries += qs.Entries
				diceGets += qs.Gets()
				diceFast += qs.FastPath
				diceComputed += qs.Computed
				diceCoalesced += qs.Coalesced
			}
			b.ReportMetric(float64(diceEntries), "dice_entries")
			b.ReportMetric(float64(diceGets), "dice_gets")
			b.ReportMetric(float64(diceFast), "dice_fast_path")
			b.ReportMetric(float64(diceComputed), "dice_computed")
			b.ReportMetric(float64(diceCoalesced), "dice_coalesced")

			b.ReportMetric(float64(result.HeapStats.HeapAllocBytes), "heap_alloc_bytes")
			b.ReportMetric(float64(result.HeapStats.HeapObjects), "heap_objects")
			b.ReportMetric(float64(result.HeapStats.TotalAlloc), "total_alloc_bytes")
			b.ReportMetric(float64(result.HeapStats.Mallocs), "mallocs")
			b.ReportMetric(float64(result.HeapStats.GCCycles), "gc_cycles")
			b.ReportMetric(float64(result.HeapStats.GCPauseNs), "ns/gc_pause")

			if err := WriteExport(ExportDirFromEnv(), b.Name(), size.label, size.topology, result); err != nil {
				b.Fatalf("write export: %s", err)
			}
		})
	}
}
