// Package container finds the Java process of a Pelican server container:
// Docker API (read only) for the container, /proc for the processes.
package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Errors returned by the Docker client.
var (
	ErrNotFound          = errors.New("container: not found or not a Pelican server container")
	ErrDockerUnavailable = errors.New("container: docker API unavailable")
)

// Info is the part of `docker inspect` the bridge uses.
type Info struct {
	UUID              string
	State             string // created, running, paused, restarting, removing, exited, dead
	Running           bool
	StartedAt         time.Time // zero if never started
	PID               int       // State.Pid, the first process (tini/entrypoint)
	Memory            int64
	MemoryReservation int64
	NanoCPUs          int64
	CPUQuota          int64
	CPUPeriod         int64
	CpusetCpus        string
}

// Inspector looks up server containers.
type Inspector interface {
	Inspect(ctx context.Context, uuid string) (Info, error)
}

// Docker talks to the Docker Engine API over its Unix socket, without the SDK.
type Docker struct {
	client *http.Client
}

// NewDocker returns a client for the socket at path.
func NewDocker(socket string) *Docker {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Docker{client: &http.Client{Transport: tr, Timeout: 5 * time.Second}}
}

type inspectJSON struct {
	Name  string `json:"Name"`
	State struct {
		Status    string `json:"Status"`
		Pid       int    `json:"Pid"`
		StartedAt string `json:"StartedAt"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Memory            int64  `json:"Memory"`
		MemoryReservation int64  `json:"MemoryReservation"`
		NanoCpus          int64  `json:"NanoCpus"`
		CpuQuota          int64  `json:"CpuQuota"`
		CpuPeriod         int64  `json:"CpuPeriod"`
		CpusetCpus        string `json:"CpusetCpus"`
	} `json:"HostConfig"`
}

// Inspect returns the container named uuid. Containers without the Wings
// labels Service=Pelican and ContainerType=server_process count as not found.
func (d *Docker) Inspect(ctx context.Context, uuid string) (Info, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+url.PathEscape(uuid)+"/json", nil)
	if err != nil {
		return Info{}, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return Info{}, ErrNotFound
	case resp.StatusCode != http.StatusOK:
		return Info{}, fmt.Errorf("%w: HTTP %d", ErrDockerUnavailable, resp.StatusCode)
	}
	return parseInspect(uuid, body)
}

func parseInspect(uuid string, body []byte) (Info, error) {
	var j inspectJSON
	if err := json.Unmarshal(body, &j); err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrDockerUnavailable, err)
	}
	// Docker also resolves IDs and ID prefixes; only accept the exact name.
	if strings.TrimPrefix(j.Name, "/") != uuid ||
		j.Config.Labels["Service"] != "Pelican" || j.Config.Labels["ContainerType"] != "server_process" {
		return Info{}, ErrNotFound
	}
	info := Info{
		UUID:              uuid,
		State:             j.State.Status,
		Running:           j.State.Status == "running", // Docker reports Running=true while paused
		PID:               j.State.Pid,
		Memory:            j.HostConfig.Memory,
		MemoryReservation: j.HostConfig.MemoryReservation,
		NanoCPUs:          j.HostConfig.NanoCpus,
		CPUQuota:          j.HostConfig.CpuQuota,
		CPUPeriod:         j.HostConfig.CpuPeriod,
		CpusetCpus:        j.HostConfig.CpusetCpus,
	}
	if t, err := time.Parse(time.RFC3339Nano, j.State.StartedAt); err == nil && t.Year() > 1 {
		info.StartedAt = t
	}
	return info, nil
}
