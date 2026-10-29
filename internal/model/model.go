package model

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var Version = "1.3.0"

var accessPathPattern = regexp.MustCompile(`^/[A-Za-z0-9][A-Za-z0-9_-]{15,95}$`)

// ValidateAccessPath 只允许单段入口；空值用于兼容旧部署。
func ValidateAccessPath(value string) error {
	if value != "" && !accessPathPattern.MatchString(value) {
		return fmt.Errorf("管理路径必须是以 / 开头、长度 16–96 的单段字母数字路径，可包含 - 和 _")
	}
	return nil
}

// NewAccessPath 使用 128 位系统随机数生成独立管理入口。
func NewAccessPath() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("生成随机管理路径失败：%w", err)
	}
	return "/ns-" + hex.EncodeToString(value[:]), nil
}

type Node struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	PublicIP         string `json:"public_ip"`
	PrivateIP        string `json:"private_ip"`
	PublicInterface  string `json:"public_interface"`
	PrivateInterface string `json:"private_interface"`
	AgentURL         string `json:"agent_url,omitempty"`
	ReservedPorts    []int  `json:"reserved_ports"`
}

type Mapping struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	SourceNode string    `json:"source_node"`
	TargetNode string    `json:"target_node"`
	Protocol   string    `json:"protocol"`
	ListenPort string    `json:"listen_port"`
	TargetPort string    `json:"target_port"`
	Enabled    bool      `json:"enabled"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type AgentRule struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	ListenPort string `json:"listen_port"`
	TargetPort string `json:"target_port"`
	TargetIP   string `json:"target_ip"`
	Enabled    bool   `json:"enabled"`
}

type ApplyRequest struct {
	Revision       uint64      `json:"revision"`
	Rules          []AgentRule `json:"rules"`
	AllowedTargets *[]string   `json:"allowed_targets,omitempty"`
}

type ApplyResult struct {
	Revision  uint64    `json:"revision"`
	AppliedAt time.Time `json:"applied_at"`
}

type Listener struct {
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
	Port     int    `json:"port"`
	Process  string `json:"process,omitempty"`
}

type InterfaceStats struct {
	Name    string `json:"name"`
	RXBytes uint64 `json:"rx_bytes"`
	TXBytes uint64 `json:"tx_bytes"`
}

type AgentStatus struct {
	NodeID           string           `json:"node_id"`
	Version          string           `json:"version"`
	Revision         uint64           `json:"revision"`
	SavedRevision    uint64           `json:"saved_revision"`
	AppliedAt        time.Time        `json:"applied_at"`
	Forwarding       bool             `json:"forwarding"`
	Rules            []AgentRule      `json:"rules"`
	Listeners        []Listener       `json:"listeners"`
	Interfaces       []InterfaceStats `json:"interfaces"`
	UptimeSeconds    int64            `json:"uptime_seconds"`
	Warnings         []string         `json:"warnings"`
	AutomaticTargets bool             `json:"automatic_targets"`
	AllowedTargets   []string         `json:"allowed_targets,omitempty"`
}

type NodeStatus struct {
	Node
	Online    bool         `json:"online"`
	Error     string       `json:"error,omitempty"`
	LatencyMS int64        `json:"latency_ms"`
	Status    *AgentStatus `json:"status,omitempty"`
}

type ProbeRequest struct {
	TargetIP   string `json:"target_ip"`
	TargetPort string `json:"target_port"`
	Protocol   string `json:"protocol"`
}

type ProbeResult struct {
	Reachable bool   `json:"reachable"`
	LatencyMS int64  `json:"latency_ms"`
	Message   string `json:"message"`
	Interface string `json:"interface"`
}

type AuditEvent struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Action  string    `json:"action"`
	Detail  string    `json:"detail"`
	Success bool      `json:"success"`
}

type PortRange struct{ Start, End int }

func ParsePorts(value string) (PortRange, error) {
	parts := strings.Split(strings.TrimSpace(value), "-")
	if len(parts) < 1 || len(parts) > 2 {
		return PortRange{}, fmt.Errorf("端口格式应为 8080 或 8080-8090")
	}
	a, err := strconv.Atoi(parts[0])
	if err != nil || a < 1 || a > 65535 {
		return PortRange{}, fmt.Errorf("端口必须在 1–65535 之间")
	}
	b := a
	if len(parts) == 2 {
		b, err = strconv.Atoi(parts[1])
		if err != nil || b < a || b > 65535 {
			return PortRange{}, fmt.Errorf("端口范围无效")
		}
	}
	if b-a > 999 {
		return PortRange{}, fmt.Errorf("单条映射最多包含 1000 个端口")
	}
	return PortRange{a, b}, nil
}

func (p PortRange) String() string {
	if p.Start == p.End {
		return strconv.Itoa(p.Start)
	}
	return fmt.Sprintf("%d-%d", p.Start, p.End)
}
func (p PortRange) Contains(port int) bool { return port >= p.Start && port <= p.End }
func (p PortRange) Overlaps(other PortRange) bool {
	return p.Start <= other.End && other.Start <= p.End
}
func Protocols(value string) []string {
	if value == "both" {
		return []string{"tcp", "udp"}
	}
	return []string{value}
}
func ProtocolsOverlap(a, b string) bool { return a == b || a == "both" || b == "both" }

var identifier = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)
var ifaceName = regexp.MustCompile(`^[a-zA-Z0-9_.:-]{1,32}$`)

func ValidID(value string) bool        { return identifier.MatchString(value) }
func ValidInterface(value string) bool { return ifaceName.MatchString(value) }

func ValidatePorts(protocol, listen, target string) error {
	if protocol != "tcp" && protocol != "udp" && protocol != "both" {
		return fmt.Errorf("协议必须为 TCP、UDP 或 TCP + UDP")
	}
	a, err := ParsePorts(listen)
	if err != nil {
		return err
	}
	b, err := ParsePorts(target)
	if err != nil {
		return err
	}
	if a.End-a.Start != b.End-b.Start {
		return fmt.Errorf("入口与目标的端口数量必须一致")
	}
	return nil
}

func ValidateNode(n Node) error {
	if !ValidID(n.ID) || strings.TrimSpace(n.Name) == "" {
		return fmt.Errorf("机器 ID 或名称无效")
	}
	pub, e := netip.ParseAddr(n.PublicIP)
	if e != nil || !pub.Is4() || pub.IsUnspecified() || pub.IsMulticast() || pub.IsLoopback() {
		return fmt.Errorf("机器公网 IPv4 无效")
	}
	priv, e := netip.ParseAddr(n.PrivateIP)
	if e != nil || !priv.Is4() || !priv.IsPrivate() {
		return fmt.Errorf("机器私网 IPv4 无效")
	}
	if !ValidInterface(n.PublicInterface) || !ValidInterface(n.PrivateInterface) || n.PublicInterface == n.PrivateInterface {
		return fmt.Errorf("公网与私网网卡配置无效")
	}
	return nil
}
