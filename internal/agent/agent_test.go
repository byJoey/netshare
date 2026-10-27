package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"netshare/internal/model"
)

type fakeRunner struct {
	installed   map[string]bool
	listeners   string
	routeDevice string
	failCheck   bool
	failApply   bool
	mutations   []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	switch name {
	case "ss":
		return []byte(f.listeners), nil
	case "ip":
		if len(args) > 2 && args[2] == "address" {
			ip := "203.0.113.10"
			if args[len(args)-1] == "eth8" {
				ip = "10.42.0.10"
			}
			return []byte(fmt.Sprintf(`[{"addr_info":[{"local":%q}]}]`, ip)), nil
		}
		device := f.routeDevice
		if device == "" {
			device = "eth8"
		}
		return []byte(fmt.Sprintf(`[{"dev":%q,"src":"10.42.0.10"}]`, device)), nil
	case "nft":
		if len(args) > 1 && args[0] == "-j" {
			if f.installed == nil {
				return []byte("Error: No such file or directory"), errors.New("退出码 1")
			}
			entries := []any{}
			for c := range f.installed {
				entries = append(entries, map[string]any{"rule": map[string]string{"comment": c}})
			}
			data, _ := json.Marshal(map[string]any{"nftables": entries})
			return data, nil
		}
		if args[0] == "-c" {
			if f.failCheck {
				return []byte("syntax error"), errors.New("失败")
			}
			return nil, nil
		}
		if f.failApply {
			return []byte("transaction rejected"), errors.New("失败")
		}
		f.mutations = append(f.mutations, string(stdin))
		f.installed = map[string]bool{}
		re := regexp.MustCompile(`comment "([^"]+)"`)
		for _, m := range re.FindAllSubmatch(stdin, -1) {
			f.installed[string(m[1])] = true
		}
		return nil, nil
	}
	return nil, fmt.Errorf("未预期的命令 %s %v", name, args)
}

func testConfig(dir string) Config {
	return Config{Node: model.Node{ID: "l1", Name: "L1", PublicIP: "203.0.113.10", PrivateIP: "10.42.0.10", PublicInterface: "eth7", PrivateInterface: "eth8", ReservedPorts: []int{443}}, Listen: "10.42.0.10:31880", StateFile: filepath.Join(dir, "state.json"), ControllerCN: "controller", AllowedTargets: []string{"10.42.0.11"}}
}

func newTestServer(t *testing.T) (*Server, *fakeRunner) {
	t.Helper()
	dir := t.TempDir()
	runner := &fakeRunner{}
	s, err := NewServer(testConfig(dir), runner)
	if err != nil {
		t.Fatal(err)
	}
	s.forwardFile = filepath.Join(dir, "ip_forward")
	s.sysctlFile = filepath.Join(dir, "sysctl.conf")
	if err = os.WriteFile(s.forwardFile, []byte("0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return s, runner
}

func testRule() model.AgentRule {
	return model.AgentRule{ID: "map-one", Name: "测试服务", Protocol: "tcp", ListenPort: "18080", TargetPort: "8080", TargetIP: "10.42.0.11", Enabled: true}
}

func TestValidationProtectsPortsAndTargets(t *testing.T) {
	s, _ := newTestServer(t)
	cases := []struct {
		name      string
		edit      func(*model.AgentRule)
		listeners []model.Listener
	}{
		{"SSH", func(r *model.AgentRule) { r.ListenPort = "22" }, nil},
		{"控制台", func(r *model.AgentRule) { r.ListenPort = "443" }, nil},
		{"代理端口", func(r *model.AgentRule) { r.ListenPort = "31880" }, nil},
		{"端口不等长", func(r *model.AgentRule) { r.ListenPort = "18080-18081" }, nil},
		{"注入ID", func(r *model.AgentRule) { r.ID = "x\"; flush ruleset" }, nil},
		{"外部目标", func(r *model.AgentRule) { r.TargetIP = "1.1.1.1" }, nil},
		{"非白名单私网", func(r *model.AgentRule) { r.TargetIP = "10.42.0.12" }, nil},
		{"公网监听冲突", func(r *model.AgentRule) {}, []model.Listener{{Protocol: "tcp", Address: "203.0.113.10", Port: 18080}}},
		{"全地址监听冲突", func(r *model.AgentRule) {}, []model.Listener{{Protocol: "tcp", Address: "::", Port: 18080}}},
		{"both监听冲突", func(r *model.AgentRule) { r.Protocol = "both" }, []model.Listener{{Protocol: "udp", Address: "0.0.0.0", Port: 18080}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := testRule()
			c.edit(&r)
			if err := s.validateRules([]model.AgentRule{r}, c.listeners); err == nil {
				t.Fatal("危险配置未被阻止")
			}
		})
	}
	if err := s.validateRules([]model.AgentRule{testRule()}, []model.Listener{{Protocol: "tcp", Address: "10.42.0.10", Port: 18080}, {Protocol: "tcp", Address: "127.0.0.1", Port: 18080}, {Protocol: "udp", Address: "*", Port: 18080}}); err != nil {
		t.Fatalf("私网或其他协议的监听被误判：%v", err)
	}
	other := testRule()
	other.ID = "map-two"
	other.Protocol = "both"
	if err := s.validateRules([]model.AgentRule{testRule(), other}, nil); err == nil {
		t.Fatal("映射间冲突未阻止")
	}
	other.Enabled = false
	if err := s.validateRules([]model.AgentRule{testRule(), other}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestExpansionBudget(t *testing.T) {
	s, _ := newTestServer(t)
	a := testRule()
	a.Protocol = "both"
	a.ListenPort = "20000-20999"
	a.TargetPort = "30000-30999"
	b := testRule()
	b.ID = "more"
	b.ListenPort = "22000"
	if err := s.validateRules([]model.AgentRule{a}, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.validateRules([]model.AgentRule{a, b}, nil); err == nil {
		t.Fatal("展开规则超限未阻止")
	}
}

func TestRenderScopesPublicIngressAndOffsetsPorts(t *testing.T) {
	s, _ := newTestServer(t)
	r := testRule()
	r.Protocol = "both"
	r.ListenPort = "18080-18081"
	r.TargetPort = "8080-8081"
	script := string(renderRules(s.cfg.Node, 5, []model.AgentRule{r}))
	for _, needle := range []string{`iifname "eth7" ip daddr 203.0.113.10 tcp dport 18080`, `dnat to 10.42.0.11:8080`, `udp dport 18081 counter dnat to 10.42.0.11:8081`, `iifname "eth7" oifname "eth8"`, `ct original proto-dst 18081`, `snat to 10.42.0.10`, `netshare:revision:5`} {
		if !strings.Contains(script, needle) {
			t.Errorf("缺少精确匹配：%s", needle)
		}
	}
	if strings.Contains(script, "flush ruleset") || strings.Contains(script, "table inet f2b") {
		t.Fatal("触及其他服务规则")
	}
	if count := strings.Count(script, "add rule"); count != 9 {
		t.Fatalf("展开规则数量 %d，预期9", count)
	}
	r.Enabled = false
	if strings.Contains(string(renderRules(s.cfg.Node, 6, []model.AgentRule{r})), "dnat to") {
		t.Fatal("停用规则仍生成NAT")
	}
}

func TestApplyPersistsAndRestoresSameRevision(t *testing.T) {
	s, runner := newTestServer(t)
	req := model.ApplyRequest{Revision: 7, Rules: []model.AgentRule{testRule()}}
	result, err := s.Apply(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if result.Revision != 7 || s.state.OriginalForwarding == nil || *s.state.OriginalForwarding {
		t.Fatal("未保存首次转发状态")
	}
	data, _ := os.ReadFile(s.forwardFile)
	if strings.TrimSpace(string(data)) != "1" {
		t.Fatal("未开启转发")
	}
	info, _ := os.Stat(s.cfg.StateFile)
	if info.Mode().Perm() != 0600 {
		t.Fatal("状态文件权限不正确")
	}
	runner.installed = nil
	if _, err = s.Apply(context.Background(), req); err != nil {
		t.Fatalf("同版重同步失败：%v", err)
	}
	if !runner.installed["netshare:revision:7"] {
		t.Fatal("未重建丢失规则")
	}
	before := len(runner.mutations)
	req.Revision = 6
	if _, err = s.Apply(context.Background(), req); err == nil {
		t.Fatal("过旧版本未拒绝")
	}
	req.Revision = 7
	req.Rules[0].TargetPort = "9999"
	if _, err = s.Apply(context.Background(), req); err == nil {
		t.Fatal("同版不同内容未拒绝")
	}
	if len(runner.mutations) != before {
		t.Fatal("版本拒绝后仍修改内核")
	}
	loaded, err := NewServer(s.cfg, runner)
	if err != nil {
		t.Fatal(err)
	}
	loaded.forwardFile = s.forwardFile
	loaded.sysctlFile = s.sysctlFile
	if err = loaded.Restore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if loaded.state.Revision != 7 || loaded.state.Rules[0].TargetPort != "8080" {
		t.Fatal("重启恢复错误")
	}
}

func TestNftFailureRestoresForwarding(t *testing.T) {
	for _, stage := range []string{"check", "apply"} {
		t.Run(stage, func(t *testing.T) {
			s, r := newTestServer(t)
			if stage == "check" {
				r.failCheck = true
			} else {
				r.failApply = true
			}
			if _, err := s.Apply(context.Background(), model.ApplyRequest{Revision: 1, Rules: []model.AgentRule{testRule()}}); err == nil {
				t.Fatal("未返回nft错误")
			}
			forward, _ := os.ReadFile(s.forwardFile)
			if strings.TrimSpace(string(forward)) != "0" {
				t.Fatal("未恢复ip_forward")
			}
			if _, err := os.Stat(s.sysctlFile); !os.IsNotExist(err) {
				t.Fatal("遗留sysctl文件")
			}
			if s.state.Revision != 0 {
				t.Fatal("失败仍更新内存状态")
			}
		})
	}
}

func TestPersistenceFailureRollsKernelAndDiskBack(t *testing.T) {
	s, r := newTestServer(t)
	old := model.ApplyRequest{Revision: 10, Rules: []model.AgentRule{testRule()}}
	if _, err := s.Apply(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	originalWrite := s.writeFile
	failed := false
	s.writeFile = func(path string, data []byte, mode os.FileMode) error {
		if path == s.cfg.StateFile && !failed {
			failed = true
			return errors.New("模拟磁盘空间不足")
		}
		return originalWrite(path, data, mode)
	}
	nextRule := testRule()
	nextRule.TargetPort = "8081"
	if _, err := s.Apply(context.Background(), model.ApplyRequest{Revision: 11, Rules: []model.AgentRule{nextRule}}); err == nil {
		t.Fatal("磁盘失败被忽略")
	}
	if s.state.Revision != 10 || !r.installed["netshare:revision:10"] {
		t.Fatal("内核或内存未回到旧配置")
	}
	data, _ := os.ReadFile(s.cfg.StateFile)
	var disk diskState
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	if disk.Revision != 10 || disk.Rules[0].TargetPort != "8080" {
		t.Fatal("磁盘未回到旧配置")
	}
}

func TestWrongRouteAndForeignTableNeverMutate(t *testing.T) {
	s, r := newTestServer(t)
	r.routeDevice = "eth7"
	if _, err := s.Apply(context.Background(), model.ApplyRequest{Revision: 1, Rules: []model.AgentRule{testRule()}}); err == nil {
		t.Fatal("目标走公网仍允许应用")
	}
	if len(r.mutations) != 0 {
		t.Fatal("路由拒绝后修改了规则")
	}
	r.routeDevice = "eth8"
	r.installed = map[string]bool{"other-application": true}
	if _, err := s.Apply(context.Background(), model.ApplyRequest{Revision: 1, Rules: []model.AgentRule{testRule()}}); err == nil {
		t.Fatal("覆盖了其他程序的同名表")
	}
	if len(r.mutations) != 0 {
		t.Fatal("覆盖了同名外部表")
	}
}

func TestStatusDetectsKernelDrift(t *testing.T) {
	s, r := newTestServer(t)
	if _, err := s.Apply(context.Background(), model.ApplyRequest{Revision: 2, Rules: []model.AgentRule{testRule()}}); err != nil {
		t.Fatal(err)
	}
	s.readFile = func(path string) ([]byte, error) {
		if strings.HasPrefix(path, "/sys/") {
			return []byte("12345\n"), nil
		}
		return os.ReadFile(path)
	}
	status, err := s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Revision != 2 || status.SavedRevision != 2 || len(status.Rules) != 1 || !status.Forwarding || status.Interfaces[0].TXBytes != 12345 {
		t.Fatalf("状态不正确：%+v", status)
	}
	delete(r.installed, "netshare:map-one:tcp:18080:d")
	status, err = s.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Revision != 0 || status.SavedRevision != 2 || len(status.Rules) != 0 || len(status.Warnings) == 0 {
		t.Fatal("缺少内核规则仍报告已应用")
	}
	r.installed = nil
	status, err = s.Status(context.Background())
	if err != nil || status.Revision != 0 || status.SavedRevision != 2 || len(status.Rules) != 0 {
		t.Fatal("丢表后未保留已保存版本，控制台将无法对齐修订号")
	}
}

func TestListenParsingAndBindingScopes(t *testing.T) {
	data := []byte("tcp LISTEN 0 4096 *:22 *:* users:((\"sshd\",pid=1,fd=3))\n" + "udp UNCONN 0 0 127.0.0.53%lo:53 0.0.0.0:* users:((\"systemd-resolve\",pid=2,fd=3))\n" + "tcp LISTEN 0 4096 [::ffff:203.0.113.10]:8080 [::]:*\n")
	listeners, err := parseListeners(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(listeners) != 3 || listeners[0].Process != "sshd" || listeners[1].Address != "127.0.0.53" {
		t.Fatalf("解析结果错误：%+v", listeners)
	}
	if !publicListener(listeners[2].Address, "203.0.113.10") {
		t.Fatal("IPv4映射IPv6监听未判为公网")
	}
}

func TestRestoreFailureKeepsManagementAndClearsAfterRepair(t *testing.T) {
	s, runner := newTestServer(t)
	rule := testRule()
	if _, err := s.Apply(context.Background(), model.ApplyRequest{Revision: 1, Rules: []model.AgentRule{rule}}); err != nil {
		t.Fatal(err)
	}
	runner.installed = nil
	runner.listeners = "tcp LISTEN 0 4096 *:18080 *:* users:((\"other-service\",pid=1,fd=3))\n"
	if err := s.Restore(context.Background()); err == nil {
		t.Fatal("恢复时应检测到本机端口冲突")
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/v1/status", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "启动恢复映射失败") {
		t.Fatal("恢复失败后管理状态不可用或未显示原因", w.Body.String())
	}
	rule.Enabled = false
	if _, err := s.Apply(context.Background(), model.ApplyRequest{Revision: 2, Rules: []model.AgentRule{rule}}); err != nil {
		t.Fatal("无法通过管理接口停用冲突映射", err)
	}
	if s.restoreWarning != "" {
		t.Fatal("修复后仍保留旧的恢复失败告警")
	}
}

func TestHTTPRejectsUnknownFieldsAndUnsupportedMethods(t *testing.T) {
	s, _ := newTestServer(t)
	for _, test := range []struct {
		method, path, body string
		status             int
	}{{"POST", "/v1/apply", `{"revision":1,"rules":[],"shell":"bad"}`, 400}, {"GET", "/v1/apply", "", 405}, {"POST", "/nope", "{}", 404}, {"POST", "/v1/apply", "{} {}", 400}, {"POST", "/v1/apply", strings.Repeat("x", maxBodyBytes+1), 400}} {
		r := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, r)
		if w.Code != test.status {
			t.Errorf("%s %s 返回 %d，期望%d：%s", test.method, test.path, w.Code, test.status, w.Body.String())
		}
		var result map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["error"] == "" {
			t.Fatal("错误响应不是JSON")
		}
	}
}

func TestProbeRejectsSSRFAndDoesNotClaimUDPWorks(t *testing.T) {
	s, _ := newTestServer(t)
	if _, err := s.Probe(context.Background(), model.ProbeRequest{TargetIP: "169.254.169.254", TargetPort: "80", Protocol: "tcp"}); err == nil {
		t.Fatal("允许访问元数据地址")
	}
	result, err := s.Probe(context.Background(), model.ProbeRequest{TargetIP: "10.42.0.11", TargetPort: "9000", Protocol: "udp"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Reachable || !strings.Contains(result.Message, "没有握手") || result.Interface != "eth8" {
		t.Fatal("将UDP握手当作可达证明")
	}
}

func TestMutualTLSRejectsMissingAndWrongClient(t *testing.T) {
	s, _ := newTestServer(t)
	dir := t.TempDir()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "测试CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	issue := func(cn string, usage x509.ExtKeyUsage, serial int64) tls.Certificate {
		t.Helper()
		leafKey, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: cn}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		der, e := x509.CreateCertificate(rand.Reader, template, caCert, &leafKey.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		keyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
		cert, e := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
		if e != nil {
			t.Fatal(e)
		}
		return cert
	}
	serverCert := issue("agent-l1", x509.ExtKeyUsageServerAuth, 2)
	s.cfg.CAFile = filepath.Join(dir, "ca.pem")
	s.cfg.CertFile = filepath.Join(dir, "server.pem")
	s.cfg.KeyFile = filepath.Join(dir, "server-key.pem")
	_ = os.WriteFile(s.cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644)
	_ = os.WriteFile(s.cfg.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCert.Certificate[0]}), 0644)
	serverKey, _ := x509.MarshalPKCS8PrivateKey(serverCert.PrivateKey)
	_ = os.WriteFile(s.cfg.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKey}), 0600)
	tlsConfig, err := loadTLS(s.cfg)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	server.TLS = tlsConfig
	server.StartTLS()
	defer server.Close()
	for _, item := range []struct {
		name string
		cert []tls.Certificate
		ok   bool
	}{{"没有客户端证书", nil, false}, {"其他节点证书", []tls.Certificate{issue("other-agent", x509.ExtKeyUsageClientAuth, 3)}, false}, {"控制台证书", []tls.Certificate{issue("controller", x509.ExtKeyUsageClientAuth, 4)}, true}} {
		t.Run(item.name, func(t *testing.T) {
			transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, Certificates: item.cert}}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: time.Second}
			response, e := client.Get(server.URL)
			if item.ok {
				if e != nil {
					t.Fatal(e)
				}
				response.Body.Close()
			} else {
				if e == nil {
					response.Body.Close()
					t.Fatal("未经授权的客户端连接成功")
				}
			}
		})
	}
}
