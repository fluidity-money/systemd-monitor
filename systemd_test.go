package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSystemdStates(t *testing.T) {
	for _, tc := range []struct {
		load, active, sub, want string
		commandError            bool
	}{
		{"loaded", "active", "running", "active", false},
		{"loaded", "active", "exited", "active", false},
		{"loaded", "failed", "failed", "failed/failed", false},
		{"loaded", "inactive", "dead", "inactive/dead", false},
		{"loaded", "activating", "auto-restart", "activating/auto-restart", false},
		{"loaded", "deactivating", "stop", "deactivating/stop", false},
		{"not-found", "inactive", "dead", "not-found", true},
		{"masked", "inactive", "dead", "masked", false},
	} {
		t.Run(tc.want+tc.sub, func(t *testing.T) {
			client := &systemdClient{units: []string{"worker.service"}, run: func(ctx context.Context, args ...string) ([]byte, error) {
				want := []string{"--system", "--no-pager", "show", "--property=LoadState,ActiveState,SubState", "--", "worker.service"}
				if !reflect.DeepEqual(args, want) {
					t.Fatalf("args = %v", args)
				}
				if _, ok := ctx.Deadline(); !ok {
					t.Fatal("missing query deadline")
				}
				var err error
				if tc.commandError {
					err = errors.New("exit 1")
				}
				return []byte("SubState=" + tc.sub + "\nLoadState=" + tc.load + "\nActiveState=" + tc.active + "\n"), err
			}}
			services, err := client.listServices(context.Background())
			if err != nil || len(services) != 1 || services[0].State != tc.want {
				t.Fatalf("services = %v, err = %v", services, err)
			}
			state, _ := reconcile(State{}, services, time.Now(), alertAfter)
			if (len(state.Services) == 0) != (tc.want == "active") {
				t.Fatalf("wrong health classification: %v", state)
			}
		})
	}
}

func TestSystemdQueryErrors(t *testing.T) {
	for _, output := range []string{"", "LoadState=loaded\n", "LoadState=loaded\nActiveState=active\nSubState=running\n"} {
		client := &systemdClient{units: []string{"worker.service"}, run: func(context.Context, ...string) ([]byte, error) {
			return []byte(output), errors.New("bus unavailable")
		}}
		if _, err := client.listServices(context.Background()); err == nil {
			t.Fatal("accepted failed query")
		}
	}
	client := &systemdClient{units: []string{"worker.service"}, run: func(context.Context, ...string) ([]byte, error) { return []byte("LoadState=loaded\n"), nil }}
	if _, err := client.listServices(context.Background()); err == nil {
		t.Fatal("accepted incomplete properties")
	}
}

type fakeLister struct {
	services []Service
	err      error
}

func (f fakeLister) listServices(context.Context) ([]Service, error) { return f.services, f.err }

func TestCheckRetriesPersistsAndRearms(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	cfg := config{StateFile: filepath.Join(t.TempDir(), "state.json"), WebhookURL: server.URL, Hostname: "host"}
	now := time.Now().UTC()
	lister := fakeLister{services: []Service{{ID: "worker.service", Name: "worker.service", State: "failed/failed"}}}
	state, err := checkOnce(context.Background(), lister, server.Client(), cfg, State{}, now)
	if err != nil || calls != 0 {
		t.Fatalf("first observation: %v, %d calls", err, calls)
	}
	state, err = checkOnce(context.Background(), lister, server.Client(), cfg, state, now.Add(alertAfter-time.Nanosecond))
	if err != nil || calls != 0 {
		t.Fatal("alert before threshold")
	}
	state, err = checkOnce(context.Background(), lister, server.Client(), cfg, state, now.Add(alertAfter))
	if err == nil || calls != 1 || state.Services["worker.service"].Notified {
		t.Fatal("failed delivery was not retained")
	}
	state, err = loadState(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	state, err = checkOnce(context.Background(), lister, server.Client(), cfg, state, now.Add(alertAfter+time.Second))
	if err != nil || calls != 2 || !state.Services["worker.service"].Notified {
		t.Fatal("delivery was not retried")
	}
	state, err = loadState(cfg.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	state, err = checkOnce(context.Background(), lister, server.Client(), cfg, state, now.Add(2*alertAfter))
	if err != nil || calls != 2 {
		t.Fatal("duplicate delivery after restart")
	}
	lister.services[0].State = "active"
	state, err = checkOnce(context.Background(), lister, server.Client(), cfg, state, now.Add(3*alertAfter))
	if err != nil || len(state.Services) != 0 || calls != 2 {
		t.Fatal("recovery not cleared")
	}
	lister.services[0].State = "inactive/dead"
	state, err = checkOnce(context.Background(), lister, server.Client(), cfg, state, now.Add(4*alertAfter))
	if err != nil || calls != 2 {
		t.Fatal("new outage did not restart threshold")
	}
	_, err = checkOnce(context.Background(), lister, server.Client(), cfg, state, now.Add(5*alertAfter))
	if err != nil || calls != 3 {
		t.Fatal("new outage not reported")
	}
}

func TestQueryAndSaveErrorsDoNotNotify(t *testing.T) {
	cfg := config{StateFile: filepath.Join(t.TempDir(), "state.json")}
	previous := State{Services: map[string]Failure{"worker.service": {FirstSeen: time.Now()}}}
	got, err := checkOnce(context.Background(), fakeLister{err: errors.New("offline")}, nil, cfg, previous, time.Now())
	if err == nil || !reflect.DeepEqual(got, previous) {
		t.Fatal("query error lost state")
	}
	if _, err := os.Stat(cfg.StateFile); !os.IsNotExist(err) {
		t.Fatal("query error wrote state")
	}
	cfg.StateFile = t.TempDir() // Cannot replace a directory with a state file.
	_, err = checkOnce(context.Background(), fakeLister{services: []Service{{ID: "worker.service", State: "failed"}}}, nil, cfg, previous, time.Now().Add(alertAfter))
	if err == nil {
		t.Fatal("save failure ignored")
	}
}

func TestConfig(t *testing.T) {
	t.Setenv("STATE_FILE", "/tmp/state.json")
	t.Setenv("BETTERUPTIME_WEBHOOK_URL", "https://example.com/hook")
	t.Setenv("SYSTEMD_SERVICES", "nginx, worker@one.service nginx.service")
	t.Setenv("POLL_INTERVAL", "")
	cfg, err := configFromEnv()
	if err != nil || !reflect.DeepEqual(cfg.Services, []string{"nginx.service", "worker@one.service"}) || cfg.PollInterval != 30*time.Second {
		t.Fatalf("config = %#v, err = %v", cfg, err)
	}
	for _, unit := range []string{"", "*.service", "-bad.service", "worker@.service", "foo.timer", "/tmp/a.service"} {
		t.Setenv("SYSTEMD_SERVICES", unit)
		if _, err := configFromEnv(); err == nil {
			t.Fatalf("accepted unit %q", unit)
		}
	}
	t.Setenv("SYSTEMD_SERVICES", "nginx")
	for _, key := range []string{"STATE_FILE", "BETTERUPTIME_WEBHOOK_URL"} {
		old := os.Getenv(key)
		t.Setenv(key, "")
		if _, err := configFromEnv(); err == nil {
			t.Fatalf("accepted absent %s", key)
		}
		t.Setenv(key, old)
	}
	t.Setenv("POLL_INTERVAL", "0s")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("accepted zero interval")
	}
	t.Setenv("POLL_INTERVAL", "1s")
	t.Setenv("BETTERUPTIME_WEBHOOK_URL", "file:///secret")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("accepted non-HTTP webhook")
	}
}

func TestCorruptStateFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadState(path); err == nil {
		t.Fatal("accepted corrupt state")
	}
}

func TestWebhookErrorDoesNotLeakURL(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := sendWebhook(ctx, http.DefaultClient, "https://example.com/secret-token", "host", Service{}, time.Now(), time.Now())
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("unsafe error: %v", err)
	}
}
