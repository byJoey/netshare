package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"netshare/internal/model"
)

func renderRules(node model.Node, revision uint64, rules []model.AgentRule) []byte {
	var b strings.Builder
	b.WriteString("add table ip netshare\nflush table ip netshare\n")
	b.WriteString("add chain ip netshare prerouting { type nat hook prerouting priority dstnat; policy accept; }\n")
	b.WriteString("add chain ip netshare postrouting { type nat hook postrouting priority srcnat; policy accept; }\n")
	fmt.Fprintf(&b, "add rule ip netshare prerouting counter comment \"netshare:revision:%d\"\n", revision)
	for _, r := range canonicalRules(rules) {
		if !r.Enabled {
			continue
		}
		ports, _ := model.ParsePorts(r.ListenPort)
		target, _ := model.ParsePorts(r.TargetPort)
		for _, protocol := range model.Protocols(r.Protocol) {
			for port := ports.Start; port <= ports.End; port++ {
				targetPort := target.Start + (port - ports.Start)
				fmt.Fprintf(&b, "add rule ip netshare prerouting iifname %q ip daddr %s %s dport %d counter dnat to %s:%d comment \"netshare:%s:%s:%d:d\"\n", node.PublicInterface, node.PublicIP, protocol, port, r.TargetIP, targetPort, r.ID, protocol, port)
				fmt.Fprintf(&b, "add rule ip netshare postrouting iifname %q oifname %q ip daddr %s %s dport %d ct status dnat ct original ip daddr %s ct original proto-dst %d counter snat to %s comment \"netshare:%s:%s:%d:s\"\n", node.PublicInterface, node.PrivateInterface, r.TargetIP, protocol, targetPort, node.PublicIP, port, node.PrivateIP, r.ID, protocol, port)
			}
		}
	}
	return []byte(b.String())
}

func expectedComments(revision uint64, rules []model.AgentRule) map[string]bool {
	result := map[string]bool{fmt.Sprintf("netshare:revision:%d", revision): true}
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		p, _ := model.ParsePorts(r.ListenPort)
		for _, protocol := range model.Protocols(r.Protocol) {
			for port := p.Start; port <= p.End; port++ {
				result[fmt.Sprintf("netshare:%s:%s:%d:d", r.ID, protocol, port)] = true
				result[fmt.Sprintf("netshare:%s:%s:%d:s", r.ID, protocol, port)] = true
			}
		}
	}
	return result
}

type nftSnapshot struct {
	Nftables []struct {
		Rule *struct {
			Comment string `json:"comment"`
		} `json:"rule,omitempty"`
	} `json:"nftables"`
}

func snapshotComments(data []byte) (map[string]bool, error) {
	var parsed nftSnapshot
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, fmt.Errorf("无法解析 nftables 状态：%w", err)
	}
	result := map[string]bool{}
	for _, entry := range parsed.Nftables {
		if entry.Rule != nil {
			result[entry.Rule.Comment] = true
		}
	}
	return result, nil
}

func (s *Server) tableComments(ctx context.Context) (map[string]bool, error) {
	data, err := s.command(ctx, "nft", []string{"-j", "list", "table", "ip", "netshare"}, nil)
	if err != nil {
		return nil, err
	}
	return snapshotComments(data)
}

func (s *Server) ensureOwnership(ctx context.Context) error {
	data, err := s.command(ctx, "nft", []string{"-j", "list", "table", "ip", "netshare"}, nil)
	if err != nil {
		if strings.Contains(string(data), "No such file or directory") || strings.Contains(string(data), "does not exist") {
			return nil
		}
		return fmt.Errorf("无法确认 NetShare 规则表状态：%w", err)
	}
	comments, err := snapshotComments(data)
	if err != nil {
		return err
	}
	for comment := range comments {
		if strings.HasPrefix(comment, "netshare:revision:") {
			return nil
		}
	}
	return fmt.Errorf("发现不属于 NetShare 的同名 nftables 表，已停止操作")
}

func (s *Server) checkAddresses(ctx context.Context) error {
	for _, item := range []struct{ iface, ip string }{{s.cfg.Node.PublicInterface, s.cfg.Node.PublicIP}, {s.cfg.Node.PrivateInterface, s.cfg.Node.PrivateIP}} {
		data, err := s.command(ctx, "ip", []string{"-j", "-4", "address", "show", "dev", item.iface}, nil)
		if err != nil {
			return err
		}
		var entries []struct {
			AddrInfo []struct {
				Local string `json:"local"`
			} `json:"addr_info"`
		}
		if err = json.Unmarshal(data, &entries); err != nil {
			return fmt.Errorf("网卡地址输出无效：%w", err)
		}
		found := false
		for _, entry := range entries {
			for _, addr := range entry.AddrInfo {
				if addr.Local == item.ip {
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("网卡 %s 上没有配置的地址 %s，已停止应用", item.iface, item.ip)
		}
	}
	return nil
}

func (s *Server) checkRoute(ctx context.Context, targetIP string) error {
	data, err := s.command(ctx, "ip", []string{"-j", "-4", "route", "get", targetIP, "from", s.cfg.Node.PrivateIP}, nil)
	if err != nil {
		return err
	}
	var routes []struct {
		Dev     string `json:"dev"`
		Prefsrc string `json:"prefsrc"`
		Src     string `json:"src"`
		Type    string `json:"type"`
	}
	if err = json.Unmarshal(data, &routes); err != nil || len(routes) == 0 {
		return fmt.Errorf("无法解析到 %s 的私网路由", targetIP)
	}
	route := routes[0]
	if route.Dev != s.cfg.Node.PrivateInterface || route.Type == "unreachable" || route.Type == "blackhole" || route.Type == "prohibit" {
		return bad(409, "到 %s 的路由没有走私网网卡 %s", targetIP, s.cfg.Node.PrivateInterface)
	}
	if route.Prefsrc != "" && route.Prefsrc != s.cfg.Node.PrivateIP {
		return bad(409, "到目标的源地址与本机私网地址不符")
	}
	if route.Src != "" && route.Src != s.cfg.Node.PrivateIP {
		return bad(409, "到目标的源地址与本机私网地址不符")
	}
	return nil
}

var processName = regexp.MustCompile(`users:\(\("([^"\n]+)"`)

func parseListeners(data []byte) ([]model.Listener, error) {
	result := []model.Listener{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, fmt.Errorf("无法解析系统监听端口")
		}
		protocol := fields[0]
		if protocol != "tcp" && protocol != "udp" {
			continue
		}
		local := fields[4]
		separator := strings.LastIndex(local, ":")
		if separator < 0 {
			return nil, fmt.Errorf("无法解析监听地址")
		}
		address := strings.Trim(local[:separator], "[]")
		if percent := strings.Index(address, "%"); percent >= 0 {
			address = address[:percent]
		}
		port, err := strconv.Atoi(local[separator+1:])
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		name := "系统服务"
		if match := processName.FindStringSubmatch(line); len(match) > 1 {
			name = match[1]
		}
		result = append(result, model.Listener{Protocol: protocol, Address: address, Port: port, Process: name})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Port == result[j].Port {
			return result[i].Protocol < result[j].Protocol
		}
		return result[i].Port < result[j].Port
	})
	return result, nil
}

func (s *Server) listeners(ctx context.Context) ([]model.Listener, error) {
	data, err := s.command(ctx, "ss", []string{"-H", "-lntup"}, nil)
	if err != nil {
		return nil, err
	}
	return parseListeners(data)
}

func (s *Server) Status(ctx context.Context) (model.AgentStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := model.AgentStatus{NodeID: s.cfg.Node.ID, AutomaticTargets: true, AllowedTargets: s.allowedTargets(), Version: model.Version, Revision: s.state.Revision, SavedRevision: s.state.Revision, AppliedAt: s.state.AppliedAt, Rules: append([]model.AgentRule{}, s.state.Rules...), Listeners: []model.Listener{}, Interfaces: []model.InterfaceStats{}, Warnings: []string{}, UptimeSeconds: int64(time.Since(s.started).Seconds())}
	if s.restoreWarning != "" {
		status.Warnings = append(status.Warnings, s.restoreWarning)
	}
	listeners, err := s.listeners(ctx)
	if err != nil {
		status.Warnings = append(status.Warnings, "监听端口读取失败："+err.Error())
	} else {
		status.Listeners = listeners
	}
	forward, err := s.readFile(s.forwardFile)
	if err != nil {
		status.Warnings = append(status.Warnings, "IPv4 转发状态读取失败")
	} else {
		status.Forwarding = strings.TrimSpace(string(forward)) == "1"
	}
	if hasEnabled(s.state.Rules) && !status.Forwarding {
		status.Warnings = append(status.Warnings, "IPv4 转发已关闭，映射无法接收新连接")
	}
	comments, err := s.tableComments(ctx)
	if err != nil || !sameComments(comments, expectedComments(s.state.Revision, s.state.Rules)) {
		status.Revision = 0
		status.Rules = []model.AgentRule{}
		status.AppliedAt = time.Time{}
		status.Warnings = append(status.Warnings, "内核中的 NetShare 规则与保存配置不一致，请重新同步")
	}
	for _, iface := range []string{s.cfg.Node.PublicInterface, s.cfg.Node.PrivateInterface} {
		stats := model.InterfaceStats{Name: iface}
		for _, item := range []struct {
			name  string
			value *uint64
		}{{"rx_bytes", &stats.RXBytes}, {"tx_bytes", &stats.TXBytes}} {
			data, e := s.readFile("/sys/class/net/" + iface + "/statistics/" + item.name)
			if e != nil {
				status.Warnings = append(status.Warnings, "无法读取网卡 "+iface+" 的计数")
				continue
			}
			value, e := strconv.ParseUint(strings.TrimSpace(string(data)), 10, 64)
			if e != nil {
				status.Warnings = append(status.Warnings, "网卡计数格式无效")
				continue
			}
			*item.value = value
		}
		status.Interfaces = append(status.Interfaces, stats)
	}
	return status, nil
}

func sameComments(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range b {
		if !a[k] {
			return false
		}
	}
	return true
}

func (s *Server) Probe(ctx context.Context, req model.ProbeRequest) (model.ProbeResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.allowed[req.TargetIP] {
		return model.ProbeResult{}, bad(400, "检查目标不在本节点的私网白名单中")
	}
	if req.Protocol != "tcp" && req.Protocol != "udp" && req.Protocol != "both" {
		return model.ProbeResult{}, bad(400, "检查协议无效")
	}
	p, err := model.ParsePorts(req.TargetPort)
	if err != nil || p.Start != p.End {
		return model.ProbeResult{}, bad(400, "连通性检查一次只支持一个有效目标端口")
	}
	if err = s.checkRoute(ctx, req.TargetIP); err != nil {
		return model.ProbeResult{}, err
	}
	result := model.ProbeResult{Interface: s.cfg.Node.PrivateInterface}
	if req.Protocol == "udp" {
		result.Message = "UDP 没有握手，私网路由有效；请使用真实客户端验证收发"
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dialer := net.Dialer{Timeout: 2 * time.Second, LocalAddr: &net.TCPAddr{IP: net.ParseIP(s.cfg.Node.PrivateIP)}}
	started := time.Now()
	conn, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(req.TargetIP, strconv.Itoa(p.Start)))
	result.LatencyMS = time.Since(started).Milliseconds()
	if err != nil {
		result.Message = "私网路由有效，但目标 TCP 端口无法连接：" + err.Error()
		return result, nil
	}
	_ = conn.Close()
	result.Reachable = true
	result.Message = "已通过私网连接目标 TCP 端口"
	if req.Protocol == "both" {
		result.Message += "；UDP 仍需真实客户端验证"
	}
	return result, nil
}

type forwardSnapshot struct {
	runtime []byte
	sysctl  []byte
	existed bool
}

func (s *Server) enableForwarding() (*forwardSnapshot, error) {
	runtime, err := s.readFile(s.forwardFile)
	if err != nil {
		return nil, fmt.Errorf("读取 IPv4 转发状态失败：%w", err)
	}
	value := strings.TrimSpace(string(runtime))
	if value != "0" && value != "1" {
		return nil, fmt.Errorf("IPv4 转发状态无效")
	}
	old, err := s.readFile(s.sysctlFile)
	existed := err == nil
	if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取转发持久配置失败：%w", err)
	}
	if existed && !strings.Contains(string(old), "由 NetShare 节点代理管理") {
		return nil, fmt.Errorf("同名 sysctl 文件不属于 NetShare，已停止操作")
	}
	snapshot := &forwardSnapshot{runtime: runtime, sysctl: old, existed: existed}
	if err = s.writeFile(s.sysctlFile, []byte("# 由 NetShare 节点代理管理；卸载请参考运维文档\nnet.ipv4.ip_forward = 1\n"), 0644); err != nil {
		return nil, combineRollback(fmt.Errorf("写入转发持久配置失败：%w", err), s.restoreForwarding(snapshot))
	}
	if value != "1" {
		if err = os.WriteFile(s.forwardFile, []byte("1\n"), 0644); err != nil {
			return nil, combineRollback(fmt.Errorf("开启 IPv4 转发失败：%w", err), s.restoreForwarding(snapshot))
		}
	}
	return snapshot, nil
}

func (s *Server) restoreForwarding(snapshot *forwardSnapshot) error {
	var errs []error
	if err := os.WriteFile(s.forwardFile, snapshot.runtime, 0644); err != nil {
		errs = append(errs, fmt.Errorf("恢复 IPv4 转发失败：%w", err))
	}
	if snapshot.existed {
		if err := s.writeFile(s.sysctlFile, snapshot.sysctl, 0644); err != nil {
			errs = append(errs, fmt.Errorf("恢复转发持久配置失败：%w", err))
		}
	} else {
		if err := s.removeFile(s.sysctlFile); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%v", errs)
	}
	return nil
}

// ValidateTarget 可用于安装检查，不接受公网、回环或当前节点自己的地址。
func ValidateTarget(ip, local string) bool {
	a, err := netip.ParseAddr(ip)
	return err == nil && a.Is4() && a.IsPrivate() && a.String() == ip && ip != local
}

func (s *Server) allowedTargets() []string {
	out := []string{}
	for ip := range s.allowed {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}
