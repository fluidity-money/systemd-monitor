package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

const (
	defaultPollInterval = 30 * time.Second
	alertAfter          = 5 * time.Minute
)

type Service struct {
	ID    string
	Name  string
	State string
}

type Failure struct {
	Name      string    `json:"name"`
	State     string    `json:"state"`
	FirstSeen time.Time `json:"first_seen"`
	Notified  bool      `json:"notified"`
}

type State struct {
	Services map[string]Failure `json:"services"`
}

type config struct {
	StateFile    string
	WebhookURL   string
	Services     []string
	Hostname     string
	PollInterval time.Duration
}

type serviceLister interface {
	listServices(context.Context) ([]Service, error)
}

type systemdClient struct {
	units []string
	run   func(context.Context, ...string) ([]byte, error)
}

func main() {
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatal(err)
	}

	state, err := loadState(cfg.StateFile)
	if err != nil {
		log.Fatalf("load state: %v", err)
	}

	systemd := newSystemdClient(cfg.Services)
	webhookClient := &http.Client{Timeout: 15 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	check := func() {
		var checkErr error
		state, checkErr = checkOnce(ctx, systemd, webhookClient, cfg, state, time.Now().UTC())
		if checkErr != nil {
			log.Printf("check failed: %v", checkErr)
		}
	}

	check()
	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			check()
		}
	}
}

func configFromEnv() (config, error) {
	cfg := config{
		StateFile:  os.Getenv("STATE_FILE"),
		WebhookURL: os.Getenv("BETTERUPTIME_WEBHOOK_URL"),

		PollInterval: defaultPollInterval,
	}
	if cfg.StateFile == "" {
		return config{}, errors.New("STATE_FILE is required")
	}
	if cfg.WebhookURL == "" {
		return config{}, errors.New("BETTERUPTIME_WEBHOOK_URL is required")
	}
	u, err := url.Parse(cfg.WebhookURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return config{}, errors.New("BETTERUPTIME_WEBHOOK_URL must be an HTTP(S) URL without userinfo")
	}
	units := strings.FieldsFunc(os.Getenv("SYSTEMD_SERVICES"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	if len(units) == 0 {
		return config{}, errors.New("SYSTEMD_SERVICES is required")
	}
	seen := make(map[string]bool)
	valid := regexp.MustCompile(`^[a-zA-Z0-9_:][a-zA-Z0-9_: .@\\-]*$`)
	for _, unit := range units {
		if strings.HasSuffix(unit, "@") || strings.HasSuffix(unit, "@*") {
			unit += ".service"
		} else if !strings.Contains(unit, ".") {
			unit += ".service"
		}
		if strings.HasSuffix(unit, "@.service") {
			unit = strings.TrimSuffix(unit, "@.service") + "@*.service"
		}
		validationName := strings.ReplaceAll(unit, "@*.service", "@instance.service")
		if !valid.MatchString(validationName) || !strings.HasSuffix(unit, ".service") {
			return config{}, fmt.Errorf("invalid SYSTEMD_SERVICES unit %q: expected a service name or template@ wildcard", unit)
		}
		if !seen[unit] {
			cfg.Services = append(cfg.Services, unit)
			seen[unit] = true
		}
	}
	cfg.Hostname, err = os.Hostname()
	if err != nil || cfg.Hostname == "" {
		return config{}, errors.New("cannot determine hostname")
	}
	if value := os.Getenv("POLL_INTERVAL"); value != "" {
		interval, err := time.ParseDuration(value)
		if err != nil || interval <= 0 {
			return config{}, fmt.Errorf("invalid POLL_INTERVAL %q", value)
		}
		cfg.PollInterval = interval
	}
	return cfg, nil
}

func newSystemdClient(units []string) *systemdClient {
	return &systemdClient{units: units, run: func(ctx context.Context, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, "systemctl", args...).Output()
	}}
}

// expandUnits discovers loaded instances afresh on every poll. Exact names are
// queried even when absent, so explicitly configured missing services still alert.
func (s *systemdClient) expandUnits(ctx context.Context) ([]string, error) {
	var units []string
	seen := make(map[string]bool)
	for _, selector := range s.units {
		matches := []string{selector}
		if strings.HasSuffix(selector, "@*.service") {
			queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			output, err := s.run(queryCtx, "--system", "--no-pager", "list-units", "--all", "--type=service", "--plain", "--no-legend", "--full", "--", selector)
			cancel()
			if err != nil {
				return nil, fmt.Errorf("discover systemd instances for %s: %w", selector, err)
			}
			matches = nil
			prefix := strings.TrimSuffix(selector, "*.service")
			for _, line := range strings.Split(string(output), "\n") {
				fields := strings.Fields(line)
				if len(fields) == 0 {
					continue
				}
				unit := fields[0]
				if len(fields) < 4 || !strings.HasPrefix(unit, prefix) || !strings.HasSuffix(unit, ".service") || unit == prefix+".service" {
					return nil, fmt.Errorf("unexpected instance row %q for %s", line, selector)
				}
				matches = append(matches, unit)
			}
		}
		for _, unit := range matches {
			if !seen[unit] {
				units = append(units, unit)
				seen[unit] = true
			}
		}
	}
	return units, nil
}

func (s *systemdClient) listServices(ctx context.Context) ([]Service, error) {
	units, err := s.expandUnits(ctx)
	if err != nil {
		return nil, err
	}
	services := make([]Service, 0, len(units))
	for _, unit := range units {
		queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		output, err := s.run(queryCtx, "--system", "--no-pager", "show", "--property=LoadState,ActiveState,SubState", "--", unit)
		cancel()
		properties := make(map[string]string)
		for _, line := range strings.Split(string(output), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if ok {
				properties[key] = value
			}
		}
		load, active, sub := properties["LoadState"], properties["ActiveState"], properties["SubState"]
		if err != nil && load != "not-found" {
			return nil, fmt.Errorf("query systemd service %s: %w", unit, err)
		}
		if load == "" || active == "" || sub == "" {
			return nil, fmt.Errorf("incomplete systemd response for %s", unit)
		}
		state := active + "/" + sub
		if load != "loaded" {
			state = load
		} else if active == "active" {
			state = "active"
		}
		services = append(services, Service{ID: unit, Name: unit, State: state})
	}
	return services, nil
}

func checkOnce(ctx context.Context, systemd serviceLister, webhookClient *http.Client, cfg config, previous State, now time.Time) (State, error) {
	services, err := systemd.listServices(ctx)
	if err != nil {
		return previous, err
	}

	next, alerts := reconcile(previous, services, now, alertAfter)
	if err := saveState(cfg.StateFile, next); err != nil {
		return previous, fmt.Errorf("save observed state: %w", err)
	}

	var errs []error
	for _, service := range alerts {
		failure := next.Services[service.ID]
		if err := sendWebhook(ctx, webhookClient, cfg.WebhookURL, cfg.Hostname, service, failure.FirstSeen, now); err != nil {
			errs = append(errs, fmt.Errorf("alert for %s: %w", service.Name, err))
			continue
		}
		next = markNotified(next, service.ID)
		if err := saveState(cfg.StateFile, next); err != nil {
			return next, fmt.Errorf("save notification state: %w", err)
		}
		log.Printf("reported service %s (%s) in state %s", service.Name, service.ID, service.State)
	}
	return next, errors.Join(errs...)
}

func reconcile(previous State, services []Service, now time.Time, threshold time.Duration) (State, []Service) {
	if previous.Services == nil {
		previous.Services = make(map[string]Failure)
	}
	next := State{Services: make(map[string]Failure)}
	var alerts []Service
	for _, service := range services {
		state := strings.ToLower(service.State)
		if state == "active" {
			continue
		}

		failure, exists := previous.Services[service.ID]
		if !exists {
			failure.FirstSeen = now
		}
		failure.Name = service.Name
		failure.State = state
		next.Services[service.ID] = failure
		if !failure.Notified && now.Sub(failure.FirstSeen) >= threshold {
			alerts = append(alerts, service)
		}
	}
	return next, alerts
}

func markNotified(state State, id string) State {
	failure, ok := state.Services[id]
	if ok {
		failure.Notified = true
		state.Services[id] = failure
	}
	return state
}

func loadState(path string) (State, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return State{Services: make(map[string]Failure)}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if state.Services == nil {
		state.Services = make(map[string]Failure)
	}
	return state, nil
}

func saveState(path string, state State) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(filepath.Dir(path), ".systemd-monitor-*.tmp")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

func sendWebhook(ctx context.Context, client *http.Client, webhookURL, hostname string, service Service, firstSeen, now time.Time) error {
	durationSeconds := int64(now.Sub(firstSeen) / time.Second)
	payload := struct {
		Incident struct {
			ID          string `json:"id"`
			Status      string `json:"status"`
			Severity    string `json:"severity"`
			Title       string `json:"title"`
			Description string `json:"description"`
			Metadata    struct {
				ServiceID       string    `json:"service_id"`
				Hostname        string    `json:"hostname"`
				ServiceName     string    `json:"service_name"`
				State           string    `json:"state"`
				FirstSeen       time.Time `json:"first_seen"`
				DurationSeconds int64     `json:"duration_seconds"`
			} `json:"metadata"`
		} `json:"incident"`
	}{}
	payload.Incident.ID = fmt.Sprintf("systemd-%s-%s-%d", hostname, service.ID, firstSeen.UnixNano())
	payload.Incident.Status = "alert"
	payload.Incident.Severity = "high"
	payload.Incident.Title = fmt.Sprintf("systemd service %s on %s is %s", service.Name, hostname, service.State)
	payload.Incident.Description = fmt.Sprintf("Service %s on %s has been non-active for at least 5 minutes (current state: %s)", service.Name, hostname, service.State)
	payload.Incident.Metadata.Hostname = hostname
	payload.Incident.Metadata.ServiceID = service.ID
	payload.Incident.Metadata.ServiceName = service.Name
	payload.Incident.Metadata.State = service.State
	payload.Incident.Metadata.FirstSeen = firstSeen
	payload.Incident.Metadata.DurationSeconds = durationSeconds
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return fmt.Errorf("webhook request failed: %v", urlErr.Err)
		}
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("webhook returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return nil
}
