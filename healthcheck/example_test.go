// Copyright 2020 by the contributors.
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

package healthcheck

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func Example() {
	_, upstreamURL := upstream() // Mock some upstream Server

	// Create a Handler that we can use to register liveness and readiness checks.
	healthchecks := NewHandler()

	// Add a readiness check to make sure an upstream dependency resolves in DNS.
	// If this fails we don't want to receive requests, but we shouldn't be
	// restarted or rescheduled.
	upstreamHost := upstreamURL.Hostname()
	healthchecks.AddReadinessCheck(
		"upstream-dependency-dns",
		DNSResolveCheck(upstreamHost, 50*time.Millisecond))

	// Add a liveness check to detect Goroutine leaks. If this fails we want
	// to be restarted/rescheduled.
	healthchecks.AddLivenessCheck(
		"goroutine-threshold",
		GoroutineCountCheck(100),
	)

	// Serve http://0.0.0.0:8080/live and http://0.0.0.0:8080/ready endpoints.
	// go http.ListenAndServe("0.0.0.0:8080", healthchecks)

	// Make a request to the readiness endpoint and print the response.
	fmt.Print(dumpRequest(healthchecks, "GET", "/ready"))

	// Output:
	// HTTP/1.1 200 OK
	// Connection: close
	// Content-Type: application/json; charset=utf-8
	//
	// {
	//     "goroutine-threshold": "OK",
	//     "upstream-dependency-dns": "OK"
	// }
}

func Example_database() {
	// Connect to a database/sql database
	var database *sql.DB
	database = connectToDatabase()

	// Create a Handler that we can use to register liveness and readiness checks.
	healthchecks := NewHandler()

	// Add a readiness check to we don't receive requests unless we can reach
	// the database with a ping in <1 second.
	healthchecks.AddReadinessCheck("database", DatabasePingCheck(database, 1*time.Second))

	// Serve http://0.0.0.0:8080/live and http://0.0.0.0:8080/ready endpoints.
	// go http.ListenAndServe("0.0.0.0:8080", healthchecks)

	// Make a request to the readiness endpoint and print the response.
	fmt.Print(dumpRequest(healthchecks, "GET", "/ready"))

	// Output:
	// HTTP/1.1 200 OK
	// Connection: close
	// Content-Type: application/json; charset=utf-8
	//
	// {
	//     "database": "OK"
	// }
}

func Example_advanced() {
	upstream, _ := upstream() // Mock some upstream Server

	// Create a Handler that we can use to register liveness and readiness checks.
	healthchecks := NewHandler()

	// Add a readiness check against the health of an upstream HTTP dependency
	healthchecks.AddReadinessCheck(
		"upstream-dependency-http",
		HTTPGetCheck(upstream.URL, 500*time.Millisecond))

	// Implement a custom check with a 50 millisecond timeout.
	healthchecks.AddLivenessCheck(
		"custom-check-with-timeout",
		Timeout(func() error {
			// Simulate some work that could take a long time
			time.Sleep(time.Millisecond * 100)
			return nil
		}, 50*time.Millisecond),
	)

	// Expose the readiness endpoints on a custom path /healthz mixed into
	// our main application mux.
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("Hello, world!"))
	})
	mux.HandleFunc("/healthz", healthchecks.ReadyEndpoint)

	// Sleep for just a moment to make sure our Async handler had a chance to run
	time.Sleep(500 * time.Millisecond)

	// Make a sample request to the /healthz endpoint and print the response.
	fmt.Println(dumpRequest(mux, "GET", "/healthz"))

	// Output:
	// HTTP/1.1 503 Service Unavailable
	// Connection: close
	// Content-Type: application/json; charset=utf-8
	//
	// {
	//     "custom-check-with-timeout": "timed out after 50ms",
	//     "upstream-dependency-http": "OK"
	// }
}

func Example_metrics() {
	// Create a new Prometheus registry (you'd likely already have one of these).
	registry := prometheus.NewRegistry()

	// Create a metrics-exposing Handler for the Prometheus registry
	// It wraps the default handler to add metrics.
	healthchecks := NewMetricsHandler(NewHandler(), registry)

	// Add a simple readiness check that always fails.
	healthchecks.AddReadinessCheck(
		"failing-check",
		func() error {
			return fmt.Errorf("example failure")
		},
	)

	// Add a liveness check that always succeeds
	healthchecks.AddLivenessCheck(
		"successful-check",
		func() error {
			return nil
		},
	)

	// Create an "admin" listener on 0.0.0.0:9402
	internal := http.NewServeMux()
	// go http.ListenAndServe("0.0.0.0:9402", internal)

	// Expose prometheus metrics on /metrics
	internal.Handle("/metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))

	// Expose a liveness check on /live and readiness check on /ready
	internal.HandleFunc("/live", healthchecks.LiveEndpoint)
	internal.HandleFunc("/ready", healthchecks.ReadyEndpoint)

	// Make a request to the metrics endpoint and print the response.
	fmt.Println(dumpRequest(internal, "GET", "/metrics"))

	// Output:
	// HTTP/1.1 200 OK
	// Connection: close
	// Content-Type: text/plain; version=0.0.4; charset=utf-8
	//
	// # HELP healthcheck Indicates if check is healthy (1 is healthy, 0 is unhealthy)
	// # TYPE healthcheck gauge
	// healthcheck{check="live",name="successful-check"} 1
	// healthcheck{check="ready",name="failing-check"} 0
}

func upstream() (*httptest.Server, *url.URL) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	u, _ := url.Parse(s.URL)
	return s, u
}

func dumpRequest(handler http.Handler, method string, path string) string {
	req, err := http.NewRequest(method, path, nil)
	if err != nil {
		panic(err)
	}
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	dump, err := httputil.DumpResponse(rr.Result(), true)
	if err != nil {
		panic(err)
	}
	return strings.Replace(string(dump), "\r\n", "\n", -1)
}

func connectToDatabase() *sql.DB {
	db, _, err := sqlmock.New()
	if err != nil {
		panic(err)
	}
	return db
}
