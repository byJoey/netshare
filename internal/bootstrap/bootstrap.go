// Package bootstrap 为控制台和节点生成独立的首次部署凭据。
package bootstrap

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"

	"netshare/internal/model"
)

const (
	defaultListen    = "127.0.0.1:18440"
	defaultAgentPort = 31880
	maxNodes         = 32
)

type options struct {
	dir         string
	nodesFile   string
	standalone  bool
	domain      string
	externalURL string
	listen      string
	agentPort   int
}

type controllerConfig struct {
	Listen       string       `json:"listen"`
	ExternalURL  string       `json:"external_url"`
	AccessPath   string       `json:"access_path"`
	Username     string       `json:"username"`
	PasswordHash string       `json:"password_hash"`
	StateFile    string       `json:"state_file"`
	CAFile       string       `json:"ca_file"`
	CertFile     string       `json:"cert_file"`
	KeyFile      string       `json:"key_file"`
	CookieSecure bool         `json:"cookie_secure"`
	Nodes        []model.Node `json:"nodes"`
}

type agentConfig struct {
	Node           model.Node `json:"node"`
	Listen         string     `json:"listen"`
	CAFile         string     `json:"ca_file"`
	CertFile       string     `json:"cert_file"`
	KeyFile        string     `json:"key_file"`
	StateFile      string     `json:"state_file"`
	ControllerCN   string     `json:"controller_cn"`
	AllowedTargets []string   `json:"allowed_targets"`
}

type bundleManifest struct {
	Version                string    `json:"version"`
	CreatedAt              time.Time `json:"created_at"`
	Domain                 string    `json:"domain"`
	Nodes                  []string  `json:"nodes"`
	LeafCertificatesExpire time.Time `json:"leaf_certificates_expire"`
	CAExpires              time.Time `json:"ca_expires"`
}

type certificate struct {
	certPEM []byte
	keyPEM  []byte
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
}

// Run 解析 init 子命令参数，只输出凭据文件路径，不输出任何密码或私钥。
func Run(args []string) error {
	fs := flag.NewFlagSet("netshare init", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var opt options
	fs.StringVar(&opt.dir, "dir", "", "新建的私密配置目录，必须尚不存在")
	fs.StringVar(&opt.nodesFile, "nodes", "", "机器清单 JSON 文件")
	fs.BoolVar(&opt.standalone, "standalone", false, "先安装空控制台，之后通过 node add 配对节点")
	fs.StringVar(&opt.domain, "domain", "", "管理台域名，例如 netshare.example.com")
	fs.StringVar(&opt.externalURL, "external-url", "", "空控制台的直接访问地址，例如 http://192.0.2.10:18440")
	fs.StringVar(&opt.listen, "listen", defaultListen, "控制台本地监听地址")
	fs.IntVar(&opt.agentPort, "agent-port", defaultAgentPort, "私网代理监听端口")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("init 不接受位置参数")
	}
	if err := initialize(opt); err != nil {
		return err
	}
	abs, _ := filepath.Abs(opt.dir)
	fmt.Fprintf(os.Stdout, "初始化完成，配置目录：%s\n管理员密码文件：%s\n内部 CA 私钥只保留在 authority 目录，不要交给 Web 服务或节点。\n", abs, filepath.Join(abs, "controller", "admin-password.txt"))
	return nil
}

func initialize(opt options) error {
	if strings.TrimSpace(opt.dir) == "" || (!opt.standalone && strings.TrimSpace(opt.nodesFile) == "") {
		return fmt.Errorf("请提供 --dir，以及 --nodes 或 --standalone")
	}
	if opt.standalone && opt.nodesFile != "" {
		return fmt.Errorf("--standalone 不能与 --nodes 同时使用")
	}
	var domain, externalURL string
	cookieSecure := true
	var err error
	if opt.externalURL != "" {
		if !opt.standalone || opt.domain != "" {
			return fmt.Errorf("--external-url 仅用于 --standalone，不能与 --domain 同时使用")
		}
		externalURL, err = validateDirectHTTP(opt.externalURL, opt.listen)
		if err != nil {
			return err
		}
		cookieSecure = false
	} else {
		domain, err = validateDomain(opt.domain)
		if err != nil {
			return err
		}
		if err = validateListen(opt.listen); err != nil {
			return err
		}
		externalURL = "https://" + domain
	}
	if opt.agentPort < 1 || opt.agentPort > 65535 || opt.agentPort == 22 {
		return fmt.Errorf("代理端口必须为 1–65535 且不能使用 SSH 22")
	}
	nodes := []model.Node{}
	if !opt.standalone {
		nodes, err = readNodes(opt.nodesFile, opt.agentPort)
		if err != nil {
			return err
		}
	}
	abs, err := filepath.Abs(opt.dir)
	if err != nil {
		return fmt.Errorf("解析输出目录失败：%w", err)
	}
	if _, err = os.Lstat(abs); err == nil {
		return fmt.Errorf("配置目录已存在，拒绝覆盖：%s", abs)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查配置目录失败：%w", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	ca, err := makeCA(now)
	if err != nil {
		return err
	}
	client, err := makeLeaf(now, "controller", nil, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, ca)
	if err != nil {
		return err
	}
	passwordBytes := make([]byte, 24)
	if _, err = rand.Read(passwordBytes); err != nil {
		return fmt.Errorf("生成管理员密码失败：%w", err)
	}
	password := base64.RawURLEncoding.EncodeToString(passwordBytes)
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		return fmt.Errorf("生成密码摘要失败：%w", err)
	}

	// 先在内存生成所有内容，避免输入或证书错误留下半套凭据。
	files := map[string][]byte{
		"authority/ca.pem":              ca.certPEM,
		"authority/ca-key.pem":          ca.keyPEM,
		"controller/ca.pem":             ca.certPEM,
		"controller/controller.pem":     client.certPEM,
		"controller/controller-key.pem": client.keyPEM,
		"controller/admin-password.txt": []byte(password + "\n"),
	}
	accessPath, err := model.NewAccessPath()
	if err != nil {
		return err
	}
	controller := controllerConfig{Listen: opt.listen, ExternalURL: externalURL, AccessPath: accessPath, Username: "admin", PasswordHash: string(hash), StateFile: "/var/lib/netshare/state.json", CAFile: "/etc/netshare/ca.pem", CertFile: "/etc/netshare/controller.pem", KeyFile: "/etc/netshare/controller-key.pem", CookieSecure: cookieSecure, Nodes: nodes}
	files["controller/controller.json"], err = marshal(controller)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		leaf, e := makeLeaf(now, node.ID, net.ParseIP(node.PrivateIP), []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, ca)
		if e != nil {
			return e
		}
		targets := make([]string, 0, len(nodes)-1)
		for _, peer := range nodes {
			if peer.ID != node.ID {
				targets = append(targets, peer.PrivateIP)
			}
		}
		config := agentConfig{Node: node, Listen: net.JoinHostPort(node.PrivateIP, strconv.Itoa(opt.agentPort)), CAFile: "/etc/netshare-agent/ca.pem", CertFile: "/etc/netshare-agent/server.pem", KeyFile: "/etc/netshare-agent/server-key.pem", StateFile: "/var/lib/netshare-agent/state.json", ControllerCN: "controller", AllowedTargets: targets}
		prefix := filepath.Join("agents", node.ID)
		files[filepath.Join(prefix, "agent.json")], err = marshal(config)
		if err != nil {
			return err
		}
		files[filepath.Join(prefix, "ca.pem")] = ca.certPEM
		files[filepath.Join(prefix, "server.pem")] = leaf.certPEM
		files[filepath.Join(prefix, "server-key.pem")] = leaf.keyPEM
		ids = append(ids, node.ID)
	}
	files["manifest.json"], err = marshal(bundleManifest{Version: model.Version, CreatedAt: now, Domain: domain, Nodes: ids, LeafCertificatesExpire: client.cert.NotAfter, CAExpires: ca.cert.NotAfter})
	if err != nil {
		return err
	}
	return writeBundle(abs, files)
}

// writeBundle 排他创建私密导出目录，失败时删除本次尚未完整写出的内容。
func writeBundle(abs string, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return fmt.Errorf("创建父目录失败：%w", err)
	}
	// Mkdir 的排他语义保证并发初始化不会覆盖另一套凭据。
	if err := os.Mkdir(abs, 0700); err != nil {
		return fmt.Errorf("创建新配置目录失败：%w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(abs)
		}
	}()
	paths := make([]string, 0, len(files))
	for path := range files {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		full := filepath.Join(abs, path)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			return fmt.Errorf("创建私密子目录失败：%w", err)
		}
		if err := writePrivate(full, files[path]); err != nil {
			return err
		}
	}
	dirHandle, err := os.Open(abs)
	if err != nil {
		return err
	}
	err = dirHandle.Sync()
	_ = dirHandle.Close()
	if err != nil {
		return fmt.Errorf("同步配置目录失败：%w", err)
	}
	success = true
	return nil
}

func readNodes(path string, port int) ([]model.Node, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("读取机器清单失败：%w", err)
	}
	defer f.Close()
	dec := json.NewDecoder(io.LimitReader(f, 1<<20))
	dec.DisallowUnknownFields()
	var nodes []model.Node
	if err = dec.Decode(&nodes); err != nil {
		return nil, fmt.Errorf("机器清单 JSON 无效：%w", err)
	}
	if err = dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("机器清单只能包含一个 JSON 数组")
	}
	if len(nodes) < 2 || len(nodes) > maxNodes {
		return nil, fmt.Errorf("机器清单需要 2–%d 台机器", maxNodes)
	}
	ids, privateIPs, publicIPs := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for i := range nodes {
		n := &nodes[i]
		n.Name = strings.TrimSpace(n.Name)
		if err = model.ValidateNode(*n); err != nil {
			return nil, fmt.Errorf("机器 %d：%w", i+1, err)
		}
		if len([]rune(n.Name)) > 80 {
			return nil, fmt.Errorf("机器名称最多 80 个字符")
		}
		if ids[n.ID] || privateIPs[n.PrivateIP] || publicIPs[n.PublicIP] {
			return nil, fmt.Errorf("机器 ID、公网或私网 IP 有重复")
		}
		ids[n.ID] = true
		privateIPs[n.PrivateIP] = true
		publicIPs[n.PublicIP] = true
		expected := "https://" + net.JoinHostPort(n.PrivateIP, strconv.Itoa(port))
		if n.AgentURL != "" && n.AgentURL != expected {
			return nil, fmt.Errorf("机器 %s 的 agent_url 应为 %s", n.ID, expected)
		}
		n.AgentURL = expected
		reserved := map[int]bool{22: true, port: true}
		for _, p := range n.ReservedPorts {
			if p < 1 || p > 65535 {
				return nil, fmt.Errorf("机器 %s 的保留端口无效", n.ID)
			}
			reserved[p] = true
		}
		n.ReservedPorts = nil
		for p := range reserved {
			n.ReservedPorts = append(n.ReservedPorts, p)
		}
		sort.Ints(n.ReservedPorts)
	}
	return nodes, nil
}

func validateListen(value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("控制台监听地址应为 127.0.0.1:18440：%w", err)
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return fmt.Errorf("控制台监听端口无效")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("控制台 HTTP 只能监听本机回环地址，请通过 HTTPS 反向代理对外提供服务")
	}
	return nil
}

func validateDirectHTTP(value, listen string) (string, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != "" {
		return "", fmt.Errorf("--external-url 应为 http://IPv4:端口，不带路径")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return "", fmt.Errorf("--external-url 需要可访问的 IPv4 地址")
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("--external-url 需要 1–65535 的明确端口")
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil || net.ParseIP(host) == nil || net.ParseIP(host).To4() == nil || net.ParseIP(host).IsMulticast() {
		return "", fmt.Errorf("直接访问模式需要明确的 IPv4 监听地址，例如 0.0.0.0:18440")
	}
	listenPort, err := strconv.Atoi(port)
	if err != nil || listenPort != p {
		return "", fmt.Errorf("直接访问地址端口必须与监听端口相同")
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(p)), nil
}

func validateDomain(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) > 253 || len(value) < 3 || !strings.Contains(value, ".") || net.ParseIP(value) != nil {
		return "", fmt.Errorf("--domain 需要完整域名，例如 netshare.example.com")
	}
	for _, label := range strings.Split(value, ".") {
		if len(label) < 1 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", fmt.Errorf("管理台域名格式无效")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", fmt.Errorf("管理台域名只能包含英文字母、数字、连字符和点")
			}
		}
	}
	u, err := url.Parse("https://" + value)
	if err != nil || u.Hostname() != value || u.Port() != "" {
		return "", fmt.Errorf("管理台域名格式无效")
	}
	return value, nil
}

func serialNumber() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, err
	}
	if n.Sign() == 0 {
		n.SetInt64(1)
	}
	return n, nil
}

func makeCA(now time.Time) (certificate, error) {
	serial, err := serialNumber()
	if err != nil {
		return certificate{}, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "NetShare Internal CA", Organization: []string{"NetShare"}}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.AddDate(10, 0, 0), KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, BasicConstraintsValid: true, IsCA: true, MaxPathLen: 0, MaxPathLenZero: true}
	return sign(template, nil, certificate{})
}

func makeLeaf(now time.Time, cn string, ip net.IP, usage []x509.ExtKeyUsage, ca certificate) (certificate, error) {
	serial, err := serialNumber()
	if err != nil {
		return certificate{}, err
	}
	expires := now.AddDate(2, 0, 0)
	if expires.After(ca.cert.NotAfter) {
		expires = ca.cert.NotAfter
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: cn, Organization: []string{"NetShare"}}, NotBefore: now.Add(-5 * time.Minute), NotAfter: expires, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usage, BasicConstraintsValid: true}
	if ip != nil {
		template.IPAddresses = []net.IP{ip}
	}
	return sign(template, ca.cert, ca)
}

func sign(template, parent *x509.Certificate, ca certificate) (certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return certificate{}, fmt.Errorf("生成证书私钥失败：%w", err)
	}
	if parent == nil {
		parent = template
		ca.key = key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, ca.key)
	if err != nil {
		return certificate{}, fmt.Errorf("签发证书失败：%w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return certificate{}, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return certificate{}, err
	}
	return certificate{certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), cert: parsed, key: key}, nil
}

func marshal(value any) ([]byte, error) {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func writePrivate(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("创建配置文件失败：%w", err)
	}
	if _, err = f.Write(content); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("保存配置文件失败：%w", err)
	}
	return nil
}
