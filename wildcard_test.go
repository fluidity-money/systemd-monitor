package main

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestTemplateConfig(t *testing.T) {
	t.Setenv("STATE_FILE", "/tmp/state.json")
	t.Setenv("BETTERUPTIME_WEBHOOK_URL", "https://example.com/hook")
	t.Setenv("POLL_INTERVAL", "")
	for _, selector := range []string{"worker@", "worker@.service", "worker@*", "worker@*.service", "my.worker@"} {
		t.Setenv("SYSTEMD_SERVICES", selector)
		cfg, err := configFromEnv()
		want := "worker@*.service"
		if selector == "my.worker@" {
			want = "my.worker@*.service"
		}
		if err != nil || !reflect.DeepEqual(cfg.Services, []string{want}) {
			t.Fatalf("%s: %#v, %v", selector, cfg, err)
		}
	}
}

func TestTemplateDiscoveryRefreshAndDeduplication(t *testing.T) {
	output := "worker@one.service loaded active running Worker one\nworker@two.service loaded failed failed Worker two\n"
	var queried []string
	client := &systemdClient{units: []string{"worker@*.service", "worker@one.service"}, run: func(ctx context.Context, args ...string) ([]byte, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Fatal("missing deadline")
		}
		if args[2] == "list-units" {
			want := []string{"--system", "--no-pager", "list-units", "--all", "--type=service", "--plain", "--no-legend", "--full", "--", "worker@*.service"}
			if !reflect.DeepEqual(args, want) {
				t.Fatalf("discovery args: %v", args)
			}
			return []byte(output), nil
		}
		queried = append(queried, args[len(args)-1])
		return []byte("LoadState=loaded\nActiveState=failed\nSubState=failed\n"), nil
	}}
	services, err := client.listServices(context.Background())
	if err != nil || len(services) != 2 || !reflect.DeepEqual(queried, []string{"worker@one.service", "worker@two.service"}) {
		t.Fatalf("services %v, queries %v, err %v", services, queried, err)
	}
	output = "worker@three.service loaded active running New worker\n"
	queried = nil
	_, err = client.listServices(context.Background())
	if err != nil || !reflect.DeepEqual(queried, []string{"worker@three.service", "worker@one.service"}) {
		t.Fatalf("discovery not refreshed: %v, %v", queried, err)
	}
	output = ""
	client.units = []string{"worker@*.service"}
	services, err = client.listServices(context.Background())
	if err != nil || len(services) != 0 {
		t.Fatalf("empty match: %v, %v", services, err)
	}
}

func TestTemplateDiscoveryErrors(t *testing.T) {
	for _, output := range []string{"garbage", "other@one.service loaded active running", "worker@.service loaded inactive dead"} {
		client := &systemdClient{units: []string{"worker@*.service"}, run: func(context.Context, ...string) ([]byte, error) { return []byte(output), nil }}
		if _, err := client.listServices(context.Background()); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
	client := &systemdClient{units: []string{"worker@*.service"}, run: func(context.Context, ...string) ([]byte, error) { return nil, errors.New("bus unavailable") }}
	if _, err := client.listServices(context.Background()); err == nil {
		t.Fatal("discovery failure ignored")
	}
}
