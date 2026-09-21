// Copyright 2026 The KubeVirt Metrics Exporter Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	resp := httptest.NewRecorder()

	healthHandler().ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.Code, http.StatusOK)
	}
	if body := resp.Body.String(); body != "ok" {
		t.Errorf("body = %q, want %q", body, "ok")
	}
}

func TestMetricsHandlerHealthEndpoint(t *testing.T) {
	tests := []struct {
		name          string
		includeHealth bool
		wantStatus    int
	}{
		{name: "enabled without separate health server", includeHealth: true, wantStatus: http.StatusOK},
		{name: "disabled with separate health server", includeHealth: false, wantStatus: http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			metricsHandler(tt.includeHealth).ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			if resp.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", resp.Code, tt.wantStatus)
			}
		})
	}
}
