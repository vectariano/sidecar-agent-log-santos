package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
)

const (
	defaultBatchSize     = 5
	defaultFlushInterval = 2 * time.Second
	defaultHTTPTimeout   = 10 * time.Second
)

type Config struct {
	LogEndpoint   string
	AuthToken     string
	Services      map[string]struct{}
	BatchSize     int
	FlushInterval time.Duration
}

type LogEvent struct {
	ServiceID string                 `json:"service_id"`
	Timestamp string                 `json:"@timestamp"`
	Level     string                 `json:"level"`
	Message   string                 `json:"message"`
	TraceID   string                 `json:"trace_id,omitempty"`
	SpanID    string                 `json:"span_id,omitempty"`
	Attrs     map[string]interface{} `json:"attrs,omitempty"`
}

type BatchRequest struct {
	Logs []LogEvent `json:"logs"`
}

type Sidecar struct {
	cfg        Config
	docker     *client.Client
	httpClient *http.Client

	eventsCh chan LogEvent

	mu       sync.Mutex
	attached map[string]context.CancelFunc
}

func NewConfig() (Config, error) {
	endpoint := strings.TrimSpace(os.Getenv("LOG_ENDPOINT"))
	token := strings.TrimSpace(os.Getenv("AUTH_TOKEN"))
	servicesRaw := strings.TrimSpace(os.Getenv("LOG_SERVICES"))

	if endpoint == "" {
		return Config{}, errors.New("LOG_ENDPOINT is required")
	}
	if token == "" {
		return Config{}, errors.New("AUTH_TOKEN is required")
	}
	if servicesRaw == "" {
		return Config{}, errors.New("LOG_SERVICES is required")
	}

	services := make(map[string]struct{})
	for _, item := range strings.Split(servicesRaw, ",") {
		name := strings.TrimSpace(item)
		if name == "" {
			continue
		}
		services[name] = struct{}{}
	}

	if len(services) == 0 {
		return Config{}, errors.New("LOG_SERVICES contains no valid service names")
	}

	return Config{
		LogEndpoint:   endpoint,
		AuthToken:     token,
		Services:      services,
		BatchSize:     defaultBatchSize,
		FlushInterval: defaultFlushInterval,
	}, nil
}

func NewSidecar(cfg Config) (*Sidecar, error) {
	dockerCli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	return &Sidecar{
		cfg:    cfg,
		docker: dockerCli,
		httpClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
		eventsCh: make(chan LogEvent, 4096),
		attached: make(map[string]context.CancelFunc),
	}, nil
}

func (s *Sidecar) Run(ctx context.Context) error {
	if _, err := s.docker.Ping(ctx); err != nil {
		return fmt.Errorf("docker ping failed: %w", err)
	}
	log.Println("docker connection established")

	go s.runSender(ctx)

	if err := s.attachToExistingContainers(ctx); err != nil {
		return fmt.Errorf("attach existing containers: %w", err)
	}

	go s.watchDockerEvents(ctx)

	<-ctx.Done()
	log.Println("sidecar stopped")
	return nil
}

func (s *Sidecar) attachToExistingContainers(ctx context.Context) error {
	containers, err := s.docker.ContainerList(ctx, container.ListOptions{
		All: false,
	})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}

	for _, c := range containers {
		serviceName := getComposeServiceName(c.Labels)
		if !s.isTargetService(serviceName) {
			continue
		}
		s.attachContainer(ctx, c.ID)
	}

	return nil
}

func (s *Sidecar) watchDockerEvents(ctx context.Context) {
	filterArgs := filters.NewArgs()
	filterArgs.Add("type", "container")
	filterArgs.Add("event", "start")
	filterArgs.Add("event", "restart")
	filterArgs.Add("event", "die")
	filterArgs.Add("event", "destroy")

	msgCh, errCh := s.docker.Events(ctx, events.ListOptions{
		Filters: filterArgs,
	})

	for {
		select {
		case <-ctx.Done():
			return

		case err := <-errCh:
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("docker events error: %v", err)
				time.Sleep(2 * time.Second)
			}

		case msg := <-msgCh:
			serviceName := msg.Actor.Attributes["com.docker.compose.service"]
			if !s.isTargetService(serviceName) {
				continue
			}

			switch msg.Action {
			case "start", "restart":
				s.attachContainer(ctx, msg.ID)
			case "die", "destroy":
				s.detachContainer(msg.ID)
			}
		}
	}
}

func (s *Sidecar) attachContainer(parent context.Context, containerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.attached[containerID]; exists {
		return
	}

	ctx, cancel := context.WithCancel(parent)
	s.attached[containerID] = cancel

	go func() {
		defer s.detachContainer(containerID)

		if err := s.streamContainerLogs(ctx, containerID); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("stream logs failed for container=%s: %v", shortID(containerID), err)
		}
	}()
}

func (s *Sidecar) detachContainer(containerID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cancel, exists := s.attached[containerID]
	if !exists {
		return
	}

	cancel()
	delete(s.attached, containerID)
}

func (s *Sidecar) streamContainerLogs(ctx context.Context, containerID string) error {
	inspect, err := s.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return fmt.Errorf("inspect container: %w", err)
	}

	serviceName := getComposeServiceName(inspect.Config.Labels)
	if !s.isTargetService(serviceName) {
		return nil
	}

	containerName := strings.TrimPrefix(inspect.Name, "/")
	imageName := inspect.Config.Image
	composeProject := inspect.Config.Labels["com.docker.compose.project"]

	log.Printf("attach logs: service=%s container=%s", serviceName, containerName)

	reader, err := s.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
		Timestamps: true,
		Tail:       "0",
	})
	if err != nil {
		return fmt.Errorf("container logs: %w", err)
	}
	defer reader.Close()

	if inspect.Config.Tty {
		return s.scanStream(ctx, reader, "stdout", serviceName, containerName, containerID, imageName, composeProject)
	}

	stdoutReader, stdoutWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		defer stdoutWriter.Close()
		defer stderrWriter.Close()

		if _, err := stdcopy.StdCopy(stdoutWriter, stderrWriter, reader); err != nil &&
			!errors.Is(err, io.EOF) &&
			!errors.Is(err, context.Canceled) {
			log.Printf("stdcopy error for container=%s: %v", shortID(containerID), err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := s.scanStream(ctx, stdoutReader, "stdout", serviceName, containerName, containerID, imageName, composeProject); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Printf("stdout scan error for container=%s: %v", shortID(containerID), err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := s.scanStream(ctx, stderrReader, "stderr", serviceName, containerName, containerID, imageName, composeProject); err != nil &&
			!errors.Is(err, context.Canceled) {
			log.Printf("stderr scan error for container=%s: %v", shortID(containerID), err)
		}
	}()

	wg.Wait()
	return nil
}

func (s *Sidecar) scanStream(
	ctx context.Context,
	r io.Reader,
	stream string,
	serviceName string,
	containerName string,
	containerID string,
	imageName string,
	composeProject string,
) error {
	scanner := bufio.NewScanner(r)

	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line := scanner.Text()
		event := buildLogEvent(
			line,
			stream,
			serviceName,
			containerName,
			containerID,
			imageName,
			composeProject,
		)

		select {
		case s.eventsCh <- event:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scanner error: %w", err)
	}
	return nil
}

func (s *Sidecar) runSender(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.FlushInterval)
	defer ticker.Stop()

	batch := make([]LogEvent, 0, s.cfg.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}

		toSend := append([]LogEvent(nil), batch...)
		batch = batch[:0]

		if err := s.sendBatch(ctx, toSend); err != nil {
			log.Printf("send batch failed: %v", err)
		}
	}

	for {
		select {
		case <-ctx.Done():
			flush()
			return

		case event := <-s.eventsCh:
			batch = append(batch, event)
			if len(batch) >= s.cfg.BatchSize {
				flush()
			}

		case <-ticker.C:
			flush()
		}
	}
}

func (s *Sidecar) sendBatch(ctx context.Context, logs []LogEvent) error {
	payload := BatchRequest{Logs: logs}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal batch: %w", err)
	}

	prettyBody, _ := json.MarshalIndent(payload, "", "  ")
	log.Printf("sending payload:\n%s", string(prettyBody))

	var lastErr error
	backoff := time.Second

	for attempt := 1; attempt <= 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.LogEndpoint, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("create request: %w", err)
		}

		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+s.cfg.AuthToken)

		resp, err := s.httpClient.Do(req)
		if err == nil && resp != nil {
			defer resp.Body.Close()
		}

		if err == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			log.Printf("sent batch: %d events", len(logs))
			return nil
		}

		if err != nil {
			lastErr = err
		} else {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 16*1024))
			lastErr = fmt.Errorf("unexpected status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(respBody)))
		}

		time.Sleep(backoff)
		backoff *= 2
	}

	return fmt.Errorf("send batch failed after retries: %w", lastErr)
}

func buildLogEvent(
	line string,
	stream string,
	serviceName string,
	containerName string,
	containerID string,
	imageName string,
	composeProject string,
) LogEvent {
	ts, payload := splitDockerTimestamp(line)
	if ts.IsZero() {
		ts = time.Now().UTC()
	}

	event := LogEvent{
		ServiceID: serviceName,
		Timestamp: formatTimestamp(ts),
		Level:     strings.ToUpper(defaultLevelByStream(stream)),
		Message:   payload,
	}

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(payload), &raw); err != nil {
		return event
	}

	if v, ok := raw["ts"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			event.Timestamp = formatTimestamp(parsed)
		}
	} else if v, ok := raw["timestamp"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			event.Timestamp = formatTimestamp(parsed)
		}
	} else if v, ok := raw["@timestamp"].(string); ok && v != "" {
		event.Timestamp = v
	} else if v, ok := raw["time"].(string); ok {
		if parsed, err := time.Parse(time.RFC3339Nano, v); err == nil {
			event.Timestamp = formatTimestamp(parsed)
		}
	}

	if v, ok := raw["level"].(string); ok && v != "" {
		event.Level = strings.ToUpper(v)
	} else if v, ok := raw["severity"].(string); ok && v != "" {
		event.Level = strings.ToUpper(v)
	}

	if v, ok := raw["message"].(string); ok && v != "" {
		event.Message = v
	} else if v, ok := raw["msg"].(string); ok && v != "" {
		event.Message = v
	} else if v, ok := raw["log"].(string); ok && v != "" {
		event.Message = v
	}

	if v, ok := raw["trace_id"].(string); ok && v != "" {
		event.TraceID = v
	}
	if v, ok := raw["span_id"].(string); ok && v != "" {
		event.SpanID = v
	}

	var attrs map[string]interface{}

	if v, ok := raw["attrs"].(map[string]interface{}); ok && v != nil {
		attrs = make(map[string]interface{})
		for key, value := range v {
			attrs[key] = value
		}

		attrs["container_id"] = containerID
		attrs["container_name"] = containerName
		attrs["image"] = imageName
		attrs["compose_project"] = composeProject
		attrs["stream"] = stream
	}

	event.Attrs = attrs
	return event
}

func formatTimestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func splitDockerTimestamp(line string) (time.Time, string) {
	line = strings.TrimRight(line, "\r\n")

	parts := strings.SplitN(line, " ", 2)
	if len(parts) != 2 {
		return time.Time{}, line
	}

	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, line
	}

	return ts, parts[1]
}

func getComposeServiceName(labels map[string]string) string {
	return labels["com.docker.compose.service"]
}

func (s *Sidecar) isTargetService(serviceName string) bool {
	if serviceName == "" {
		return false
	}
	_, ok := s.cfg.Services[serviceName]
	return ok
}

func defaultLevelByStream(stream string) string {
	if stream == "stderr" {
		return "ERROR"
	}
	return "INFO"
}

func shortID(containerID string) string {
	if len(containerID) > 12 {
		return containerID[:12]
	}
	return containerID
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	cfg, err := NewConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	sidecar, err := NewSidecar(cfg)
	if err != nil {
		log.Fatalf("init sidecar error: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := sidecar.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatalf("run error: %v", err)
	}
}
