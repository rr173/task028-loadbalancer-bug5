package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthRequiresExplicitHealthyField(t *testing.T) {
	api := New()
	register := httptest.NewRequest(http.MethodPost, "/nodes", strings.NewReader(`{"id":"a","address":"a:80","weight":1}`))
	register.Header.Set("Content-Type", "application/json")
	registerRR := httptest.NewRecorder()
	api.Handler().ServeHTTP(registerRR, register)
	if registerRR.Code != http.StatusOK {
		t.Fatalf("register status=%d body=%s", registerRR.Code, registerRR.Body.String())
	}

	health := httptest.NewRequest(http.MethodPost, "/health", strings.NewReader(`{"id":"a"}`))
	health.Header.Set("Content-Type", "application/json")
	healthRR := httptest.NewRecorder()
	api.Handler().ServeHTTP(healthRR, health)
	if healthRR.Code != http.StatusBadRequest {
		t.Fatalf("missing healthy status=%d body=%s", healthRR.Code, healthRR.Body.String())
	}

	stats := httptest.NewRequest(http.MethodGet, "/stats", nil)
	statsRR := httptest.NewRecorder()
	api.Handler().ServeHTTP(statsRR, stats)
	var got struct{ Nodes []struct{ Healthy bool `json:"healthy"` } `json:"nodes"` }
	if err := json.Unmarshal(statsRR.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes) != 1 || !got.Nodes[0].Healthy {
		t.Fatalf("stats after invalid health=%s, node should remain healthy", statsRR.Body.String())
	}
}
