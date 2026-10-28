package bootstrap

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"netshare/internal/agent"
	"netshare/internal/control"
	"netshare/internal/model"
)

func standaloneBundle(t *testing.T) (string, string, control.Config) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(base, "standalone")
	if err = initialize(options{dir: dir, standalone: true, domain: "netshare.example.test", listen: defaultListen, agentPort: defaultAgentPort}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "controller/controller.json")
	cfg, err := control.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	// 使用真实新证书和隔离的状态路径，控制台启动不依赖 mock 二进制或节点。
	cfg.CAFile = filepath.Join(dir, "controller/ca.pem")
	cfg.CertFile = filepath.Join(dir, "controller/controller.pem")
	cfg.KeyFile = filepath.Join(dir, "controller/controller-key.pem")
	cfg.StateFile = filepath.Join(base, "state.json")
	data, err := marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return dir, path, cfg
}

func addOptions(dir, configPath, out string, node model.Node) nodeOptions {
	return nodeOptions{configPath: configPath, authorityDir: filepath.Join(dir, "authority"), dir: out, node: node, agentPort: defaultAgentPort}
}

func TestStandaloneStartsWithoutNodesAndPersistsEmptyState(t *testing.T) {
	dir, configPath, cfg := standaloneBundle(t)
	if cfg.Nodes == nil || len(cfg.Nodes) != 0 {
		t.Fatal("空控制台必须带有空节点数组")
	}
	c, err := control.New(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRecorder()
	c.Handler(nil).ServeHTTP(r, httptest.NewRequest(http.MethodGet, cfg.AccessPath+"/healthz", nil))
	if r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"ok":true`) {
		t.Fatalf("空控制台健康检查失败：%d %s", r.Code, r.Body.String())
	}
	state, err := os.ReadFile(cfg.StateFile)
	if err != nil || !bytes.Contains(state, []byte(`"mappings": []`)) {
		t.Fatal("首次空控制台没有保存空状态")
	}
	// 原 init 不允许把空清单误当 standalone。
	if err = initialize(options{dir: filepath.Join(dir, "bad"), nodesFile: writeNodes(t, nil), domain: "netshare.example.test", listen: defaultListen, agentPort: defaultAgentPort}); err == nil {
		t.Fatal("旧 init 清单仍应要求至少两台节点")
	}
	if err = initialize(options{dir: filepath.Join(dir, "bad"), standalone: true, nodesFile: configPath, domain: "netshare.example.test", listen: defaultListen, agentPort: defaultAgentPort}); err == nil {
		t.Fatal("standalone 不能同时接受机器清单")
	}
}

func TestAddNodeExportsSameCAPairingAndPreservesController(t *testing.T) {
	dir, path, cfg := standaloneBundle(t)
	before, _ := os.ReadFile(path)
	password, _ := os.ReadFile(filepath.Join(dir, "controller/admin-password.txt"))
	key, _ := os.ReadFile(cfg.KeyFile)
	state := []byte(`{"revision":19,"mappings":[],"events":[]}`)
	if err := os.WriteFile(cfg.StateFile, state, 0600); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(filepath.Dir(dir), "first")
	opt := addOptions(dir, path, first, testNodes()[0])
	if err := addNode(opt); err != nil {
		t.Fatal(err)
	}
	one, err := control.LoadConfig(filepath.Join(first, "controller.json"))
	if err != nil || len(one.Nodes) != 1 {
		t.Fatalf("首个节点不能加载：%v", err)
	}
	firstAgent := filepath.Join(first, "agents", testNodes()[0].ID)
	if err = agent.CheckCommand(context.Background(), []string{"--config", filepath.Join(firstAgent, "agent.json"), "--credential-dir", firstAgent, "--skip-address-check"}); err != nil {
		t.Fatal("首节点的真实配对包应能通过安全预检：", err)
	}
	second := filepath.Join(filepath.Dir(dir), "second")
	opt = addOptions(dir, filepath.Join(first, "controller.json"), second, testNodes()[1])
	if err = addNode(opt); err != nil {
		t.Fatal(err)
	}
	paired, err := control.LoadConfig(filepath.Join(second, "controller.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paired.Nodes) != 2 {
		t.Fatal("两次添加应有两台节点")
	}
	nodes := paired.Nodes
	paired.Nodes = cfg.Nodes
	if !reflect.DeepEqual(paired, cfg) {
		t.Fatal("添加节点修改了原控制台的入口、密码或其它配置")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("node add 不得修改源配置")
	}
	stateAfter, _ := os.ReadFile(cfg.StateFile)
	keyAfter, _ := os.ReadFile(cfg.KeyFile)
	passwordAfter, _ := os.ReadFile(filepath.Join(dir, "controller/admin-password.txt"))
	if !bytes.Equal(state, stateAfter) || !bytes.Equal(key, keyAfter) || !bytes.Equal(password, passwordAfter) {
		t.Fatal("node add 修改了原状态、客户端密钥或首次密码")
	}
	pool := x509.NewCertPool()
	pool.AddCert(certAt(t, filepath.Join(dir, "authority/ca.pem")))
	for _, node := range nodes {
		base := filepath.Join(second, "agents", node.ID)
		if err = agent.CheckCommand(context.Background(), []string{"--config", filepath.Join(base, "agent.json"), "--credential-dir", base, "--skip-address-check"}); err != nil {
			t.Fatal("完整配对包预检失败：", err)
		}
		var agent agentConfig
		data, err := os.ReadFile(filepath.Join(base, "agent.json"))
		if err != nil || json.Unmarshal(data, &agent) != nil {
			t.Fatalf("节点配置读取失败：%v", err)
		}
		if len(agent.AllowedTargets) != 1 || agent.AllowedTargets[0] == node.PrivateIP || agent.Listen != node.PrivateIP+":31880" {
			t.Fatal("旧节点的目标白名单没有随新增节点更新")
		}
		if !contains(agent.Node.ReservedPorts, 22) || !contains(agent.Node.ReservedPorts, 31880) {
			t.Fatal("节点缺少管理保留端口")
		}
		leaf := certAt(t, filepath.Join(base, "server.pem"))
		if _, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: node.PrivateIP, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Fatal("新增叶子不属于原 CA：", err)
		}
		if _, err = tls.LoadX509KeyPair(filepath.Join(base, "server.pem"), filepath.Join(base, "server-key.pem")); err != nil {
			t.Fatal(err)
		}
		if _, err = leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
			t.Fatal("节点证书不能冒充控制台")
		}
	}
	if err = filepath.WalkDir(second, func(path string, entry fs.DirEntry, walkErr error) error {
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
			t.Errorf("导出权限错误：%s %o", path, info.Mode().Perm())
		}
		if strings.Contains(entry.Name(), "ca-key") || strings.Contains(entry.Name(), "password") {
			t.Errorf("配对导出夹带非叶子凭据：%s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckAgentRejectsWrongCAIdentityStateAndAddress(t *testing.T) {
	dir, path, _ := standaloneBundle(t)
	out := filepath.Join(filepath.Dir(dir), "paired")
	if err := addNode(addOptions(dir, path, out, testNodes()[0])); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(out, "agents", testNodes()[0].ID)
	configPath := filepath.Join(base, "agent.json")
	args := []string{"--config", configPath, "--credential-dir", base, "--skip-address-check"}
	if err := agent.CheckCommand(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(base, "ca.pem")
	originalCA, _ := os.ReadFile(caPath)
	other, _, _ := standaloneBundle(t)
	wrongCA, _ := os.ReadFile(filepath.Join(other, "authority/ca.pem"))
	if err := os.WriteFile(caPath, wrongCA, 0600); err != nil {
		t.Fatal(err)
	}
	if err := agent.CheckCommand(context.Background(), args); err == nil {
		t.Fatal("应拒绝错误 CA")
	}
	if err := os.WriteFile(caPath, originalCA, 0600); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(configPath)
	var cfg agentConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.Node.ID = "other-node"
	modified, _ := marshal(cfg)
	if err := os.WriteFile(configPath, modified, 0600); err != nil {
		t.Fatal(err)
	}
	if err := agent.CheckCommand(context.Background(), args); err == nil {
		t.Fatal("应拒绝证书 CN 与节点ID不符")
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(filepath.Dir(dir), "existing-agent-state.json")
	if err := os.WriteFile(state, []byte(`{"node_id":"wrong","revision":1,"rules":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := agent.CheckCommand(context.Background(), append(args, "--state-file", state)); err == nil {
		t.Fatal("应拒绝另一台节点的状态文件")
	}
	if err := agent.CheckCommand(context.Background(), args[:len(args)-1]); err == nil {
		t.Fatal("正常预检应拒绝本机没有的网卡/IP")
	}
}

func TestAddNodeRejectsMismatchedCAAndUnsafeInputs(t *testing.T) {
	dir, path, cfg := standaloneBundle(t)
	first := filepath.Join(filepath.Dir(dir), "first")
	if err := addNode(addOptions(dir, path, first, testNodes()[0])); err != nil {
		t.Fatal(err)
	}
	other, _, _ := standaloneBundle(t)
	cases := []struct {
		name   string
		change func(*nodeOptions)
	}{
		{"重复ID", func(o *nodeOptions) { o.node.ID = testNodes()[0].ID }},
		{"重复公网IP", func(o *nodeOptions) { o.node.PublicIP = testNodes()[0].PublicIP }},
		{"重复私网IP", func(o *nodeOptions) { o.node.PrivateIP = testNodes()[0].PrivateIP }},
		{"网卡注入", func(o *nodeOptions) { o.node.PrivateInterface = "eth1;echo" }},
		{"错误CA", func(o *nodeOptions) { o.authorityDir = filepath.Join(other, "authority") }},
		{"SSH端口", func(o *nodeOptions) { o.agentPort = 22 }},
		{"缺少CA", func(o *nodeOptions) { o.authorityDir = "" }},
		{"超长名称", func(o *nodeOptions) { o.node.Name = strings.Repeat("字", 81) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := filepath.Join(filepath.Dir(dir), "reject-"+tc.name)
			o := addOptions(dir, filepath.Join(first, "controller.json"), out, testNodes()[1])
			tc.change(&o)
			if err := addNode(o); err == nil {
				t.Fatal("应拒绝无效配对请求")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("非法输入不应留下部分配对资料")
			}
		})
	}
	// 公开权限与符号链接都不能让 CA 私钥被用来生成配对资料。
	if err := os.Chmod(filepath.Join(dir, "authority"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := addNode(addOptions(dir, path, filepath.Join(filepath.Dir(dir), "public-ca"), testNodes()[1])); err == nil {
		t.Fatal("应拒绝非私密 CA 目录")
	}
	if err := os.Chmod(filepath.Join(dir, "authority"), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(dir), "controller-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := addNode(addOptions(dir, link, filepath.Join(filepath.Dir(dir), "symlink"), testNodes()[1])); err == nil {
		t.Fatal("应拒绝配置符号链接")
	}
	// 显式叶子目录只解决隔离根路径，仍必须验证同一套 CA。
	cfg.CAFile, cfg.CertFile, cfg.KeyFile = "/missing/ca", "/missing/cert", "/missing/key"
	data, _ := marshal(cfg)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	o := addOptions(dir, path, filepath.Join(filepath.Dir(dir), "override"), testNodes()[0])
	o.credentialDir = filepath.Join(dir, "controller")
	if err := addNode(o); err != nil {
		t.Fatal("显式叶子路径应能用于隔离验证：", err)
	}
}
