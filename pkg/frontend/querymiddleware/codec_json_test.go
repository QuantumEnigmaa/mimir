// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/queryrange/marshaling_test.go
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/querier/queryrange/query_range_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package querymiddleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/go-kit/log"
	dskit_metrics "github.com/grafana/dskit/metrics"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/stretchr/testify/require"

	apierror "github.com/grafana/mimir/pkg/api/error"
	"github.com/grafana/mimir/pkg/mimirpb"
)

func TestCodec_JSONResponse_Metrics(t *testing.T) {
	headers := http.Header{"Content-Type": []string{"application/json"}}
	expectedRespHeaders := []*PrometheusHeader{
		{
			Name:   "Content-Type",
			Values: []string{"application/json"},
		},
	}

	for _, tc := range []struct {
		name            string
		responseHeaders http.Header
		resp            prometheusAPIResponse
		expected        *PrometheusResponse
		expectedErr     error
	}{
		{
			name: "successful string response",
			resp: prometheusAPIResponse{
				Status: statusSuccess,
				Data: prometheusResponseData{
					Type:   model.ValString,
					Result: &model.String{Value: "foo", Timestamp: 1_500},
				},
			},
			expected: &PrometheusResponse{
				Status: statusSuccess,
				Data: &PrometheusData{
					ResultType: model.ValString.String(),
					Result: []SampleStream{
						{
							Labels:  []mimirpb.LabelAdapter{{Name: "value", Value: "foo"}},
							Samples: []mimirpb.Sample{{TimestampMs: 1_500}},
						},
					},
				},
				Headers: expectedRespHeaders,
			},
		},
		{
			name: "successful scalar response",
			resp: prometheusAPIResponse{
				Status: statusSuccess,
				Data: prometheusResponseData{
					Type: model.ValScalar,
					Result: &model.Scalar{
						Value:     200,
						Timestamp: 1_000,
					},
				},
			},
			expected: &PrometheusResponse{
				Status: statusSuccess,
				Data: &PrometheusData{
					ResultType: model.ValScalar.String(),
					Result: []SampleStream{
						{Samples: []mimirpb.Sample{{TimestampMs: 1_000, Value: 200}}},
					},
				},
				Headers: expectedRespHeaders,
			},
		},
		{
			name: "successful vector response",
			resp: prometheusAPIResponse{
				Status: statusSuccess,
				Data: prometheusResponseData{
					Type: model.ValVector,
					Result: model.Vector{
						{Metric: model.Metric{"foo": "bar"}, Timestamp: 1_000, Value: 200},
						{Metric: model.Metric{"bar": "baz"}, Timestamp: 1_000, Value: 201},
					},
				},
			},
			expected: &PrometheusResponse{
				Status: statusSuccess,
				Data: &PrometheusData{
					ResultType: model.ValVector.String(),
					Result: []SampleStream{
						{Labels: []mimirpb.LabelAdapter{{Name: "foo", Value: "bar"}}, Samples: []mimirpb.Sample{{TimestampMs: 1_000, Value: 200}}},
						{Labels: []mimirpb.LabelAdapter{{Name: "bar", Value: "baz"}}, Samples: []mimirpb.Sample{{TimestampMs: 1_000, Value: 201}}},
					},
				},
				Headers: expectedRespHeaders,
			},
		},
		{
			name: "successful matrix response with float values",
			resp: prometheusAPIResponse{
				Status: statusSuccess,
				Data: prometheusResponseData{
					Type: model.ValMatrix,
					Result: model.Matrix{
						{Metric: model.Metric{"foo": "bar"}, Values: []model.SamplePair{{Timestamp: 1_000, Value: 100}, {Timestamp: 2_000, Value: 200}}},
						{Metric: model.Metric{"bar": "baz"}, Values: []model.SamplePair{{Timestamp: 1_000, Value: 101}, {Timestamp: 2_000, Value: 201}}},
					},
				},
			},
			expected: &PrometheusResponse{
				Status: statusSuccess,
				Data: &PrometheusData{
					ResultType: model.ValMatrix.String(),
					Result: []SampleStream{
						{Labels: []mimirpb.LabelAdapter{{Name: "foo", Value: "bar"}}, Samples: []mimirpb.Sample{{TimestampMs: 1_000, Value: 100}, {TimestampMs: 2_000, Value: 200}}},
						{Labels: []mimirpb.LabelAdapter{{Name: "bar", Value: "baz"}}, Samples: []mimirpb.Sample{{TimestampMs: 1_000, Value: 101}, {TimestampMs: 2_000, Value: 201}}},
					},
				},
				Headers: expectedRespHeaders,
			},
		},
		{
			name: "successful empty matrix response",
			resp: prometheusAPIResponse{
				Status: statusSuccess,
				Data: prometheusResponseData{
					Type:   model.ValMatrix,
					Result: model.Matrix{},
				},
			},
			expected: &PrometheusResponse{
				Status: statusSuccess,
				Data: &PrometheusData{
					ResultType: model.ValMatrix.String(),
					Result:     []SampleStream{},
				},
				Headers: expectedRespHeaders,
			},
		},
		{
			name: "error response",
			resp: prometheusAPIResponse{
				Status:    statusError,
				ErrorType: "expected",
				Error:     "failed",
			},
			expectedErr: apierror.New(apierror.Type("expected"), "failed"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewPedanticRegistry()
			codec := NewCodec(reg, 0*time.Minute, formatJSON, nil)

			body, err := json.Marshal(tc.resp)
			require.NoError(t, err)
			httpResponse := &http.Response{
				StatusCode: 200,
				Header:     headers,
				Body: &prometheusReadCloser{
					Reader:    bytes.NewBuffer(body),
					finalizer: func() {},
				},
				ContentLength: int64(len(body)),
			}
			decoded, err := codec.DecodeMetricsQueryResponse(context.Background(), httpResponse, nil, log.NewNopLogger())
			if err != nil || tc.expectedErr != nil {
				require.Equal(t, tc.expectedErr, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expected, decoded)

			metrics, err := dskit_metrics.NewMetricFamilyMapFromGatherer(reg)
			require.NoError(t, err)
			durationHistogram, err := dskit_metrics.FindHistogramWithNameAndLabels(metrics, "cortex_frontend_query_response_codec_duration_seconds", "format", "json", "operation", "decode")
			require.NoError(t, err)
			require.Equal(t, uint64(1), *durationHistogram.SampleCount)
			require.Less(t, *durationHistogram.SampleSum, 0.1)
			payloadSizeHistogram, err := dskit_metrics.FindHistogramWithNameAndLabels(metrics, "cortex_frontend_query_response_codec_payload_bytes", "format", "json", "operation", "decode")
			require.NoError(t, err)
			require.Equal(t, uint64(1), *payloadSizeHistogram.SampleCount)
			require.Equal(t, float64(len(body)), *payloadSizeHistogram.SampleSum)

			httpRequest := &http.Request{
				Header: http.Header{"Accept": []string{jsonMimeType}},
			}

			// Reset response, as the above call will have consumed the body reader.
			httpResponse = &http.Response{
				StatusCode: 200,
				Header:     headers,
				Body: &prometheusReadCloser{
					Reader:    bytes.NewBuffer(body),
					finalizer: func() {},
				},
				ContentLength: int64(len(body)),
			}
			encoded, err := codec.EncodeMetricsQueryResponse(context.Background(), httpRequest, decoded)
			require.NoError(t, err)

			requireEqualHttpResponse(t, httpResponse, encoded)

			metrics, err = dskit_metrics.NewMetricFamilyMapFromGatherer(reg)
			require.NoError(t, err)
			durationHistogram, err = dskit_metrics.FindHistogramWithNameAndLabels(metrics, "cortex_frontend_query_response_codec_duration_seconds", "format", "json", "operation", "encode")
			require.NoError(t, err)
			require.Equal(t, uint64(1), *durationHistogram.SampleCount)
			require.Less(t, *durationHistogram.SampleSum, 0.1)
			payloadSizeHistogram, err = dskit_metrics.FindHistogramWithNameAndLabels(metrics, "cortex_frontend_query_response_codec_payload_bytes", "format", "json", "operation", "encode")
			require.NoError(t, err)
			require.Equal(t, uint64(1), *payloadSizeHistogram.SampleCount)
			require.Equal(t, float64(len(body)), *payloadSizeHistogram.SampleSum)
		})
	}
}

// requireEqualHttpResponse checks the responses are the same with special handling for the Body.
func requireEqualHttpResponse(t *testing.T, expected, actual *http.Response) {
	// Compare all HTTP response fields except the Body
	require.Equal(t, expected.StatusCode, actual.StatusCode)
	require.Equal(t, expected.Header, actual.Header)
	require.Equal(t, expected.ContentLength, actual.ContentLength)

	// Verify that body types match
	require.Equal(t, reflect.TypeOf(expected.Body), reflect.TypeOf(actual.Body))

	// Read and compare the body contents
	expectedJSON, err := readResponseBody(expected)
	require.NoError(t, err)
	actualJSON, err := readResponseBody(actual)
	require.NoError(t, err)
	require.JSONEq(t, string(expectedJSON), string(actualJSON))

	// No need to reset the bodies since they're typically not used after this comparison
}

func TestCodec_JSONResponse_Labels(t *testing.T) {
	headers := http.Header{"Content-Type": []string{"application/json"}}
	expectedRespHeaders := []*PrometheusHeader{
		{
			Name:   "Content-Type",
			Values: []string{"application/json"},
		},
	}

	for _, tc := range []struct {
		name             string
		request          LabelsSeriesQueryRequest
		isSeriesResponse bool
		responseHeaders  http.Header
		resp             prometheusAPIResponse
		expected         Response
		expectedErr      error
	}{
		{
			name:             "successful labels response",
			request:          &PrometheusLabelNamesQueryRequest{},
			isSeriesResponse: false,
			resp: prometheusAPIResponse{
				Status: statusSuccess,
				Data:   []string{"foo", "bar"},
			},
			expected: &PrometheusLabelsResponse{
				Status:  statusSuccess,
				Data:    []string{"foo", "bar"},
				Headers: expectedRespHeaders,
			},
		},
		{
			name:             "successful series response",
			request:          &PrometheusSeriesQueryRequest{},
			isSeriesResponse: true,
			resp: prometheusAPIResponse{
				Status: statusSuccess,
				Data: []SeriesData{
					{
						"__name__": "series_1",
						"foo":      "bar",
					},
					{
						"__name__": "hist_series_1",
						"hoo":      "hbar",
					},
				},
			},
			expected: &PrometheusSeriesResponse{
				Status: statusSuccess,
				Data: []SeriesData{
					{
						"__name__": "series_1",
						"foo":      "bar",
					},
					{
						"__name__": "hist_series_1",
						"hoo":      "hbar",
					},
				},
				Headers: expectedRespHeaders,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewPedanticRegistry()
			codec := NewCodec(reg, 0*time.Minute, formatJSON, nil)

			body, err := json.Marshal(tc.resp)
			require.NoError(t, err)
			httpResponse := &http.Response{
				StatusCode:    200,
				Header:        headers,
				Body:          io.NopCloser(bytes.NewBuffer(body)),
				ContentLength: int64(len(body)),
			}
			decoded, err := codec.DecodeLabelsSeriesQueryResponse(context.Background(), httpResponse, tc.request, log.NewNopLogger())
			if err != nil || tc.expectedErr != nil {
				require.Equal(t, tc.expectedErr, err)
				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.expected, decoded)

			httpRequest := &http.Request{
				Header: http.Header{"Accept": []string{jsonMimeType}},
			}

			// Reset response, as the above call will have consumed the body reader.
			httpResponse = &http.Response{
				StatusCode:    200,
				Header:        headers,
				Body:          io.NopCloser(bytes.NewBuffer(body)),
				ContentLength: int64(len(body)),
			}
			encoded, err := codec.EncodeLabelsSeriesQueryResponse(context.Background(), httpRequest, decoded, tc.isSeriesResponse)
			require.NoError(t, err)

			requireEqualHttpResponse(t, httpResponse, encoded)
		})
	}
}

func TestCodec_JSONEncoding_Metrics(t *testing.T) {
	responseHistogram := mimirpb.FloatHistogram{
		CounterResetHint: histogram.GaugeType,
		Schema:           3,
		ZeroThreshold:    1.23,
		ZeroCount:        456,
		Count:            9001,
		Sum:              789.1,
		PositiveSpans: []mimirpb.BucketSpan{
			{Offset: 4, Length: 1},
			{Offset: 3, Length: 2},
		},
		NegativeSpans: []mimirpb.BucketSpan{
			{Offset: 7, Length: 3},
			{Offset: 9, Length: 1},
		},
		PositiveBuckets: []float64{100, 200, 300},
		NegativeBuckets: []float64{400, 500, 600, 700},
	}

	for _, tc := range []struct {
		name            string
		responseHeaders http.Header
		expectedJSON    string
		response        *PrometheusResponse
		expectedErr     error
	}{
		{
			name: "successful matrix response with histogram values",
			response: &PrometheusResponse{
				Status: statusSuccess,
				Data: &PrometheusData{
					ResultType: model.ValMatrix.String(),
					Result: []SampleStream{
						{Labels: []mimirpb.LabelAdapter{{Name: "foo", Value: "bar"}}, Histograms: []mimirpb.FloatHistogramPair{{TimestampMs: 1_234, Histogram: &responseHistogram}}},
					},
				},
			},
			expectedJSON: `
				{
				  "status": "success",
				  "data": {
					"resultType": "matrix",
					"result": [
					  {
					    "metric": {"foo": "bar"},
					    "histograms": [
					  	  [
					  	    1.234,
					  	    {
					  	  	  "count": "9001",
					  	  	  "sum": "789.1",
					  	  	  "buckets": [
					  	  	    [1, "-5.187358218604039", "-4.756828460010884", "700"],
					  	  	    [1, "-2.1810154653305154", "-2", "600"],
					  	  	    [1, "-2", "-1.8340080864093422", "500"],
					  	  	    [1, "-1.8340080864093422", "-1.6817928305074288", "400"],
					  	  	    [3, "-1.23", "1.23", "456"],
					  	  	    [0, "1.2968395546510096", "1.414213562373095", "100"],
					  	  	    [0, "1.8340080864093422", "2", "200"],
					  	  	    [0, "2", "2.1810154653305154", "300"]
					  	  	  ]
					  	    }
					  	  ]
					    ]
					  }
				    ]
				  }
				}
			`,
		},
		{
			name: "successful matrix response with a single series with both float and histogram values",
			response: &PrometheusResponse{
				Status: statusSuccess,
				Data: &PrometheusData{
					ResultType: model.ValMatrix.String(),
					Result: []SampleStream{
						{
							Labels:     []mimirpb.LabelAdapter{{Name: "foo", Value: "bar"}},
							Samples:    []mimirpb.Sample{{TimestampMs: 1_000, Value: 101}, {TimestampMs: 2_000, Value: 201}},
							Histograms: []mimirpb.FloatHistogramPair{{TimestampMs: 3_000, Histogram: &responseHistogram}}},
					},
				},
			},
			expectedJSON: `
				{
				  "status": "success",
				  "data": {
					"resultType": "matrix",
					"result": [
					  {
						"metric": {"foo": "bar"},
						"histograms": [
						  [
							3,
							{
							  "count": "9001",
							  "sum": "789.1",
							  "buckets": [
								[1, "-5.187358218604039", "-4.756828460010884", "700"],
								[1, "-2.1810154653305154", "-2", "600"],
								[1, "-2", "-1.8340080864093422", "500"],
								[1, "-1.8340080864093422", "-1.6817928305074288", "400"],
								[3, "-1.23", "1.23", "456"],
								[0, "1.2968395546510096", "1.414213562373095", "100"],
								[0, "1.8340080864093422", "2", "200"],
								[0, "2", "2.1810154653305154", "300"]
							  ]
							}
						  ]
						],
						"values": [[1, "101"], [2, "201"]]
					  }
					]
				  }
				}
			`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewPedanticRegistry()
			codec := NewCodec(reg, 0*time.Minute, formatJSON, nil)
			httpRequest := &http.Request{
				Header: http.Header{"Accept": []string{jsonMimeType}},
			}

			encoded, err := codec.EncodeMetricsQueryResponse(context.Background(), httpRequest, tc.response)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, encoded.StatusCode)
			require.Equal(t, "application/json", encoded.Header.Get("Content-Type"))

			encodedJSON, err := readResponseBody(encoded)
			require.NoError(t, err)
			require.JSONEq(t, tc.expectedJSON, string(encodedJSON))
			require.Equal(t, len(encodedJSON), int(encoded.ContentLength))

			metrics, err := dskit_metrics.NewMetricFamilyMapFromGatherer(reg)
			require.NoError(t, err)
			durationHistogram, err := dskit_metrics.FindHistogramWithNameAndLabels(metrics, "cortex_frontend_query_response_codec_duration_seconds", "format", "json", "operation", "encode")
			require.NoError(t, err)
			require.Equal(t, uint64(1), *durationHistogram.SampleCount)
			require.Less(t, *durationHistogram.SampleSum, 0.1)
			payloadSizeHistogram, err := dskit_metrics.FindHistogramWithNameAndLabels(metrics, "cortex_frontend_query_response_codec_payload_bytes", "format", "json", "operation", "encode")
			require.NoError(t, err)
			require.Equal(t, uint64(1), *payloadSizeHistogram.SampleCount)
			require.Equal(t, float64(encoded.ContentLength), *payloadSizeHistogram.SampleSum)
		})
	}
}

func TestCodec_JSONEncoding_Labels(t *testing.T) {
	for _, tc := range []struct {
		name             string
		expectedJSON     string
		response         Response
		isSeriesResponse bool
	}{
		{
			name: "successful labels response",
			response: &PrometheusLabelsResponse{
				Status: statusSuccess,
				Data: []string{
					"foo",
					"bar",
				},
			},
			expectedJSON: `
				{
				  "status": "success",
				  "data": ["foo", "bar"]
				}
			`,
			isSeriesResponse: false,
		},
		{
			name: "successful series response",
			response: &PrometheusSeriesResponse{
				Status: statusSuccess,
				Data: []SeriesData{
					{
						"__name__": "series_1",
						"foo":      "bar",
					},
					{
						"__name__": "hist_series_1",
						"hoo":      "hbar",
					},
				},
			},
			expectedJSON: `
				{
				  "status": "success",
				  "data": [{
					"__name__": "series_1",
					"foo": "bar"
				  }, {
					"__name__": "hist_series_1",
					"hoo": "hbar"
				  }]
				}
			`,
			isSeriesResponse: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewPedanticRegistry()
			codec := NewCodec(reg, 0*time.Minute, formatJSON, nil)
			httpRequest := &http.Request{
				Header: http.Header{"Accept": []string{jsonMimeType}},
			}

			encoded, err := codec.EncodeLabelsSeriesQueryResponse(context.Background(), httpRequest, tc.response, tc.isSeriesResponse)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, encoded.StatusCode)
			require.Equal(t, "application/json", encoded.Header.Get("Content-Type"))

			encodedJSON, err := readResponseBody(encoded)
			require.NoError(t, err)
			require.JSONEq(t, tc.expectedJSON, string(encodedJSON))
			require.Equal(t, len(encodedJSON), int(encoded.ContentLength))
		})
	}
}
