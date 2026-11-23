package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemctlCommandDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, script, selector, want string
		wantState                    string
	}{
		{
			name:     "query bus failure",
			script:   "printf '%s\\n' 'Failed to connect to bus: Permission denied' >&2\nexit 1\n",
			selector: "florin-graph.service",
			want:     "query systemd service florin-graph.service: exit status 1: Failed to connect to bus: Permission denied",
		},
		{
			name:     "discovery bus failure",
			script:   "printf '%s\\n' 'Failed to connect to bus: Permission denied' >&2\nexit 1\n",
			selector: "worker@*.service",
			want:     "discover systemd instances for worker@*.service: exit status 1: Failed to connect to bus: Permission denied",
		},
		{
			name:     "stderr is not state",
			script:   "printf '%s\\n' 'LoadState=not-found' 'ActiveState=inactive' 'SubState=dead' >&2\nexit 1\n",
			selector: "worker.service",
			want:     "query systemd service worker.service: exit status 1:",
		},
		{
			name:      "missing unit remains an outage",
			script:    "printf '%s\\n' 'LoadState=not-found' 'ActiveState=inactive' 'SubState=dead'\nprintf '%s\\n' 'Unit worker.service could not be found.' >&2\nexit 1\n",
			selector:  "worker.service",
			wantState: "not-found",
		},
		{
			name:      "healthy unit with stderr warning",
			script:    "printf '%s\\n' 'LoadState=loaded' 'ActiveState=active' 'SubState=running'\nprintf '%s\\n' 'warning' >&2\n",
			selector:  "worker.service",
			wantState: "active",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\n"+tc.script), 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			services, err := newSystemdClient([]string{tc.selector}).listServices(context.Background())
			if tc.wantState != "" {
				if err != nil || len(services) != 1 || services[0].State != tc.wantState {
					t.Fatalf("services = %v, err = %v", services, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(services) != 0 {
				t.Fatalf("services = %v, err = %v; want %q", services, err, tc.want)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("lost command error: %v", err)
			}
		})
	}
}

func TestSystemctlCancelledQuery(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newSystemdClient([]string{"worker.service"}).listServices(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
}
