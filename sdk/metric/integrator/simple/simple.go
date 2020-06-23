// Copyright The OpenTelemetry Authors
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

package simple // import "go.opentelemetry.io/otel/sdk/metric/integrator/simple"

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel/api/label"
	"go.opentelemetry.io/otel/api/metric"
	export "go.opentelemetry.io/otel/sdk/export/metric"
	"go.opentelemetry.io/otel/sdk/export/metric/aggregation"
	"go.opentelemetry.io/otel/sdk/resource"
)

type (
	Integrator struct {
		export.ExportKindSelector
		export.AggregationSelector

		state
	}

	stateKey struct {
		descriptor *metric.Descriptor
		distinct   label.Distinct
		resource   label.Distinct
	}

	stateValue struct {
		// labels corresponds to the stateKey.distinct field.
		labels *label.Set

		// resource corresponds to the stateKey.resource field.
		resource *resource.Resource

		// updated indicates the last sequence number when this value had
		// Process() called by an accumulator.
		updated int64

		// stateful indicates that a cumulative aggregation is
		// being maintained, taken from the process start time.
		stateful bool

		current    export.Aggregator // refers to single-accumulator checkpoint or delta.
		delta      export.Aggregator // owned if multi accumulator else nil.
		cumulative export.Aggregator // owned if stateful else nil.
	}

	state struct {
		// RWMutex implements locking for the `CheckpointSet` interface.
		sync.RWMutex
		values map[stateKey]*stateValue

		// Note: the timestamp logic currently assumes all
		// exports are deltas.

		processStart  time.Time
		intervalStart time.Time
		intervalEnd   time.Time

		// startedCollection and finishedCollection are the
		// number of StartCollection() and FinishCollection()
		// calls, used to ensure that the sequence of starts
		// and finishes are correctly balanced.

		startedCollection  int64
		finishedCollection int64
	}
)

var _ export.Integrator = &Integrator{}
var _ export.CheckpointSet = &state{}
var ErrInconsistentState = fmt.Errorf("inconsistent integrator state")
var ErrInvalidExporterKind = fmt.Errorf("invalid exporter kind")

// New returns a basic Integrator using the provided
// AggregationSelector to select Aggregators.  The ExportKindSelector
// is consulted to determine the kind(s) of exporter that will consume
// data, so that this Integrator can prepare to compute Delta or
// Cumulative Aggregations as needed.
func New(aselector export.AggregationSelector, eselector export.ExportKindSelector) *Integrator {
	now := time.Now()
	return &Integrator{
		AggregationSelector: aselector,
		ExportKindSelector:  eselector,
		state: state{
			values:        map[stateKey]*stateValue{},
			processStart:  now,
			intervalStart: now,
		},
	}
}

// Process implements export.Integrator.
func (b *Integrator) Process(accum export.Accumulation) error {
	if b.startedCollection != b.finishedCollection+1 {
		return ErrInconsistentState
	}
	desc := accum.Descriptor()
	key := stateKey{
		descriptor: desc,
		distinct:   accum.Labels().Equivalent(),
		resource:   accum.Resource().Equivalent(),
	}
	agg := accum.Aggregator()

	// Check if there is an existing value.
	value, ok := b.state.values[key]
	if !ok {
		stateful := b.ExportKindFor(desc, agg.Aggregation().Kind()).MemoryRequired(desc.MetricKind())

		newValue := &stateValue{
			labels:   accum.Labels(),
			resource: accum.Resource(),
			updated:  b.state.finishedCollection,
			stateful: stateful,
			current:  agg,
		}
		if stateful {
			if desc.MetricKind().PrecomputedSum() {
				// If we know we need to compute deltas, allocate two aggregators.
				b.AggregatorFor(desc, &newValue.cumulative, &newValue.delta)
			} else {
				// In this case we are not certain to need a delta, only allocate a
				// cumulative aggregator.  We _may_ need a delta accumulator if
				// multiple synchronous Accumulators produce an Accumulation (handled
				// below), which requires merging them into a temporary Aggregator.
				b.AggregatorFor(desc, &newValue.cumulative)
			}
		}
		b.state.values[key] = newValue
		return nil
	}

	// Advance the update sequence number.
	sameCollection := b.state.finishedCollection == value.updated
	value.updated = b.state.finishedCollection

	// An existing value will be found for some stateKey when:
	// (a) stateful aggregation is being used
	// (b) multiple accumulators are being used.
	//
	// Case (a) occurs when the instrument and the exporter
	// require memory to work correctly, either because the
	// instrument reports a PrecomputedSum to a DeltaExporter or
	// the reverse, a non-PrecomputedSum instrument with a
	// CumulativeExporter.  This logic is encapsulated in
	// ExportKind.MemoryRequired(MetricKind).
	//
	// Case (b) occurs when the variable `sameCollection` is true,
	// indicating that the stateKey for Accumulation has already
	// been seen in the same collection.  When this happens, it
	// implies that multiple Accumulators are being used because
	// the Accumulator outputs a maximum of one Accumulation per
	// instrument and label set.
	//
	// The following logic distinguishes between asynchronous and
	// synchronous instruments in order to ensure that the use of
	// multiple Accumulators does not change instrument semantics.
	// To maintain the instrument semantics, multiple synchronous
	// Accumulations should be merged, whereas when multiple
	// asynchronous Accumulations are processed, the last value
	// should be kept.

	if !sameCollection {
		// This is the first Accumulation we've seen for this
		// stateKey during this collection.  Just keep a
		// reference to the Accumulator's Aggregator.
		value.current = agg
		return nil
	}
	if desc.MetricKind().Asynchronous() {
		// The last value across multiple accumulators is taken.
		// Just keep a reference to the Accumulator's Aggregator.
		value.current = agg
		return nil
	}

	// The above two cases are keeping a reference to the
	// Accumulator's Aggregator.  The remaining cases address
	// synchronous instruments, which always merge multiple
	// Accumulations using `value.delta` for temporary storage.

	if value.delta == nil {
		// The temporary `value.delta` may have been allocated
		// already, either in a prior pass through this block of
		// code or in the `!ok` branch above.  It would be
		// allocated in the `!ok` branch if this is stateful
		// PrecomputedSum instrument (in which case the exporter
		// is requesting a delta so we allocate it up front),
		// and it would be allocated in this block when multiple
		// accumulators are used and the first condition is not
		// met.
		b.AggregationSelector.AggregatorFor(desc, &value.delta)
	}
	if value.current != value.delta {
		// If the current and delta Aggregators are not the same it
		// implies that multiple Accumulators were used.  The first
		// Accumulation seen for a given stateKey will return in
		// one of the cases above after assigning `value.current
		// = agg` (i.e., after taking a reference to the
		// Accumulator's Aggregator).
		//
		// The second time through this branch copies the
		// Accumulator's Aggregator into `value.delta` and sets
		// `value.current` appropriately to avoid this branch if
		// a third Accumulator is used.
		err := value.current.SynchronizedCopy(value.delta, desc)
		if err != nil {
			return err
		}
		value.current = value.delta
	}
	// The two statements above ensures that `value.current` refers
	// to `value.delta` and not to an Accumulator's Aggregator.  Now
	// combine this Accumulation with the prior Accumulation.
	return value.delta.Merge(agg, desc)
}

// CheckpointSet returns the associated CheckpointSet.  Use the
// CheckpointSet Locker interface to synchronize access to this
// object.  The CheckpointSet.ForEach() method cannot be called
// concurrently with Process().
func (b *Integrator) CheckpointSet() export.CheckpointSet {
	return &b.state
}

// StartCollection signals to the Integrator one or more Accumulators
// will begin calling Process() calls during collection.
func (b *Integrator) StartCollection() {
	if b.startedCollection != 0 {
		b.intervalStart = b.intervalEnd
	}
	b.startedCollection++
}

// FinishCollection signals to the Integrator that a complete
// collection has finished and that ForEach will be called to access
// the CheckpointSet.
func (b *Integrator) FinishCollection() error {
	b.intervalEnd = time.Now()
	if b.startedCollection != b.finishedCollection+1 {
		return ErrInconsistentState
	}
	defer func() { b.finishedCollection++ }()

	for key, value := range b.values {
		mkind := key.descriptor.MetricKind()

		if !value.stateful {
			if value.updated != b.finishedCollection {
				delete(b.values, key)
			}
			continue
		}

		// Update Aggregator state to support exporting either a
		// delta or a cumulative aggregation.
		var err error
		if mkind.PrecomputedSum() {
			// delta_value = current_cumulative_value - previous_cumulative_value
			if subt, ok := value.current.(export.Subtractor); ok {
				err = subt.Subtract(value.cumulative, value.delta, key.descriptor)

				if err == nil {
					err = value.current.SynchronizedCopy(value.cumulative, key.descriptor)
				}
			} else {
				err = aggregation.ErrNoSubtraction
			}
		} else {
			// cumulative_value = previous_cumulative_value + current_delta_value
			err = value.cumulative.Merge(value.current, key.descriptor)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ForEach iterates through the CheckpointSet, passing an
// export.Record with the appropriate Cumulative or Delta aggregation
// to an exporter.
func (b *state) ForEach(exporter export.ExportKindSelector, f func(export.Record) error) error {
	if b.startedCollection != b.finishedCollection {
		return ErrInconsistentState
	}
	for key, value := range b.values {
		mkind := key.descriptor.MetricKind()

		var agg aggregation.Aggregation
		var start time.Time

		ekind := exporter.ExportKindFor(key.descriptor, value.current.Aggregation().Kind())
		switch ekind {
		case export.PassThroughExporter:
			// No state is required, pass through the checkpointed value.
			agg = value.current.Aggregation()

			if mkind.PrecomputedSum() {
				start = b.processStart
			} else {
				start = b.intervalStart
			}

		case export.CumulativeExporter:
			// If stateful, the sum has been computed.  If stateless, the
			// input was already cumulative.  Either way, use the checkpointed
			// value:
			if value.stateful {
				agg = value.cumulative.Aggregation()
			} else {
				agg = value.current.Aggregation()
			}
			start = b.processStart

		case export.DeltaExporter:
			// Precomputed sums are a special case.
			if mkind.PrecomputedSum() {
				agg = value.delta.Aggregation()
			} else {
				agg = value.current.Aggregation()
			}
			start = b.intervalStart

		default:
			return fmt.Errorf("%v: %w", ekind, ErrInvalidExporterKind)
		}

		if err := f(export.NewRecord(
			key.descriptor,
			value.labels,
			value.resource,
			agg,
			start,
			b.intervalEnd,
		)); err != nil && !errors.Is(err, aggregation.ErrNoData) {
			return err
		}
	}
	return nil
}
