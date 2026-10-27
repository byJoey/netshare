package control

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
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"netshare/internal/model"
)

type contractPKI struct {
	ca     *x509.Certificate
	key    *ecdsa.PrivateKey
	serial int64
}

func newContractPKI(t *testing.T) *contractPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "契约测试 CA"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &contractPKI{ca: ca, key: key, serial: 1}
}

func (p *contractPKI) issue(t *testing.T, cn string, usage x509.ExtKeyUsage, serverIP string) tls.Certificate {
	t.Helper()
	p.serial++
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(p.serial), Subject: pkix.Name{CommonName: cn}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
	if serverIP != "" {
		template.IPAddresses = []net.IP{net.ParseIP(serverIP)}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func (p *contractPKI) clientConfig(t *testing.T, certificate tls.Certificate) Config {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{CAFile: filepath.Join(dir, "ca.pem"), CertFile: filepath.Join(dir, "controller.pem"), KeyFile: filepath.Join(dir, "controller-key.pem")}
	keyDER, err := x509.MarshalPKCS8PrivateKey(certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct {
		path string
		data []byte
	}{{cfg.CAFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: p.ca.Raw})}, {cfg.CertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Certificate[0]})}, {cfg.KeyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})}} {
		if err = os.WriteFile(file.path, file.data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return cfg
}

func (p *contractPKI) server(t *testing.T, serverIP string, handler http.Handler) *httptest.Server {
	t.Helper()
	roots := x509.NewCertPool()
	roots.AddCert(p.ca)
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{p.issue(t, "node-contract", x509.ExtKeyUsageServerAuth, serverIP)}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots, VerifyConnection: func(state tls.ConnectionState) error {
		if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 || state.PeerCertificates[0].Subject.CommonName != "controller" {
			return fmt.Errorf("客户端证书不属于控制台")
		}
		return nil
	}}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func closeContractClient(t *testing.T, client *http.Client) {
	t.Helper()
	t.Cleanup(func() {
		if transport, ok := client.Transport.(*http.Transport); ok {
			transport.CloseIdleConnections()
		}
	})
}

func TestRemoteAgentsMutualTLSContract(t *testing.T) {
	pki := newContractPKI(t)
	timestamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	rule := model.AgentRule{ID: "contract-rule", Name: "契约映射", Protocol: "both", ListenPort: "18080", TargetPort: "8080", TargetIP: "10.42.0.11", Enabled: true}
	var mu sync.Mutex
	state := model.ApplyRequest{}
	seen := map[string]int{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || len(r.TLS.PeerCertificates) == 0 || r.TLS.PeerCertificates[0].Subject.CommonName != "controller" {
			t.Error("没有使用经验证的 TLS 1.3 控制台身份")
			http.Error(w, "身份错误", 403)
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("代理请求未设置 JSON 内容类型")
		}
		mu.Lock()
		defer mu.Unlock()
		seen[r.Method+" "+r.URL.Path]++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/apply":
			if r.Method != "POST" {
				t.Error("apply方法错误")
			}
			if err := json.NewDecoder(r.Body).Decode(&state); err != nil {
				t.Error(err)
				http.Error(w, "格式错误", 400)
				return
			}
			_ = json.NewEncoder(w).Encode(model.ApplyResult{Revision: state.Revision, AppliedAt: timestamp})
		case "/v1/status":
			if r.Method != "GET" {
				t.Error("status方法错误")
			}
			_ = json.NewEncoder(w).Encode(model.AgentStatus{NodeID: "contract", Version: model.Version, Revision: state.Revision, SavedRevision: state.Revision, Rules: state.Rules, AppliedAt: timestamp, Forwarding: true, Interfaces: []model.InterfaceStats{{Name: "eth8", RXBytes: 123, TXBytes: 456}}, Warnings: []string{}})
		case "/v1/probe":
			var probe model.ProbeRequest
			if err := json.NewDecoder(r.Body).Decode(&probe); err != nil {
				t.Error(err)
			}
			if probe.TargetIP != "10.42.0.11" || probe.TargetPort != "8080" || probe.Protocol != "tcp" {
				t.Errorf("探测请求字段错误：%+v", probe)
			}
			_ = json.NewEncoder(w).Encode(model.ProbeResult{Reachable: true, LatencyMS: 7, Message: "已通过私网连接目标 TCP 端口", Interface: "eth8"})
		default:
			http.NotFound(w, r)
		}
	})
	server := pki.server(t, "127.0.0.1", handler)
	client, err := agentHTTPClient(pki.clientConfig(t, pki.issue(t, "controller", x509.ExtKeyUsageClientAuth, "")))
	if err != nil {
		t.Fatal(err)
	}
	closeContractClient(t, client)
	agents := &remoteAgents{client: client}
	node := model.Node{ID: "contract", Name: "契约节点", AgentURL: server.URL + "/"}
	applied, err := agents.Apply(context.Background(), node, model.ApplyRequest{Revision: 41, Rules: []model.AgentRule{rule}})
	if err != nil {
		t.Fatal(err)
	}
	if applied.Revision != 41 || !applied.AppliedAt.Equal(timestamp) {
		t.Fatalf("应用响应解析错误：%+v", applied)
	}
	status, err := agents.Status(context.Background(), node)
	if err != nil {
		t.Fatal(err)
	}
	if status.NodeID != "contract" || status.Revision != 41 || status.SavedRevision != 41 || len(status.Rules) != 1 || status.Rules[0] != rule || !status.Forwarding || status.Interfaces[0].TXBytes != 456 {
		t.Fatalf("状态契约不匹配：%+v", status)
	}
	probe, err := agents.Probe(context.Background(), node, model.ProbeRequest{TargetIP: "10.42.0.11", TargetPort: "8080", Protocol: "tcp"})
	if err != nil {
		t.Fatal(err)
	}
	if !probe.Reachable || probe.Interface != "eth8" || probe.LatencyMS != 7 {
		t.Fatalf("探测响应错误：%+v", probe)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, request := range []string{"POST /v1/apply", "GET /v1/status", "POST /v1/probe"} {
		if seen[request] != 1 {
			t.Errorf("%s 次数不正确：%d", request, seen[request])
		}
	}
}

func TestRemoteAgentsTLSRejectsWrongIdentityAndServerSAN(t *testing.T) {
	pki := newContractPKI(t)
	server := pki.server(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"node_id":"contract"}`) }))
	wrongClient, err := agentHTTPClient(pki.clientConfig(t, pki.issue(t, "another-node", x509.ExtKeyUsageClientAuth, "")))
	if err != nil {
		t.Fatal(err)
	}
	closeContractClient(t, wrongClient)
	if _, err = (&remoteAgents{client: wrongClient}).Status(context.Background(), model.Node{Name: "契约节点", AgentURL: server.URL}); err == nil {
		t.Fatal("代理接受了非控制台客户端证书")
	}
	wrongSAN := pki.server(t, "192.0.2.1", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }))
	client, err := agentHTTPClient(pki.clientConfig(t, pki.issue(t, "controller", x509.ExtKeyUsageClientAuth, "")))
	if err != nil {
		t.Fatal(err)
	}
	closeContractClient(t, client)
	if _, err = (&remoteAgents{client: client}).Status(context.Background(), model.Node{Name: "契约节点", AgentURL: wrongSAN.URL}); err == nil {
		t.Fatal("控制台未验证代理证书IP SAN")
	}
}

func TestRemoteAgentsErrorsResponseLimitAndRedirect(t *testing.T) {
	pki := newContractPKI(t)
	server := pki.server(t, "127.0.0.1", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")[0] {
		case "conflict":
			w.WriteHeader(409)
			_, _ = io.WriteString(w, `{"error":"入口端口被占用"}`)
		case "fallback":
			w.WriteHeader(503)
			_, _ = io.WriteString(w, "unavailable")
		case "malformed":
			_, _ = io.WriteString(w, `{"node_id":`)
		case "large":
			_, _ = io.WriteString(w, `{"node_id":"`+strings.Repeat("x", (2<<20)+16)+`"}`)
		case "redirect":
			http.Redirect(w, r, "/landing", 307)
		default:
			_, _ = io.WriteString(w, `{"node_id":"unexpected"}`)
		}
	}))
	client, err := agentHTTPClient(pki.clientConfig(t, pki.issue(t, "controller", x509.ExtKeyUsageClientAuth, "")))
	if err != nil {
		t.Fatal(err)
	}
	closeContractClient(t, client)
	agents := &remoteAgents{client: client}
	for _, item := range []struct{ path, want string }{{"conflict", "入口端口被占用"}, {"fallback", "HTTP 503"}, {"malformed", "代理响应无效"}, {"large", "代理响应无效"}, {"redirect", "代理不允许重定向"}} {
		t.Run(item.path, func(t *testing.T) {
			_, err := agents.Status(context.Background(), model.Node{Name: "契约节点", AgentURL: server.URL + "/" + item.path})
			if err == nil || !strings.Contains(err.Error(), item.want) {
				t.Fatalf("错误信息不符合契约：%v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := agents.Status(ctx, model.Node{Name: "契约节点", AgentURL: server.URL}); err == nil {
		t.Fatal("已取消的请求仍继续执行")
	}
}

type savedRevisionAgents struct {
	mu       sync.Mutex
	requests map[string]model.ApplyRequest
}

func (a *savedRevisionAgents) Status(_ context.Context, n model.Node) (model.AgentStatus, error) {
	revision := uint64(34)
	if n.ID == "b" {
		revision = 47
	}
	return model.AgentStatus{NodeID: n.ID, Revision: 0, SavedRevision: revision, Rules: []model.AgentRule{}, Warnings: []string{"内核规则已丢失"}}, nil
}
func (a *savedRevisionAgents) Apply(_ context.Context, n model.Node, request model.ApplyRequest) (model.ApplyResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if request.Revision <= 47 {
		return model.ApplyResult{}, fmt.Errorf("旧备份修订号必须先对齐磁盘保存版本")
	}
	a.requests[n.ID] = request
	return model.ApplyResult{Revision: request.Revision, AppliedAt: time.Now()}, nil
}
func (a *savedRevisionAgents) Probe(context.Context, model.Node, model.ProbeRequest) (model.ProbeResult, error) {
	return model.ProbeResult{}, nil
}

func TestReconcileOldBackupUsesSavedRevisionWhenKernelRulesMissing(t *testing.T) {
	c, _ := testController(t)
	c.state = State{Revision: 2, Mappings: []model.Mapping{testMapping("restore-rule", "a", "b", "18080")}, Events: []model.AuditEvent{}}
	agents := &savedRevisionAgents{requests: map[string]model.ApplyRequest{}}
	c.agents = agents
	if err := c.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.snapshot().Revision != 48 {
		t.Fatalf("修订号未超过节点磁盘保存版本：%d", c.snapshot().Revision)
	}
	agents.mu.Lock()
	defer agents.mu.Unlock()
	if agents.requests["a"].Revision != 48 || agents.requests["b"].Revision != 48 || len(agents.requests["a"].Rules) != 1 || len(agents.requests["b"].Rules) != 0 {
		t.Fatalf("恢复下发错误：%+v", agents.requests)
	}
	disk, err := loadState(c.cfg.StateFile)
	if err != nil || disk.Revision != 48 || len(disk.Mappings) != 1 {
		t.Fatalf("修订号与映射未持久化：%+v %v", disk, err)
	}
}
