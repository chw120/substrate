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

package main

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"

	"go.ate.dev/substrate/internal/ateattr"
	"go.ate.dev/substrate/internal/proto/ateletpb"
)

const (
	restoreDurationMetric    = "ate.actor.restore.duration"
	checkpointDurationMetric = "ate.actor.checkpoint.duration"
	snapshotUploadSizeMetric = "atelet.snapshot.upload.size"
)

// snapshotPhaseBuckets have to cover both ends of a phase breakdown: a warm OCI
// unpack or a local rename lands in single-digit milliseconds, while a cold node
// fetching a multi-GiB snapshot runs for tens of seconds.
var snapshotPhaseBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 30, 60}

// snapshotSizeBuckets have to cover both ends of a snapshot's images: a state
// file of a few KB lands in the first bucket, while a guest memory image runs to
// several GB.
var snapshotSizeBuckets = []float64{1e6, 5e6, 1e7, 2.5e7, 5e7, 1e8, 2.5e8, 5e8, 1e9, 2e9, 5e9, 1e10}

// Instruments holds atelet's snapshot histograms. A nil *Instruments is a
// valid no-op, so call sites need no guard.
type Instruments struct {
	restoreDuration    metric.Float64Histogram
	checkpointDuration metric.Float64Histogram
	snapshotUploadSize metric.Int64Histogram
}

func NewInstruments(meter metric.Meter) (*Instruments, error) {
	restoreDuration, err := meter.Float64Histogram(
		restoreDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one phase of an actor restore on atelet. Phases overlap, so they are independent observations rather than a partition of the total phase."),
		metric.WithExplicitBucketBoundaries(snapshotPhaseBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", restoreDurationMetric, err)
	}

	checkpointDuration, err := meter.Float64Histogram(
		checkpointDurationMetric,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of one phase of an actor checkpoint on atelet. Phases overlap, so they are independent observations rather than a partition of the total phase."),
		metric.WithExplicitBucketBoundaries(snapshotPhaseBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", checkpointDurationMetric, err)
	}

	snapshotUploadSize, err := meter.Int64Histogram(
		snapshotUploadSizeMetric,
		metric.WithUnit("By"),
		metric.WithDescription("Bytes object storage holds for each file that a snapshot upload writes, as the object-store plugin reports them."),
		metric.WithExplicitBucketBoundaries(snapshotSizeBuckets...),
	)
	if err != nil {
		return nil, fmt.Errorf("create %s histogram: %w", snapshotUploadSizeMetric, err)
	}

	return &Instruments{
		restoreDuration:    restoreDuration,
		checkpointDuration: checkpointDuration,
		snapshotUploadSize: snapshotUploadSize,
	}, nil
}

// snapshotOp is the dimension set shared by every phase of one restore or
// checkpoint.
type snapshotOp struct {
	templateNamespace string
	templateName      string
	kind              string
	fidelity          string
	sandboxClass      string
}

// attrs omits kind and sandbox class while they are unknown (a restore that
// failed before reading the snapshot manifest) rather than emitting an
// empty-string series.
func (o snapshotOp) attrs() []attribute.KeyValue {
	attrs := make([]attribute.KeyValue, 0, 5)
	attrs = append(attrs,
		ateattr.TemplateAtespaceKey.String(o.templateNamespace),
		ateattr.TemplateNameKey.String(o.templateName),
		ateattr.SnapshotFidelityKey.String(o.fidelity),
	)
	if o.kind != "" {
		attrs = append(attrs, ateattr.SnapshotKindKey.String(o.kind))
	}
	if o.sandboxClass != "" {
		attrs = append(attrs, ateattr.SandboxClassAttribute(o.sandboxClass))
	}
	return attrs
}

// phase is one timed step of a snapshot operation.
type phase struct {
	name string
	d    time.Duration
}

func (i *Instruments) recordRestore(ctx context.Context, op snapshotOp, phases ...phase) {
	if i == nil || i.restoreDuration == nil {
		return
	}
	recordPhases(ctx, i.restoreDuration, op, phases)
}

func (i *Instruments) recordCheckpoint(ctx context.Context, op snapshotOp, phases ...phase) {
	if i == nil || i.checkpointDuration == nil {
		return
	}
	recordPhases(ctx, i.checkpointDuration, op, phases)
}

// recordSnapshotUploadSize records the bytes object storage holds for one
// uploaded snapshot file, labeled like atelet.snapshot.size.
func (i *Instruments) recordSnapshotUploadSize(ctx context.Context, file string, size int64, templateAtespace, templateName string) {
	if i == nil || i.snapshotUploadSize == nil {
		return
	}
	i.snapshotUploadSize.Record(ctx, size, metric.WithAttributes(
		semconv.FileNameKey.String(file),
		ateattr.TemplateAtespaceKey.String(templateAtespace),
		ateattr.TemplateNameKey.String(templateName),
	))
}

// recordPhases skips zero-valued phases: those never started, because the
// operation died before reaching them, and reporting them as instantaneous
// would drag every percentile down.
func recordPhases(ctx context.Context, h metric.Float64Histogram, op snapshotOp, phases []phase) {
	base := op.attrs()
	for _, p := range phases {
		if p.d == 0 {
			continue
		}
		attrs := make([]attribute.KeyValue, 0, len(base)+1)
		attrs = append(attrs, base...)
		attrs = append(attrs, ateattr.SnapshotPhaseKey.String(p.name))
		h.Record(ctx, p.d.Seconds(), metric.WithAttributes(attrs...))
	}
}

// isCollateral reports whether legErr is only fallout from the other leg
// canceling the shared context. That leg stopped part way, so its duration
// would read as an unusually fast success.
func isCollateral(groupErr, legErr error) bool {
	return legErr != nil && groupErr != legErr
}

// assetsAfterCollateral keeps the sandbox-asset duration when the prep leg was
// cancelled during the later OCI unpack: only the phase a leg stopped in is
// truncated, and dropping a completed one reports a step that ran as never run.
func assetsAfterCollateral(prepFailedPhase string, assets time.Duration) time.Duration {
	if prepFailedPhase == ateattr.SnapshotPhaseSandboxAssets {
		return 0
	}
	return assets
}

// restoreSnapshotKind classifies which snapshot a restore reads: local when it
// is served from the node-local copy, and otherwise latest. An empty result
// means the manifest has not been read yet, so the
// kind is not knowable.
func restoreSnapshotKind(directLocal bool, rec *sandboxAssetsRecord) string {
	if directLocal {
		return ateattr.SnapshotKindLocal
	}
	if rec == nil {
		return ""
	}
	return ateattr.SnapshotKindLatest
}

// checkpointSnapshotKind classifies which snapshot a checkpoint writes: a
// deferred-upload checkpoint (async suspend) writes the node-local one, and an
// immediate-upload checkpoint (sync suspend) writes a durable latest snapshot.
func checkpointSnapshotKind(req *ateletpb.CheckpointRequest) string {
	if req.GetUploadConfig().GetMode() == ateletpb.UploadMode_UPLOAD_MODE_DEFERRED {
		return ateattr.SnapshotKindLocal
	}
	return ateattr.SnapshotKindLatest
}
