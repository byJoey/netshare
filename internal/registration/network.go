package registration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"netshare/internal/model"
)

type Interface struct {
	Name      string
	Addresses []string
}

type NetworkOptions struct {
	PublicIP         string
	PrivateInterface string
	PrivateIP        string
	SourceIP         string
	Peers            []string
	Name             string
}

type NetworkProblem struct {
	Code       string
	Message    string
	Candidates []model.Node
}

func (e *NetworkProblem) Error() string { return e.Message }

var hostnameUUID = regexp.MustCompile(`(?i)^[a-f0-9]{8}-[a-f0-9-]{27}$`)

func excludedInterface(name string) bool {
	for _, prefix := range []string{"lo", "docker", "br-", "veth", "virbr", "tun", "tap", "wg", "tailscale", "cni", "flannel", "zt"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func DetectNetwork(opt NetworkOptions) (model.Node, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return model.Node{}, fmt.Errorf("读取本机网络失败")
	}
	var inventory []Interface
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 || excludedInterface(iface.Name) {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return model.Node{}, fmt.Errorf("读取网卡 %s 失败", iface.Name)
		}
		item := Interface{Name: iface.Name}
		for _, address := range addresses {
			item.Addresses = append(item.Addresses, address.String())
		}
		inventory = append(inventory, item)
	}
	var routes []struct {
		Dev string `json:"dev"`
	}
	data, err := exec.Command("ip", "-j", "-4", "route", "show", "default").Output()
	if err != nil || json.Unmarshal(data, &routes) != nil || len(routes) == 0 {
		return model.Node{}, fmt.Errorf("没有找到默认网络，请先确认服务器能联网")
	}
	hostname, _ := os.Hostname()
	return SelectNetwork(inventory, routes[0].Dev, hostname, opt)
}

// SelectNetwork 只从真实网卡和地址选取，不创建网卡，不把Docker网桥当云私网。
func SelectNetwork(inventory []Interface, defaultInterface, hostname string, opt NetworkOptions) (model.Node, error) {
	var publicIP, publicInterface string
	for _, iface := range inventory {
		if iface.Name != defaultInterface {
			continue
		}
		for _, value := range iface.Addresses {
			prefix, err := netip.ParsePrefix(value)
			if err != nil {
				continue
			}
			ip := prefix.Addr()
			if ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() {
				publicIP, publicInterface = ip.String(), iface.Name
				break
			}
		}
	}
	if opt.PublicIP != "" {
		publicIP, publicInterface = opt.PublicIP, defaultInterface
	} else if publicIP == "" {
		if ip, err := netip.ParseAddr(opt.SourceIP); err == nil && ip.Is4() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() {
			publicIP, publicInterface = ip.String(), defaultInterface
		}
	}
	if publicIP == "" {
		return model.Node{}, &NetworkProblem{Code: "no_public", Message: "没有识别到公网入口。NAT服务器可用 --public-ip 指定公网IP后重试"}
	}
	var candidates, matched []model.Node
	for _, iface := range inventory {
		if iface.Name == publicInterface || excludedInterface(iface.Name) || opt.PrivateInterface != "" && opt.PrivateInterface != iface.Name {
			continue
		}
		for _, value := range iface.Addresses {
			prefix, err := netip.ParsePrefix(value)
			if err != nil || !prefix.Addr().Is4() || !prefix.Addr().IsPrivate() || opt.PrivateIP != "" && opt.PrivateIP != prefix.Addr().String() {
				continue
			}
			node := model.Node{PublicIP: publicIP, PublicInterface: publicInterface, PrivateIP: prefix.Addr().String(), PrivateInterface: iface.Name, ReservedPorts: []int{22, 31880}}
			candidates = append(candidates, node)
			for _, peer := range opt.Peers {
				ip, err := netip.ParseAddr(peer)
				if err == nil && prefix.Contains(ip) {
					matched = append(matched, node)
					break
				}
			}
		}
	}
	if len(matched) == 1 {
		candidates = matched
	}
	if len(candidates) == 0 {
		return model.Node{}, &NetworkProblem{Code: "no_private", Message: "还没有可用的私网地址。请先在服务商面板启用私网，再运行这条命令"}
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].PrivateInterface+candidates[i].PrivateIP < candidates[j].PrivateInterface+candidates[j].PrivateIP
	})
	if len(candidates) != 1 {
		return model.Node{}, &NetworkProblem{Code: "ambiguous_private", Message: "发现多张私网网卡，需要选择本次使用的一张", Candidates: candidates}
	}
	node := candidates[0]
	name := strings.TrimSpace(opt.Name)
	if name == "" {
		name = hostname
		if hostnameUUID.MatchString(name) || name == "" {
			parts := strings.Split(publicIP, ".")
			name = "节点 " + parts[len(parts)-1]
		}
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	runes := []rune(name)
	if len(runes) > 60 {
		name = string(runes[:60])
	}
	node.Name = name
	return node, nil
}
