package bootstrap

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"netshare/internal/model"
)

func testNodes() []model.Node {
	return []model.Node{
		{ID: "edge-a", Name: "入口 A", PublicIP: "192.0.2.10", PrivateIP: "10.42.0.10", PublicInterface: "eth0", PrivateInterface: "eth1", ReservedPorts: []int{443, 22}},
		{ID: "edge-b", Name: "入口 B", PublicIP: "192.0.2.11", PrivateIP: "10.42.0.11", PublicInterface: "eth0", PrivateInterface: "eth1", ReservedPorts: []int{2096}},
	}
}

func writeNodes(t *testing.T, nodes []model.Node) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "nodes.json")
	data, err := json.Marshal(nodes)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func certAt(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatal("证书 PEM 无效")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestInitializePrivateBundle(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new-bundle")
	opt := options{dir: dir, nodesFile: writeNodes(t, testNodes()), domain: "NETSHARE.Example.com", listen: defaultListen, agentPort: defaultAgentPort}
	if err := initialize(opt); err != nil {
		t.Fatal(err)
	}
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := os.FileMode(0600)
		if entry.IsDir() {
			want = 0700
		}
		if info.Mode().Perm() != want {
			t.Errorf("%s 权限为 %o，期望 %o", path, info.Mode().Perm(), want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ca := certAt(t, filepath.Join(dir, "authority", "ca.pem"))
	if !ca.IsCA || !ca.MaxPathLenZero || ca.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Fatal("CA 约束不正确")
	}
	if time.Until(ca.NotAfter) < 9*365*24*time.Hour {
		t.Fatal("CA 有效期不足")
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	controller := certAt(t, filepath.Join(dir, "controller", "controller.pem"))
	if controller.Subject.CommonName != "controller" {
		t.Fatal("客户端 CN 不正确")
	}
	if _, err := controller.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("controller 证书不应可冒充服务器")
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, "controller", "controller.pem"), filepath.Join(dir, "controller", "controller-key.pem")); err != nil {
		t.Fatal(err)
	}
	for _, node := range testNodes() {
		base := filepath.Join(dir, "agents", node.ID)
		server := certAt(t, filepath.Join(base, "server.pem"))
		if _, err := server.Verify(x509.VerifyOptions{Roots: pool, DNSName: node.PrivateIP, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Fatal(err)
		}
		if err := server.VerifyHostname(node.PublicIP); err == nil {
			t.Fatal("节点证书不应包含公网 IP")
		}
		if _, err := server.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
			t.Fatal("节点证书不应可冒充 controller")
		}
		if time.Until(server.NotAfter) < 700*24*time.Hour {
			t.Fatal("节点证书有效期不足")
		}
		if _, err := tls.LoadX509KeyPair(filepath.Join(base, "server.pem"), filepath.Join(base, "server-key.pem")); err != nil {
			t.Fatal(err)
		}
		var config agentConfig
		data, err := os.ReadFile(filepath.Join(base, "agent.json"))
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		if config.Listen != net.JoinHostPort(node.PrivateIP, "31880") || config.ControllerCN != "controller" {
			t.Fatalf("代理监听或鉴权配置错误：%+v", config)
		}
		if len(config.AllowedTargets) != 1 || config.AllowedTargets[0] == node.PrivateIP {
			t.Fatal("节点目标白名单错误")
		}
		if !contains(config.Node.ReservedPorts, 22) || !contains(config.Node.ReservedPorts, 31880) {
			t.Fatal("缺少管理保留端口")
		}
		if _, err = os.Stat(filepath.Join(base, "ca-key.pem")); !os.IsNotExist(err) {
			t.Fatal("不应向代理目录复制 CA 私钥")
		}
	}
	var config controllerConfig
	data, err := os.ReadFile(filepath.Join(dir, "controller", "controller.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	pw, err := os.ReadFile(filepath.Join(dir, "controller", "admin-password.txt"))
	if err != nil {
		t.Fatal(err)
	}
	password := strings.TrimSpace(string(pw))
	if len(password) < 32 {
		t.Fatal("随机密码强度不足")
	}
	if err = bcrypt.CompareHashAndPassword([]byte(config.PasswordHash), []byte(password)); err != nil {
		t.Fatal("密码与摘要不匹配")
	}
	if config.ExternalURL != "https://netshare.example.com" || !config.CookieSecure || config.Username != "admin" {
		t.Fatal("生产控制台配置错误")
	}
	if bytes.Contains(data, []byte(password)) {
		t.Fatal("控制台配置中出现密码明文")
	}
	before := append([]byte(nil), pw...)
	if err = initialize(opt); err == nil {
		t.Fatal("重复初始化应拒绝覆盖")
	}
	after, err := os.ReadFile(filepath.Join(dir, "controller", "admin-password.txt"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("拒绝重复初始化时应保留原密码")
	}
}

func TestRejectInvalidInputBeforeCreatingDirectory(t *testing.T) {
	nodesFile := writeNodes(t, testNodes())
	cases := []struct {
		name   string
		change func(*options)
	}{
		{"缺少目录", func(o *options) { o.dir = "" }},
		{"缺少机器清单", func(o *options) { o.nodesFile = "" }},
		{"URL代替域名", func(o *options) { o.domain = "https://netshare.example.com" }},
		{"域名带端口", func(o *options) { o.domain = "netshare.example.com:443" }},
		{"域名非法字符", func(o *options) { o.domain = "bad_name.example.com" }},
		{"域名空标签", func(o *options) { o.domain = "bad..example.com" }},
		{"域名IP", func(o *options) { o.domain = "192.0.2.1" }},
		{"控制台公网HTTP", func(o *options) { o.listen = "0.0.0.0:18440" }},
		{"控制台端口无效", func(o *options) { o.listen = "127.0.0.1:0" }},
		{"代理端口无效", func(o *options) { o.agentPort = 65536 }},
		{"代理侵占SSH", func(o *options) { o.agentPort = 22 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "absent")
			opt := options{dir: dir, nodesFile: nodesFile, domain: "netshare.example.com", listen: defaultListen, agentPort: defaultAgentPort}
			tc.change(&opt)
			if err := initialize(opt); err == nil {
				t.Fatal("应拒绝非法输入")
			}
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Fatal("输入无效时不应创建输出目录")
			}
		})
	}
}

func TestReadNodesRejectsDuplicateAndUnexpectedFields(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"未知字段", `[{"id":"a","unknown":true}]`},
		{"尾随数据", `[] []`},
		{"仅一台机器", string(mustJSON(t, testNodes()[:1]))},
		{"机器数量超限", string(mustJSON(t, make([]model.Node, maxNodes+1)))},
	}
	dup := testNodes()
	dup[1].PrivateIP = dup[0].PrivateIP
	cases = append(cases, struct {
		name string
		data string
	}{"重复私网IP", string(mustJSON(t, dup))})
	badURL := testNodes()
	badURL[0].AgentURL = "https://example.com:31880"
	cases = append(cases, struct {
		name string
		data string
	}{"公网代理URL", string(mustJSON(t, badURL))})
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "nodes.json")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := readNodes(path, 31880); err == nil {
				t.Fatal("应拒绝无效机器清单")
			}
		})
	}
}

func TestRunDoesNotPrintPassword(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bundle")
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = writer
	err = Run([]string{"--dir", dir, "--nodes", writeNodes(t, testNodes()), "--domain", "netshare.example.com"})
	_ = writer.Close()
	os.Stdout = old
	output, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	pw, err := os.ReadFile(filepath.Join(dir, "controller", "admin-password.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(output, bytes.TrimSpace(pw)) || bytes.Contains(output, []byte("PRIVATE KEY")) {
		t.Fatal("标准输出泄漏凭据")
	}
	if !bytes.Contains(output, []byte("admin-password.txt")) {
		t.Fatal("应输出密码文件路径")
	}
}

func contains(values []int, want int) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}
func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
