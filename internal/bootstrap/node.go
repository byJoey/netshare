package bootstrap

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"netshare/internal/control"
	"netshare/internal/model"
)

type nodeOptions struct {
	configPath    string
	authorityDir  string
	credentialDir string
	dir           string
	node          model.Node
	agentPort     int
	exportOnly    bool
}

// NodeCommand 只导出配对候选，不修改运行中的配置或映射状态。
func NodeCommand(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "invite":
			return inviteCommand(args[1:])
		case "fetch":
			return fetchCommand(args[1:])
		}
	}
	if len(args) == 0 || (args[0] != "add" && args[0] != "export") {
		return fmt.Errorf("用法：netshare node add --config 配置 --authority CA目录 --id 节点ID --name 名称 --public-ip 公网IP --private-ip 私网IP --public-interface 公网网卡 --private-interface 私网网卡 --dir 新目录")
	}
	fs := flag.NewFlagSet("netshare node "+args[0], flag.ContinueOnError)
	var opt nodeOptions
	opt.exportOnly = args[0] == "export"
	fs.StringVar(&opt.configPath, "config", "", "现有控制台配置")
	fs.StringVar(&opt.authorityDir, "authority", "", "仅管理员可读的原 CA 目录")
	fs.StringVar(&opt.credentialDir, "credential-dir", "", "显式指定现有控制台叶子目录；默认使用配置中的路径")
	fs.StringVar(&opt.dir, "dir", "", "尚不存在的配对导出目录")
	fs.StringVar(&opt.node.ID, "id", "", "节点 ID")
	fs.StringVar(&opt.node.Name, "name", "", "节点名称")
	fs.StringVar(&opt.node.PublicIP, "public-ip", "", "节点公网 IPv4")
	fs.StringVar(&opt.node.PrivateIP, "private-ip", "", "节点私网 IPv4")
	fs.StringVar(&opt.node.PublicInterface, "public-interface", "", "公网网卡")
	fs.StringVar(&opt.node.PrivateInterface, "private-interface", "", "私网网卡")
	fs.IntVar(&opt.agentPort, "agent-port", defaultAgentPort, "新节点代理私网端口")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("node %s 不接受位置参数", args[0])
	}
	if err := addNode(opt); err != nil {
		return err
	}
	abs, _ := filepath.Abs(opt.dir)
	fmt.Fprintf(os.Stdout, "节点配对资料已生成：%s\n请应用 controller.json，并将各节点自己的 agents/节点ID 导入包重新安装到对应机器。\n", abs)
	return nil
}

func addNode(opt nodeOptions) error {
	if opt.configPath == "" || opt.authorityDir == "" || opt.dir == "" {
		return fmt.Errorf("请提供 --config、--authority 和 --dir")
	}
	if err := rejectSymlinkPath(opt.configPath); err != nil {
		return err
	}
	cfg, err := control.LoadConfig(opt.configPath)
	if err != nil {
		return err
	}
	if opt.exportOnly {
		if len(cfg.Nodes) == 0 {
			return fmt.Errorf("暂无可导出的节点，请先添加节点")
		}
	} else {
		if opt.agentPort < 1 || opt.agentPort > 65535 || opt.agentPort == 22 {
			return fmt.Errorf("代理端口必须为 1–65535 且不能使用 SSH 22")
		}
		opt.node.Name = strings.TrimSpace(opt.node.Name)
		if err := model.ValidateNode(opt.node); err != nil {
			return err
		}
		if len([]rune(opt.node.Name)) > 80 {
			return fmt.Errorf("机器名称最多 80 个字符")
		}
		if len(cfg.Nodes) >= maxNodes {
			return fmt.Errorf("最多配置 %d 台机器", maxNodes)
		}
		for _, old := range cfg.Nodes {
			if old.ID == opt.node.ID || old.PublicIP == opt.node.PublicIP || old.PrivateIP == opt.node.PrivateIP {
				return fmt.Errorf("新节点的 ID、公网或私网 IP 与现有节点重复")
			}
		}
		opt.node.AgentURL = "https://" + net.JoinHostPort(opt.node.PrivateIP, strconv.Itoa(opt.agentPort))
		opt.node.ReservedPorts = []int{22, opt.agentPort}
		sort.Ints(opt.node.ReservedPorts)
		cfg.Nodes = append(cfg.Nodes, opt.node)
	}

	ca, err := readAuthority(opt.authorityDir)
	if err != nil {
		return err
	}
	if err = matchControllerAuthority(cfg, opt.credentialDir, ca); err != nil {
		return err
	}
	abs, err := filepath.Abs(opt.dir)
	if err != nil {
		return err
	}
	if _, err = os.Lstat(abs); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("配对导出目录必须尚不存在")
	}
	files := make(map[string][]byte)
	files["controller.json"], err = marshal(cfg)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, node := range cfg.Nodes {
		u, err := url.Parse(node.AgentURL)
		if err != nil || u.Port() == "" {
			return fmt.Errorf("节点 %s 的代理地址缺少端口", node.ID)
		}
		port, err := strconv.Atoi(u.Port())
		if err != nil || port < 1 || port > 65535 || port == 22 {
			return fmt.Errorf("节点 %s 的代理端口无效", node.ID)
		}
		leaf, err := makeLeaf(now, node.ID, net.ParseIP(node.PrivateIP), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, ca)
		if err != nil {
			return err
		}
		targets := make([]string, 0, len(cfg.Nodes)-1)
		for _, peer := range cfg.Nodes {
			if peer.ID != node.ID {
				targets = append(targets, peer.PrivateIP)
			}
		}
		a := agentConfig{Node: node, Listen: net.JoinHostPort(node.PrivateIP, u.Port()), CAFile: "/etc/netshare-agent/ca.pem", CertFile: "/etc/netshare-agent/server.pem", KeyFile: "/etc/netshare-agent/server-key.pem", StateFile: "/var/lib/netshare-agent/state.json", ControllerCN: "controller", AllowedTargets: targets}
		base := filepath.Join("agents", node.ID)
		files[filepath.Join(base, "agent.json")], err = marshal(a)
		if err != nil {
			return err
		}
		files[filepath.Join(base, "ca.pem")] = ca.certPEM
		files[filepath.Join(base, "server.pem")] = leaf.certPEM
		files[filepath.Join(base, "server-key.pem")] = leaf.keyPEM
	}
	return writeBundle(abs, files)
}

// rejectSymlinkPath 不接受配置/私钥路径的符号链接，避免越过管理员选定的目录。
func rejectSymlinkPath(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	for p := abs; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil {
			return fmt.Errorf("检查凭据路径失败：%w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("凭据路径不能包含符号链接")
		}
		if p == filepath.Dir(p) {
			return nil
		}
	}
}

func readPrivateInput(path string) ([]byte, error) {
	if err := rejectSymlinkPath(path); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("凭据必须是仅属主可读写的普通文件：%s", path)
	}
	return os.ReadFile(path)
}

func readAuthority(dir string) (certificate, error) {
	if err := rejectSymlinkPath(dir); err != nil {
		return certificate{}, err
	}
	info, err := os.Stat(dir)
	if err != nil {
		return certificate{}, err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return certificate{}, fmt.Errorf("CA 目录必须仅管理员可访问（0700）")
	}
	if err = checkAuthorityOwner(info); err != nil {
		return certificate{}, err
	}
	for _, name := range []string{"ca.pem", "ca-key.pem"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return certificate{}, err
		}
		if err = checkAuthorityOwner(info); err != nil {
			return certificate{}, err
		}
	}
	certPEM, err := readPrivateInput(filepath.Join(dir, "ca.pem"))
	if err != nil {
		return certificate{}, err
	}
	keyPEM, err := readPrivateInput(filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		return certificate{}, err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) != 1 {
		return certificate{}, fmt.Errorf("原 CA 证书和私钥不匹配")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || !cert.IsCA || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return certificate{}, fmt.Errorf("原 CA 证书无效")
	}
	if err = cert.CheckSignatureFrom(cert); err != nil {
		return certificate{}, fmt.Errorf("原 CA 证书不是有效的自签 CA")
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		return certificate{}, fmt.Errorf("原 CA 私钥不是 NetShare 使用的 ECDSA 格式")
	}
	return certificate{certPEM: certPEM, keyPEM: keyPEM, cert: cert, key: key}, nil
}

func checkAuthorityOwner(info os.FileInfo) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("CA 目录和私钥必须由当前管理员持有，不能交给 Web 服务用户")
	}
	return nil
}

func matchControllerAuthority(cfg control.Config, dir string, ca certificate) error {
	caFile, certFile, keyFile := cfg.CAFile, cfg.CertFile, cfg.KeyFile
	if dir != "" {
		caFile = filepath.Join(dir, "ca.pem")
		certFile = filepath.Join(dir, "controller.pem")
		keyFile = filepath.Join(dir, "controller-key.pem")
	}
	caPEM, err := readPrivateInput(caFile)
	if err != nil {
		return err
	}
	block, rest := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 || !bytes.Equal(block.Bytes, ca.cert.Raw) {
		return fmt.Errorf("所选 CA 与控制台现有 CA 不同，拒绝生成无法配对的节点")
	}
	certPEM, err := readPrivateInput(certFile)
	if err != nil {
		return err
	}
	keyPEM, err := readPrivateInput(keyFile)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil || len(pair.Certificate) != 1 {
		return fmt.Errorf("控制台客户端证书或私钥无效")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || cert.Subject.CommonName != "controller" {
		return fmt.Errorf("控制台客户端证书身份无效")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca.cert)
	if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		return fmt.Errorf("控制台客户端证书不属于当前 CA 或已过期")
	}
	return nil
}
