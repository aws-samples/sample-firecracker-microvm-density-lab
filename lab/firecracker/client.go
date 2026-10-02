// Copyright Amazon.com, Inc. or its affiliates. All Rights Reserved.
// SPDX-License-Identifier: MIT-0
// Package firecracker is a minimal client for the Firecracker VMM HTTP API.
//
// Firecracker serves its API over a Unix domain socket rather than TCP. This client covers
// only what the density sweep needs: configure a microVM, start it, read metrics, and stop
// it. It is deliberately not a general-purpose SDK.
//
// The official firecracker-go-sdk exists and is the right choice for production code. This
// package is here so the lab has no module dependencies to vendor onto an air-gapped metal
// host, and so the exact API calls are visible in the blog rather than hidden behind a
// wrapper. Endpoint shapes follow the OpenAPI spec shipped with Firecracker v1.16.1.
package firecracker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// Client talks to one Firecracker process over its API socket.
type Client struct {
	socketPath string
	http       *http.Client
}

// New returns a client for the Firecracker API socket at path.
func New(socketPath string) *Client {
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
				// One connection per VMM is plenty and keeps the file descriptor count
				// predictable: at 180 microVMs, a pool per client would multiply.
				MaxIdleConns:    1,
				IdleConnTimeout: 30 * time.Second,
			},
			Timeout: 10 * time.Second,
		},
	}
}

// MachineConfig is the vCPU and memory configuration.
//
// SMT here refers to the *guest's* view. It is unrelated to host SMT state and can only be
// enabled on x86. The sweep leaves it false: each microVM gets one vCPU, and host-side SMT
// is what the experiment varies.
type MachineConfig struct {
	VCPUCount       int    `json:"vcpu_count"`
	MemSizeMiB      int    `json:"mem_size_mib"`
	SMT             bool   `json:"smt,omitempty"`
	CPUTemplate     string `json:"cpu_template,omitempty"`
	TrackDirtyPages bool   `json:"track_dirty_pages,omitempty"`
}

// BootSource is the guest kernel and its command line.
type BootSource struct {
	KernelImagePath string `json:"kernel_image_path"`
	BootArgs        string `json:"boot_args,omitempty"`
	InitrdPath      string `json:"initrd_path,omitempty"`
}

// Drive is a block device backing the guest.
type Drive struct {
	DriveID      string `json:"drive_id"`
	PathOnHost   string `json:"path_on_host"`
	IsRootDevice bool   `json:"is_root_device"`
	IsReadOnly   bool   `json:"is_read_only"`
}

// NetworkInterface attaches a tap device to the guest.
type NetworkInterface struct {
	IfaceID     string `json:"iface_id"`
	HostDevName string `json:"host_dev_name"`
	GuestMAC    string `json:"guest_mac,omitempty"`
}

// Vsock configures the host-guest socket channel, which is how a guest agent reports
// results without needing a network stack.
type Vsock struct {
	GuestCID int    `json:"guest_cid"`
	UDSPath  string `json:"uds_path"`
	VsockID  string `json:"vsock_id,omitempty"`
}

// Logger configures VMM log output.
type Logger struct {
	LogPath       string `json:"log_path"`
	Level         string `json:"level,omitempty"`
	ShowLevel     bool   `json:"show_level,omitempty"`
	ShowLogOrigin bool   `json:"show_log_origin,omitempty"`
}

// Metrics configures the metrics sink. Firecracker writes JSON lines to this path.
type Metrics struct {
	MetricsPath string `json:"metrics_path"`
}

// InstanceInfo is the response from GET /.
type InstanceInfo struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	VMMVersion string `json:"vmm_version"`
	AppName    string `json:"app_name"`
}

// PutMachineConfig sets vCPU count and memory size.
func (c *Client) PutMachineConfig(ctx context.Context, cfg MachineConfig) error {
	return c.put(ctx, "/machine-config", cfg)
}

// PutBootSource sets the guest kernel and boot arguments.
func (c *Client) PutBootSource(ctx context.Context, bs BootSource) error {
	return c.put(ctx, "/boot-source", bs)
}

// PutDrive attaches or replaces a block device.
func (c *Client) PutDrive(ctx context.Context, d Drive) error {
	return c.put(ctx, "/drives/"+d.DriveID, d)
}

// PutNetworkInterface attaches a tap device.
func (c *Client) PutNetworkInterface(ctx context.Context, ni NetworkInterface) error {
	return c.put(ctx, "/network-interfaces/"+ni.IfaceID, ni)
}

// PutVsock configures the vsock device.
func (c *Client) PutVsock(ctx context.Context, v Vsock) error {
	return c.put(ctx, "/vsock", v)
}

// PutLogger configures logging. The target path must already exist: Firecracker will not
// create it, and a missing path is a common cause of an otherwise silent failure.
func (c *Client) PutLogger(ctx context.Context, l Logger) error {
	return c.put(ctx, "/logger", l)
}

// PutMetrics configures the metrics sink. Like the logger path, the file or FIFO must
// already exist.
func (c *Client) PutMetrics(ctx context.Context, m Metrics) error {
	return c.put(ctx, "/metrics", m)
}

// Start boots the configured microVM.
func (c *Client) Start(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "InstanceStart"})
}

// FlushMetrics forces a metrics write. Firecracker flushes on its own schedule, so an
// explicit flush is needed to read a value at a chosen moment.
func (c *Client) FlushMetrics(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "FlushMetrics"})
}

// SendCtrlAltDel requests an orderly guest shutdown. Guests without an ACPI handler ignore
// it, so callers should treat SIGKILL of the VMM as the reliable stop and this as a
// courtesy.
func (c *Client) SendCtrlAltDel(ctx context.Context) error {
	return c.put(ctx, "/actions", map[string]string{"action_type": "SendCtrlAltDel"})
}

// Info returns instance state, and doubles as a liveness probe: a VMM wedged by the
// documented signal-handler deadlock accepts connections but stops answering.
func (c *Client) Info(ctx context.Context) (*InstanceInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, apiError(resp.StatusCode, body)
	}
	var info InstanceInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("decode instance info: %w", err)
	}
	return &info, nil
}

// WaitReady polls the API socket until the VMM answers or the context expires.
//
// The socket appears slightly before the VMM is ready to serve, so connecting is not proof
// of readiness. Callers should wait rather than assume.
func (c *Client) WaitReady(ctx context.Context) error {
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("waiting for API socket %s: %w (last error: %v)",
					c.socketPath, ctx.Err(), last)
			}
			return fmt.Errorf("waiting for API socket %s: %w", c.socketPath, ctx.Err())
		case <-ticker.C:
			probe, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
			_, err := c.Info(probe)
			cancel()
			if err == nil {
				return nil
			}
			last = err
		}
	}
}

func (c *Client) put(ctx context.Context, path string, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode %s payload: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://localhost"+path, bytes.NewReader(buf))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("PUT %s: %w", path, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	// Firecracker answers 204 for most successful mutations.
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("PUT %s: %w", path, apiError(resp.StatusCode, body))
	}
	return nil
}

// apiError extracts Firecracker's fault message, which carries the actual reason for a
// rejected request and is far more useful than the status code alone.
func apiError(status int, body []byte) error {
	var fault struct {
		FaultMessage string `json:"fault_message"`
	}
	if err := json.Unmarshal(body, &fault); err == nil && fault.FaultMessage != "" {
		return fmt.Errorf("status %d: %s", status, fault.FaultMessage)
	}
	if len(body) > 0 {
		return fmt.Errorf("status %d: %s", status, bytes.TrimSpace(body))
	}
	return fmt.Errorf("status %d", status)
}
