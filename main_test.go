package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestReconcileAlertsAfterFiveMinutesOnce(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	service := Service{ID: "abc123", Name: "worker", State: "activating/auto-restart"}

	state, alerts := reconcile(State{}, []Service{service}, now, 5*time.Minute)
	if len(alerts) != 0 {
		t.Fatalf("new failure produced %d alerts; want 0", len(alerts))
	}
	if got := state.Services[service.ID].FirstSeen; !got.Equal(now) {
		t.Fatalf("first seen = %s; want %s", got, now)
	}

	state, alerts = reconcile(state, []Service{service}, now.Add(5*time.Minute), 5*time.Minute)
	if len(alerts) != 1 || alerts[0].ID != service.ID {
		t.Fatalf("alerts = %#v; want one alert for %s", alerts, service.ID)
	}
	if state.Services[service.ID].Notified {
		t.Fatal("service was marked notified before webhook delivery")
	}

	state = markNotified(state, service.ID)
	_, alerts = reconcile(state, []Service{service}, now.Add(10*time.Minute), 5*time.Minute)
	if len(alerts) != 0 {
		t.Fatalf("already-notified failure produced %d alerts; want 0", len(alerts))
	}
}

func TestReconcileClearsRecoveredServices(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	state := State{Services: map[string]Failure{
		"abc123": {Name: "worker", State: "activating/auto-restart", FirstSeen: now, Notified: true},
	}}

	state, alerts := reconcile(state, []Service{{ID: "abc123", Name: "worker", State: "active"}}, now.Add(time.Minute), 5*time.Minute)
	if len(alerts) != 0 {
		t.Fatalf("recovery produced %d alerts; want 0", len(alerts))
	}
	if len(state.Services) != 0 {
		t.Fatalf("recovered service remains in state: %#v", state.Services)
	}
}

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	want := State{Services: map[string]Failure{
		"abc123": {
			Name:      "worker",
			State:     "activating/auto-restart",
			FirstSeen: time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC),
			Notified:  true,
		},
	}}

	if err := saveState(path, want); err != nil {
		t.Fatalf("save state: %v", err)
	}
	got, err := loadState(path)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if failure := got.Services["abc123"]; failure != want.Services["abc123"] {
		t.Fatalf("loaded failure = %#v; want %#v", failure, want.Services["abc123"])
	}
}

func TestLoadMissingStateStartsEmpty(t *testing.T) {
	state, err := loadState(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatalf("load missing state: %v", err)
	}
	if state.Services == nil || len(state.Services) != 0 {
		t.Fatalf("state = %#v; want initialized empty map", state)
	}
}

func TestSendWebhookPostsIncidentJSON(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s; want POST", r.Method)
		}
		if contentType := r.Header.Get("Content-Type"); contentType != "application/json" {
			t.Errorf("content type = %q; want application/json", contentType)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	firstSeen := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	now := firstSeen.Add(5 * time.Minute)
	err := sendWebhook(context.Background(), server.Client(), server.URL, "testhost", Service{
		ID: "abc123", Name: "worker", State: "activating/auto-restart",
	}, firstSeen, now)
	if err != nil {
		t.Fatalf("send webhook: %v", err)
	}
	incident, ok := got["incident"].(map[string]any)
	if !ok {
		t.Fatalf("payload has no incident object: %#v", got)
	}
	wantIncidentID := "systemd-testhost-abc123-" + strconv.FormatInt(firstSeen.UnixNano(), 10)
	if incident["id"] != wantIncidentID || incident["status"] != "alert" {
		t.Fatalf("incident identity/status = %#v", incident)
	}
	metadata, ok := incident["metadata"].(map[string]any)
	if !ok {
		t.Fatalf("incident has no metadata object: %#v", incident)
	}
	if metadata["service_id"] != "abc123" || metadata["service_name"] != "worker" || metadata["state"] != "activating/auto-restart" {
		t.Fatalf("metadata = %#v", metadata)
	}
	if metadata["duration_seconds"] != float64(300) {
		t.Fatalf("duration_seconds = %#v; want 300", metadata["duration_seconds"])
	}
}

func TestSendWebhookRejectsErrorResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no", http.StatusBadGateway)
	}))
	defer server.Close()

	err := sendWebhook(context.Background(), server.Client(), server.URL, "testhost",
		Service{ID: "abc123", Name: "worker", State: "dead"}, time.Now(), time.Now())
	if err == nil {
		t.Fatal("send webhook succeeded for a 502 response")
	}
}
