package api

import "encoding/json"

// Request is a JSON-RPC 2.0 request envelope.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response envelope.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError carries a JSON-RPC error code and message.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// PortMapSpec is the wire representation of a host-to-guest port mapping.
type PortMapSpec struct {
	HostPort  uint16 `json:"host_port"`
	GuestPort uint16 `json:"guest_port"`
	Protocol  string `json:"protocol"`
}

// VolumeMountSpec is the wire representation of a volume mount.
type VolumeMountSpec struct {
	DiskPath  string `json:"disk_path"`
	GuestPath string `json:"guest_path"`
	ReadOnly  bool   `json:"read_only,omitempty"`
}

// RunParams are the parameters for the VM.Run method.
type RunParams struct {
	ImagePath   string            `json:"image_path"`
	Memory      string            `json:"memory"`
	CPUs        int               `json:"cpus"`
	NetworkName string            `json:"network_name,omitempty"`
	PortMaps    []PortMapSpec     `json:"port_maps,omitempty"`
	Env         []string          `json:"env,omitempty"`
	Name        string            `json:"name,omitempty"`
	AutoRemove  bool              `json:"auto_remove,omitempty"`
	Volumes     []VolumeMountSpec `json:"volumes,omitempty"`
	Attach      bool              `json:"attach,omitempty"`
	IPAddress   string            `json:"ip_address,omitempty"`
	GatewayIP   string            `json:"gateway_ip,omitempty"`
	BridgeName  string            `json:"bridge_name,omitempty"`
	SubnetMask  string            `json:"subnet_mask,omitempty"`
	HealthCheck *HealthCheckSpec  `json:"health_check,omitempty"`
	Restart     *RestartSpec      `json:"restart,omitempty"`
	CPUShares   uint64            `json:"cpu_shares,omitempty"`
	MemoryMax   int64             `json:"memory_max,omitempty"`
	DiskIOPS    uint64            `json:"disk_iops,omitempty"`
	DiskBPS     int64             `json:"disk_bps,omitempty"`
}

// HealthCheckSpec is the wire representation of a health check configuration.
type HealthCheckSpec struct {
	Type     string `json:"type"`
	Port     int    `json:"port,omitempty"`
	Path     string `json:"path,omitempty"`
	Interval int    `json:"interval_seconds,omitempty"`
	Timeout  int    `json:"timeout_seconds,omitempty"`
	Retries  int    `json:"retries,omitempty"`
}

// RestartSpec is the wire representation of a restart policy.
type RestartSpec struct {
	Policy     string `json:"policy"`
	MaxRetries int    `json:"max_retries,omitempty"`
}

// StopParams are the parameters for VM.Stop.
type StopParams struct {
	// ID is the VM identifier.
	ID string `json:"id"`
	// Force skips graceful shutdown and sends SIGKILL immediately.
	Force bool `json:"force,omitempty"`
}

// SignalParams are the parameters for VM.Signal.
type SignalParams struct {
	// ID is the VM identifier.
	ID string `json:"id"`
	// Signal is the signal name (e.g. "SIGTERM") or number string (e.g. "15").
	Signal string `json:"signal"`
}

// VMInfo is the compact serialisable representation of a VM.
type VMInfo struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Image  string `json:"image"`
	Name   string `json:"name,omitempty"`
	Health string `json:"health,omitempty"`
}

// VMDetail is the full serialisable representation of a VM.
type VMDetail struct {
	ID              string            `json:"id"`
	State           string            `json:"state"`
	Image           string            `json:"image"`
	Name            string            `json:"name,omitempty"`
	Memory          string            `json:"memory"`
	CPUs            int               `json:"cpus"`
	Ports           []PortMapSpec     `json:"ports,omitempty"`
	Env             []string          `json:"env,omitempty"`
	Volumes         []VolumeMountSpec `json:"volumes,omitempty"`
	IPAddress       string            `json:"ip_address,omitempty"`
	GatewayIP       string            `json:"gateway_ip,omitempty"`
	CreatedAt       string            `json:"created_at"`
	StartedAt       *string           `json:"started_at,omitempty"`
	StoppedAt       *string           `json:"stopped_at,omitempty"`
	DaemonRecovered bool              `json:"daemon_recovered,omitempty"`
	Health          string            `json:"health,omitempty"`
	RestartCount    int               `json:"restart_count,omitempty"`
	RestartPolicy   string            `json:"restart_policy,omitempty"`
}

// LogsResponse carries the captured serial console output for a VM.
type LogsResponse struct {
	ID   string `json:"id"`
	Logs string `json:"logs"`
}

// IDParams carries a single VM identifier.
type IDParams struct {
	ID string `json:"id"`
}

// NetworkCreateParams are the parameters for Network.Create.
type NetworkCreateParams struct {
	Name   string `json:"name"`
	Subnet string `json:"subnet,omitempty"`
	Driver string `json:"driver,omitempty"`
}

// NetworkInfo is the serialisable representation of a network.
type NetworkInfo struct {
	Name      string `json:"name"`
	Driver    string `json:"driver"`
	Subnet    string `json:"subnet"`
	Gateway   string `json:"gateway"`
	Bridge    string `json:"bridge"`
	CreatedAt string `json:"created_at"`
}

// NetworkConnectParams connects a VM to a network.
type NetworkConnectParams struct {
	Network string `json:"network"`
	VMID    string `json:"vm_id"`
	IP      string `json:"ip,omitempty"`
}

// DNSResolveParams are the parameters for DNS.Resolve.
type DNSResolveParams struct {
	Name    string `json:"name"`
	Network string `json:"network,omitempty"`
}

// DNSRecord is the serialisable representation of an internal DNS record.
type DNSRecord struct {
	Name    string `json:"name"`
	Network string `json:"network"`
	IP      string `json:"ip"`
	VMID    string `json:"vm_id"`
}

// VMStatsResponse carries runtime resource usage for a VM.
type VMStatsResponse struct {
	ID         string  `json:"id"`
	State      string  `json:"state"`
	CPUPct     float64 `json:"cpu_pct"`
	MemBytes   int64   `json:"mem_bytes"`
	DiskBytes  int64   `json:"disk_bytes,omitempty"`
	NetRxBytes int64   `json:"net_rx_bytes"`
	NetTxBytes int64   `json:"net_tx_bytes"`
	Timestamp  string  `json:"timestamp"`
	Source     string  `json:"source"`
}

// NodeListResponse carries cluster member information.
type NodeListResponse struct {
	Nodes []NodeRow `json:"nodes"`
}

// NodeRow is the wire representation of a cluster member.
type NodeRow struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`
	Status   string `json:"status"`
	VMCount  int    `json:"vm_count"`
	CPUCap   int    `json:"cpu_capacity"`
	MemCap   int64  `json:"mem_capacity_bytes"`
	LastSeen string `json:"last_seen"`
}
