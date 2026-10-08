package arcade

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var engineInspector = struct {
	sync.RWMutex
	client *http.Client
	base   string
}{}

// The supported deployment owns local bind-mounted worlds and a local Docker
// socket. Use typed HTTP status for daemon observations on that boundary. CLI
// compatibility remains for standalone embedders that do not configure Run.
func configureEngineInspector(ctx context.Context) error {
	endpoint := os.Getenv("DOCKER_HOST")
	if endpoint == "" {
		out, err := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}").Output()
		if err != nil {
			return nil
		}
		endpoint = strings.TrimSpace(string(out))
	}
	if !strings.HasPrefix(endpoint, "unix://") {
		return fmt.Errorf("Docker endpoint must be a local unix socket for managed world bind mounts")
	}
	socket := strings.TrimPrefix(endpoint, "unix://")
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	engineInspector.Lock()
	engineInspector.client = &http.Client{Transport: transport, Timeout: 10 * time.Second}
	engineInspector.base = "http://docker"
	engineInspector.Unlock()
	refreshDaemonCapacity(ctx)
	return nil
}

func inspectEngineContainer(ctx context.Context, client *http.Client, base, id string) ContainerState {
	req, err := http.NewRequestWithContext(ctx, "GET", base+"/containers/"+url.PathEscape(containerPrefix+"-"+id)+"/json", nil)
	if err != nil {
		return ContainerUnknown
	}
	response, err := client.Do(req)
	if err != nil {
		return ContainerUnknown
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return ContainerMissing
	}
	if response.StatusCode != http.StatusOK {
		return ContainerUnknown
	}
	var body struct{ State *struct{ Running *bool } }
	d := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if d.Decode(&body) != nil || body.State == nil || body.State.Running == nil {
		return ContainerUnknown
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return ContainerUnknown
	}
	if *body.State.Running {
		return ContainerRunning
	}
	return ContainerStopped
}

// Recover actual daemon bindings, including extras absent from persisted
// desired configuration. Legacy containers require exact image and mount
// identity; newly created ones additionally carry a server ownership label.
func (m *Manager) recoverContainerIdentity(s *Server) error {
	out, err := dockerOutput(m.ctx, 10*time.Second, false, "inspect", "--type", "container", containerPrefix+"-"+s.ID)
	if err != nil {
		return fmt.Errorf("container identity unavailable: %w", err)
	}
	var rows []struct {
		Config struct {
			Image  string
			Labels map[string]string
		}
		Mounts          []struct{ Type, Source, Destination string }
		NetworkSettings struct {
			Ports map[string][]struct{ HostIP, HostPort string }
		}
	}
	if json.Unmarshal(out, &rows) != nil || len(rows) != 1 {
		return fmt.Errorf("invalid container identity")
	}
	row := rows[0]
	label := row.Config.Labels["io.teploy.arcade.server"]
	if label != "" && label != s.ID {
		return fmt.Errorf("container ownership label differs")
	}
	expected := hostPathFor(m.serverDir(s))
	if p, err := filepath.EvalSymlinks(expected); err == nil {
		expected = p
	}
	mounted := false
	for _, mount := range row.Mounts {
		if mount.Type == "bind" && mount.Source == expected && mount.Destination == serverDataPath(s) {
			mounted = true
		}
	}
	if !mounted || row.Config.Image != s.Image {
		return fmt.Errorf("container image or managed world mount differs; explicit recovery required")
	}
	var actual []portBinding
	for key, bindings := range row.NetworkSettings.Ports {
		_, proto := parseBindingNumber(key)
		for _, b := range bindings {
			port := atoi(b.HostPort)
			if port < 1 || port > 65535 || (proto != "tcp" && proto != "udp") {
				return fmt.Errorf("invalid published binding")
			}
			actual = append(actual, portBinding{port, proto})
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, other := range m.servers {
		if other != s && bindingConflict(actual, serverBindings(other)) {
			return fmt.Errorf("live container binding conflicts with %s", other.ID)
		}
	}
	s.mu.Lock()
	s.DynamicBindings = actual
	s.mu.Unlock()
	return nil
}

var daemonCapacity = struct {
	sync.RWMutex
	CPUs      int
	MemoryMB  int64
	SampledAt time.Time
}{}

func refreshDaemonCapacity(ctx context.Context) {
	out, err := dockerOutput(ctx, 10*time.Second, false, "info", "--format", "{{json .}}")
	var info struct {
		NCPU     int
		MemTotal int64
	}
	if err != nil || json.Unmarshal(out, &info) != nil {
		return
	}
	daemonCapacity.Lock()
	daemonCapacity.CPUs = info.NCPU
	daemonCapacity.MemoryMB = info.MemTotal / (1 << 20)
	daemonCapacity.SampledAt = time.Now()
	daemonCapacity.Unlock()
}
func checkFitsDaemon(runtime string, memMB int, cpu float64) error {
	if runtime != RuntimeDocker {
		return nil
	}
	daemonCapacity.RLock()
	defer daemonCapacity.RUnlock()
	if daemonCapacity.MemoryMB > 0 && int64(memMB) > daemonCapacity.MemoryMB {
		return fmt.Errorf("requested memory exceeds Docker daemon capacity (%d MiB)", daemonCapacity.MemoryMB)
	}
	if daemonCapacity.CPUs > 0 && cpu > float64(daemonCapacity.CPUs) {
		return fmt.Errorf("requested CPU exceeds Docker daemon capacity (%d cores)", daemonCapacity.CPUs)
	}
	return nil
}
func daemonCapacityView() map[string]any {
	daemonCapacity.RLock()
	defer daemonCapacity.RUnlock()
	return map[string]any{"known": daemonCapacity.CPUs > 0 && daemonCapacity.MemoryMB > 0, "total_vcpu": daemonCapacity.CPUs, "total_mb": daemonCapacity.MemoryMB, "scope": "docker_daemon", "source": "Docker Engine info", "sampled_at": daemonCapacity.SampledAt}
}
