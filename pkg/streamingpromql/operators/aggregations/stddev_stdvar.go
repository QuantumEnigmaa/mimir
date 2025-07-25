// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/prometheus/prometheus/blob/main/promql/engine.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Prometheus Authors

package aggregations

import (
	"math"

	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser/posrange"
	"github.com/prometheus/prometheus/util/annotations"

	"github.com/grafana/mimir/pkg/streamingpromql/types"
	"github.com/grafana/mimir/pkg/util/limiter"
)

// stddev represents whether this aggregation is `stddev` (true), or `stdvar` (false)
func NewStddevStdvarAggregationGroup(stddev bool) *StddevStdvarAggregationGroup {
	return &StddevStdvarAggregationGroup{stddev: stddev}
}

type StddevStdvarAggregationGroup struct {
	floats     []float64
	floatMeans []float64

	// stddev represents whether this aggregation is `stddev` (true), or `stdvar` (false)
	stddev bool

	// Keeps track of how many samples we have encountered thus far for the group at this point
	// This is necessary to do per point (instead of just counting the input series) as a series may have
	// stale or non-existent values that are not added towards the count.
	// We use float64 instead of uint64 so that we can reuse the float pool and don't have to retype on each division.
	groupSeriesCounts []float64
}

func (g *StddevStdvarAggregationGroup) AccumulateSeries(data types.InstantVectorSeriesData, timeRange types.QueryTimeRange, memoryConsumptionTracker *limiter.MemoryConsumptionTracker, emitAnnotation types.EmitAnnotationFunc, _ uint) error {
	// Native histograms are ignored for stddev and stdvar.
	if len(data.Histograms) > 0 {
		emitAnnotation(func(_ string, expressionPosition posrange.PositionRange) error {
			name := "stdvar"

			if g.stddev {
				name = "stddev"
			}

			return annotations.NewHistogramIgnoredInAggregationInfo(name, expressionPosition)
		})
	}

	if len(data.Floats) > 0 && g.floats == nil {
		// First series with float values for this group, populate it.
		var err error
		g.floats, err = types.Float64SlicePool.Get(timeRange.StepCount, memoryConsumptionTracker)
		if err != nil {
			return err
		}
		g.floatMeans, err = types.Float64SlicePool.Get(timeRange.StepCount, memoryConsumptionTracker)
		if err != nil {
			return err
		}
		g.groupSeriesCounts, err = types.Float64SlicePool.Get(timeRange.StepCount, memoryConsumptionTracker)
		if err != nil {
			return err
		}

		g.floats = g.floats[:timeRange.StepCount]
		g.floatMeans = g.floatMeans[:timeRange.StepCount]
		g.groupSeriesCounts = g.groupSeriesCounts[:timeRange.StepCount]
	}

	for _, p := range data.Floats {
		idx := timeRange.PointIndex(p.T)

		g.groupSeriesCounts[idx]++
		delta := p.F - g.floatMeans[idx]
		g.floatMeans[idx] += delta / g.groupSeriesCounts[idx]
		g.floats[idx] += delta * (p.F - g.floatMeans[idx])
	}

	types.PutInstantVectorSeriesData(data, memoryConsumptionTracker)
	return nil
}

func (g *StddevStdvarAggregationGroup) ComputeOutputSeries(_ types.ScalarData, timeRange types.QueryTimeRange, memoryConsumptionTracker *limiter.MemoryConsumptionTracker) (types.InstantVectorSeriesData, bool, error) {
	floatPointCount := 0
	for _, sc := range g.groupSeriesCounts {
		if sc > 0 {
			floatPointCount++
		}
	}
	var floatPoints []promql.FPoint
	var err error

	if floatPointCount > 0 {
		floatPoints, err = types.FPointSlicePool.Get(floatPointCount, memoryConsumptionTracker)
		if err != nil {
			return types.InstantVectorSeriesData{}, false, err
		}

		for i, sc := range g.groupSeriesCounts {
			if sc > 0 {
				t := timeRange.StartT + int64(i)*timeRange.IntervalMilliseconds
				var f float64
				if g.stddev {
					// stddev
					f = math.Sqrt(g.floats[i] / g.groupSeriesCounts[i])
				} else {
					// stdvar
					f = g.floats[i] / g.groupSeriesCounts[i]
				}
				floatPoints = append(floatPoints, promql.FPoint{T: t, F: f})
			}
		}
	}

	return types.InstantVectorSeriesData{Floats: floatPoints}, false, nil
}

func (g *StddevStdvarAggregationGroup) Close(memoryConsumptionTracker *limiter.MemoryConsumptionTracker) {
	types.Float64SlicePool.Put(&g.floats, memoryConsumptionTracker)
	types.Float64SlicePool.Put(&g.floatMeans, memoryConsumptionTracker)
	types.Float64SlicePool.Put(&g.groupSeriesCounts, memoryConsumptionTracker)
}
