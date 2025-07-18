// SPDX-License-Identifier: AGPL-3.0-only
// Provenance-includes-location: https://github.com/cortexproject/cortex/blob/master/pkg/frontend/frontend_test.go
// Provenance-includes-license: Apache-2.0
// Provenance-includes-copyright: The Cortex Authors.

package frontend

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	strconv "strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-kit/log"
	"github.com/gorilla/mux"
	"github.com/grafana/dskit/clusterutil"
	"github.com/grafana/dskit/concurrency"
	"github.com/grafana/dskit/flagext"
	httpgrpc_server "github.com/grafana/dskit/httpgrpc/server"
	"github.com/grafana/dskit/middleware"
	"github.com/grafana/dskit/services"
	"github.com/grafana/dskit/user"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"google.golang.org/grpc"

	"github.com/grafana/mimir/pkg/frontend/querymiddleware"
	"github.com/grafana/mimir/pkg/frontend/transport"
	"github.com/grafana/mimir/pkg/frontend/v1/frontendv1pb"
	querier_worker "github.com/grafana/mimir/pkg/querier/worker"
	"github.com/grafana/mimir/pkg/scheduler/schedulerdiscovery"
)

const (
	query        = "/api/v1/query_range?end=1536716898&query=sum%28container_memory_rss%29+by+%28namespace%29&start=1536673680&step=120"
	responseBody = `{"status":"success","data":{"resultType":"Matrix","result":[{"metric":{"foo":"bar"},"values":[[1536673680,"137"],[1536673780,"137"]]}]}}`
)

func TestFrontend_RequestHostHeaderWhenDownstreamURLIsConfigured(t *testing.T) {
	// Create an HTTP server listening locally. This server mocks the downstream
	// Prometheus API-compatible server.
	downstreamListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	observedHost := make(chan string, 2)
	downstreamServer := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			observedHost <- r.Host

			_, err := w.Write([]byte(responseBody))
			require.NoError(t, err)
		}),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	defer downstreamServer.Shutdown(context.Background()) //nolint:errcheck
	go downstreamServer.Serve(downstreamListen)           //nolint:errcheck

	// Configure the query-frontend with the mocked downstream server.
	config := defaultFrontendConfig()
	config.DownstreamURL = fmt.Sprintf("http://%s", downstreamListen.Addr())

	// Configure the test to send a request to the query-frontend and assert on the
	// Host HTTP header received by the downstream server.
	test := func(addr string) {
		req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/%s", addr, query), nil)
		require.NoError(t, err)

		ctx := context.Background()
		req = req.WithContext(ctx)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(ctx, "1"), req)
		require.NoError(t, err)

		client := http.Client{
			Transport: otelhttp.NewTransport(nil),
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.Equal(t, 200, resp.StatusCode)

		defer resp.Body.Close()
		_, err = io.ReadAll(resp.Body)
		require.NoError(t, err)

		// We expect the Host received by the downstream is the downstream host itself
		// and not the query-frontend host.
		downstreamReqHost := <-observedHost
		assert.Equal(t, downstreamListen.Addr().String(), downstreamReqHost)
		assert.NotEqual(t, downstreamReqHost, addr)
	}

	testFrontend(t, config, nil, test, nil, nil)
}

func TestFrontend_ClusterValidationWhenDownstreamURLIsConfigured(t *testing.T) {
	testCases := map[string]struct {
		serverClusterValidation clusterutil.ServerClusterValidationConfig
		clientClusterValidation clusterutil.ClusterValidationConfig
		expectedMetrics         string
		serverCluster           string
		enabled                 bool
		softValidation          bool
		clientCluster           string
		expectedError           string
		expectedStatusCode      int
	}{
		"if server and client have equal cluster validation labels and cluster validation is disabled no error is returned": {
			clientCluster:      "cluster",
			serverCluster:      "cluster",
			enabled:            false,
			expectedStatusCode: 200,
		},
		"if server and client have different cluster validation labels and cluster validation is disabled no error is returned": {
			clientCluster:      "client-cluster",
			serverCluster:      "server-cluster",
			enabled:            false,
			expectedStatusCode: 200,
		},
		"if server and client have equal cluster validation labels and cluster validation is enabled no error is returned": {
			clientCluster:      "cluster",
			serverCluster:      "cluster",
			enabled:            true,
			expectedStatusCode: 200,
		},
		"if server and client have different cluster validation labels and soft cluster validation is enabled no error is returned": {
			clientCluster:      "client-cluster",
			serverCluster:      "server-cluster",
			enabled:            true,
			softValidation:     true,
			expectedStatusCode: 200,
		},
		"if server and client have different cluster validation labels and cluster validation is enabled an error is returned": {
			clientCluster:      "client-cluster",
			serverCluster:      "server-cluster",
			enabled:            true,
			softValidation:     false,
			expectedStatusCode: 500,
			expectedError:      `request rejected by the server: rejected request with wrong cluster validation label "client-cluster" - it should be "server-cluster"`,
			expectedMetrics: `
				# HELP cortex_client_invalid_cluster_validation_label_requests_total Number of requests with invalid cluster validation label.
        	    # TYPE cortex_client_invalid_cluster_validation_label_requests_total counter
        	    cortex_client_invalid_cluster_validation_label_requests_total{client="querier",method="<unknown-route>",protocol="http"} 1
			`,
		},
		"if client has no cluster validation label and soft cluster validation is enabled no error is returned": {
			clientCluster:      "",
			serverCluster:      "server-cluster",
			enabled:            true,
			softValidation:     true,
			expectedStatusCode: 200,
		},
		"if client has no cluster validation label and cluster validation is enabled an error is returned": {
			clientCluster:      "",
			serverCluster:      "server-cluster",
			enabled:            true,
			softValidation:     false,
			expectedStatusCode: 511,
			expectedError:      "rejected request with empty cluster validation label - it should be \\\"server-cluster\\\"",
		},
	}
	for testName, testCase := range testCases {
		t.Run(testName, func(t *testing.T) {
			// Create an HTTP server listening locally. This server mocks the downstream
			// Prometheus API-compatible server.
			downstreamListen, err := net.Listen("tcp", "localhost:0")
			require.NoError(t, err)

			var handler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, err := w.Write([]byte(responseBody))
				require.NoError(t, err)
			})
			logger := log.NewNopLogger()
			if testCase.enabled {
				reg := prometheus.NewPedanticRegistry()
				handler = middleware.ClusterValidationMiddleware(
					testCase.serverCluster, []string{}, testCase.softValidation,
					middleware.NewInvalidClusterRequests(reg, "cortex"), logger,
				).Wrap(handler)
			}
			downstreamServer := http.Server{
				Handler:      handler,
				ReadTimeout:  10 * time.Second,
				WriteTimeout: 10 * time.Second,
			}

			defer downstreamServer.Shutdown(context.Background()) //nolint:errcheck
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				downstreamServer.Serve(downstreamListen) //nolint:errcheck
			}()
			t.Cleanup(wg.Wait)

			// Configure the query-frontend with the mocked downstream server.
			config := defaultFrontendConfig()
			config.DownstreamURL = fmt.Sprintf("http://%s", downstreamListen.Addr())
			config.ClusterValidationConfig.Label = testCase.clientCluster

			reg := prometheus.NewPedanticRegistry()

			// Configure the test to send a request to the query-frontend and assert on the
			// Host HTTP header received by the downstream server.
			test := func(addr string) {
				req, err := http.NewRequest("GET", fmt.Sprintf("http://%s/%s", addr, query), nil)
				require.NoError(t, err)

				ctx := context.Background()
				req = req.WithContext(ctx)
				err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(ctx, "1"), req)
				require.NoError(t, err)

				client := http.Client{
					Transport: otelhttp.NewTransport(nil),
				}
				resp, err := client.Do(req)
				require.NoError(t, err)
				require.Equal(t, testCase.expectedStatusCode, resp.StatusCode)
				defer resp.Body.Close()
				if resp.StatusCode != 200 {
					body, err := io.ReadAll(resp.Body)
					require.NoError(t, err)
					require.Contains(t, string(body), testCase.expectedError)

					if testCase.expectedMetrics != "" {
						err = testutil.GatherAndCompare(reg, strings.NewReader(testCase.expectedMetrics), "cortex_client_invalid_cluster_validation_label_requests_total")
						require.NoError(t, err)
					}
				}
			}

			testFrontend(t, config, nil, test, nil, reg)
		})
	}
}

func TestFrontend_LogsSlowQueriesFormValues(t *testing.T) {
	// Create an HTTP server listening locally. This server mocks the downstream
	// Prometheus API-compatible server.
	downstreamListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	downstreamServer := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, err := w.Write([]byte(responseBody))
			require.NoError(t, err)
		}),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	defer downstreamServer.Shutdown(context.Background()) //nolint:errcheck
	go downstreamServer.Serve(downstreamListen)           //nolint:errcheck

	// Configure the query-frontend with the mocked downstream server.
	config := defaultFrontendConfig()
	config.Handler.LogQueriesLongerThan = 1 * time.Microsecond
	config.DownstreamURL = fmt.Sprintf("http://%s", downstreamListen.Addr())

	var buf concurrency.SyncBuffer
	l := log.NewLogfmtLogger(&buf)

	test := func(addr string) {
		data := url.Values{}
		data.Set("test", "form")
		data.Set("issue", "3111")

		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/?foo=bar", addr), strings.NewReader(data.Encode()))
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))

		ctx := context.Background()
		req = req.WithContext(ctx)
		assert.NoError(t, err)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(ctx, "1"), req)
		assert.NoError(t, err)

		client := http.Client{
			Transport: otelhttp.NewTransport(nil),
		}

		resp, err := client.Do(req)
		assert.NoError(t, err)
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()

		assert.NoError(t, err)
		assert.Equal(t, 200, resp.StatusCode, string(b))

		logs := buf.String()
		assert.NotContains(t, logs, "unable to parse form for request")
		assert.Contains(t, logs, "msg=\"slow query detected\"")
		assert.Contains(t, logs, "param_issue=3111")
		assert.Contains(t, logs, "param_test=form")
		assert.Contains(t, logs, "param_foo=bar")
	}

	testFrontend(t, config, nil, test, l, nil)
}

func TestFrontend_ReturnsRequestBodyTooLargeError(t *testing.T) {
	// Create an HTTP server listening locally. This server mocks the downstream
	// Prometheus API-compatible server.
	downstreamListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	downstreamServer := http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, err := w.Write([]byte(responseBody))
			require.NoError(t, err)
		}),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	defer downstreamServer.Shutdown(context.Background()) //nolint:errcheck
	go downstreamServer.Serve(downstreamListen)           //nolint:errcheck

	// Configure the query-frontend with the mocked downstream server.
	config := defaultFrontendConfig()
	config.DownstreamURL = fmt.Sprintf("http://%s", downstreamListen.Addr())
	config.Handler.MaxBodySize = 1

	test := func(addr string) {
		data := url.Values{}
		data.Set("test", "max body size")

		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://%s/?foo=bar", addr), strings.NewReader(data.Encode()))
		req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Add("Content-Length", strconv.Itoa(len(data.Encode())))

		ctx := context.Background()
		req = req.WithContext(ctx)
		assert.NoError(t, err)
		err = user.InjectOrgIDIntoHTTPRequest(user.InjectOrgID(ctx, "1"), req)
		assert.NoError(t, err)

		client := http.Client{
			Transport: otelhttp.NewTransport(nil),
		}

		resp, err := client.Do(req)
		assert.NoError(t, err)
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		assert.NoError(t, err)

		assert.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode, string(b))
	}

	testFrontend(t, config, nil, test, nil, nil)
}

func testFrontend(t *testing.T, config CombinedFrontendConfig, handler http.Handler, test func(addr string), l log.Logger, reg prometheus.Registerer) {
	logger := log.NewNopLogger()
	if l != nil {
		logger = l
	}
	codec := querymiddleware.NewCodec(prometheus.NewPedanticRegistry(), 0*time.Minute, "json", nil)

	var workerConfig querier_worker.Config
	flagext.DefaultValues(&workerConfig)
	workerConfig.MaxConcurrentRequests = 1

	// localhost:0 prevents firewall warnings on Mac OS X.
	grpcListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)
	workerConfig.FrontendAddress = grpcListen.Addr().String()

	httpListen, err := net.Listen("tcp", "localhost:0")
	require.NoError(t, err)

	rt, v1, v2, err := InitFrontend(config, limits{}, limits{}, 0, logger, reg, codec)
	require.NoError(t, err)
	require.NotNil(t, rt)
	// v1 will be nil if DownstreamURL is defined.
	require.Nil(t, v2)
	if v1 != nil {
		require.NoError(t, services.StartAndAwaitRunning(context.Background(), v1))
		t.Cleanup(func() {
			require.NoError(t, services.StopAndAwaitTerminated(context.Background(), v1))
		})
	}

	grpcServer := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
	)
	defer grpcServer.GracefulStop()

	if v1 != nil {
		frontendv1pb.RegisterFrontendServer(grpcServer, v1)
	}

	r := mux.NewRouter()
	r.PathPrefix("/").Handler(middleware.Merge(
		middleware.AuthenticateUser,
		middleware.Tracer{},
	).Wrap(transport.NewHandler(config.Handler, rt, logger, reg, nil)))

	httpServer := http.Server{
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}
	defer httpServer.Shutdown(context.Background()) //nolint:errcheck

	go httpServer.Serve(httpListen) //nolint:errcheck
	go grpcServer.Serve(grpcListen) //nolint:errcheck

	var worker services.Service
	worker, err = querier_worker.NewQuerierWorker(workerConfig, httpgrpc_server.NewServer(handler, httpgrpc_server.WithReturn4XXErrors), logger, reg)
	require.NoError(t, err)
	require.NoError(t, services.StartAndAwaitRunning(context.Background(), worker))

	test(httpListen.Addr().String())

	require.NoError(t, services.StopAndAwaitTerminated(context.Background(), worker))
}

func defaultFrontendConfig() CombinedFrontendConfig {
	config := CombinedFrontendConfig{}
	flagext.DefaultValues(&config)
	flagext.DefaultValues(&config.Handler)
	flagext.DefaultValues(&config.FrontendV1)
	flagext.DefaultValues(&config.FrontendV2)

	querySchedulerDiscoveryConfig := schedulerdiscovery.Config{}
	flagext.DefaultValues(&querySchedulerDiscoveryConfig)
	config.FrontendV2.QuerySchedulerDiscovery = querySchedulerDiscoveryConfig

	return config
}

type limits struct {
	queriers             int
	queryIngestersWithin time.Duration
}

func (l limits) MaxQueriersPerUser(_ string) int {
	return l.queriers
}

func (l limits) QueryIngestersWithin(string) time.Duration {
	return l.queryIngestersWithin
}
