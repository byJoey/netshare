package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"netshare/internal/model"
	"netshare/internal/registration"
)

func joinFixture(t *testing.T) (*Controller, *httptest.Server, string) {
	t.Helper()
	c, _ := testController(t)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer := filepath.Join(dir, "signer")
	if err = registration.InitSigner(signer); err != nil {
		t.Fatal(err)
	}
	for i := range c.cfg.Nodes {
		c.cfg.Nodes[i].AgentURL = "https://" + net.JoinHostPort(c.cfg.Nodes[i].PrivateIP, "31880")
	}
	c.cfg.StateFile = filepath.Join(dir, "state.json")
	c.cfg.CAFile = filepath.Join(signer, "ca.pem")
	c.cfg.RegistrationCAFile = c.cfg.CAFile
	c.cfg.RegistrationSocket = filepath.Join(dir, "sign.sock")
	c.cfg.AccessPath = "/ns-test0123456789abcdef"
	c.configPath = filepath.Join(dir, "controller.json")
	c.signNode = func(_ context.Context, req registration.SignRequest) (registration.SignReply, error) {
		return registration.SignCertificate(signer, req)
	}
	server := httptest.NewServer(c.Handler(nil))
	t.Cleanup(server.Close)
	c.cfg.ExternalURL = server.URL
	if err = atomicJSON(c.configPath, c.cfg); err != nil {
		t.Fatal(err)
	}
	return c, server, signer
}
func reportFor(t *testing.T, ip string) (registration.Report, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, _ := registration.KeyID(&key.PublicKey)
	node := model.Node{ID: "node-" + kid[:24], Name: "自动发现的服务器", PublicIP: "203.0.113.80", PrivateIP: ip, PublicInterface: "eth7", PrivateInterface: "eth8"}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: node.ID}, IPAddresses: []net.IP{net.ParseIP(ip)}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return registration.Report{Node: node, CSR: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})}, key
}
func createTicket(t *testing.T, c *Controller, server *httptest.Server) joinTicket {
	t.Helper()
	id, session := c.auth.create(c.cfg.Username)
	req, _ := http.NewRequest("POST", server.URL+c.cfg.AccessPath+"/api/node-joins", nil)
	req.AddCookie(&http.Cookie{Name: "netshare_session", Value: id})
	req.Header.Set("X-CSRF-Token", session.CSRF)
	req.Header.Set("Origin", server.URL)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		ID      string `json:"id"`
		Command string `json:"command"`
	}
	_ = json.NewDecoder(res.Body).Decode(&out)
	if res.StatusCode != 200 || out.ID == "" || !strings.Contains(out.Command, " install agent --join '") {
		t.Fatalf("生成命令失败 HTTP %d", res.StatusCode)
	}
	ticket, err := c.readTicket(out.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}
func ticketClient(t *testing.T, server *httptest.Server, c *Controller, ticket joinTicket) *registration.Client {
	t.Helper()
	client, err := registration.NewClient(server.URL + c.cfg.AccessPath + "/api/join/" + ticket.ID + "#" + ticket.Secret)
	if err != nil {
		t.Fatal(err)
	}
	return client
}
func TestJoinCommandRequiresSessionAndCSRF(t *testing.T) {
	c, server, _ := joinFixture(t)
	res, err := server.Client().Post(server.URL+c.cfg.AccessPath+"/api/node-joins", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("允许未登录生成命令")
	}
	id, _ := c.auth.create(c.cfg.Username)
	req, _ := http.NewRequest("POST", server.URL+c.cfg.AccessPath+"/api/node-joins", nil)
	req.AddCookie(&http.Cookie{Name: "netshare_session", Value: id})
	req.Header.Set("Origin", server.URL)
	res, err = server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("允许无CSRF生成命令")
	}
	ticket := createTicket(t, c, server)
	data, err := os.ReadFile(c.ticketPath(ticket.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "PRIVATE KEY") {
		t.Fatal("服务器保存了节点私钥")
	}
	info, _ := os.Stat(c.ticketPath(ticket.ID))
	if info.Mode().Perm() != 0600 {
		t.Fatal("票据权限错误")
	}
}
func TestJoinIsRetryableAndBoundToOneNode(t *testing.T) {
	c, server, _ := joinFixture(t)
	ticket := createTicket(t, c, server)
	client := ticketClient(t, server, c, ticket)
	report, _ := reportFor(t, "10.50.3.21")
	var first, again registration.Reply
	if err := client.Request(context.Background(), "register", report, &first); err != nil {
		t.Fatal(err)
	}
	if err := client.Request(context.Background(), "register", report, &again); err != nil {
		t.Fatal("重复执行失败", err)
	}
	if string(first.Certificate) != string(again.Certificate) || len(c.nodes()) != 3 {
		t.Fatal("重跑造成重复节点或重发身份")
	}
	disk, err := LoadConfig(c.configPath)
	if err != nil || len(disk.Nodes) != 3 {
		t.Fatal("未持久登记", err)
	}
	other, _ := reportFor(t, "10.50.3.22")
	if client.Request(context.Background(), "register", other, &again) == nil {
		t.Fatal("其他机器重用了命令")
	}
	ticket, _ = c.readTicket(ticket.ID, false)
	ticket.Phase = "revoked"
	_ = c.saveTicket(ticket)
	if client.Request(context.Background(), "register", report, &again) == nil {
		t.Fatal("撤销命令仍可用")
	}
	if len(c.nodes()) != 3 {
		t.Fatal("撤销命令误删已登记节点")
	}
}
func TestConcurrentJoinTicketOnlyAdmitsOneKey(t *testing.T) {
	c, server, _ := joinFixture(t)
	ticket := createTicket(t, c, server)
	reports := make([]registration.Report, 12)
	for i := range reports {
		reports[i], _ = reportFor(t, fmt.Sprintf("10.50.3.%d", 40+i))
	}
	var successes atomic.Int32
	var wg sync.WaitGroup
	for _, r := range reports {
		wg.Add(1)
		go func(report registration.Report) {
			defer wg.Done()
			client, _ := registration.NewClient(server.URL + c.cfg.AccessPath + "/api/join/" + ticket.ID + "#" + ticket.Secret)
			var reply registration.Reply
			if client.Request(context.Background(), "register", report, &reply) == nil {
				successes.Add(1)
			}
		}(r)
	}
	wg.Wait()
	if successes.Load() != 1 || len(c.nodes()) != 3 {
		t.Fatalf("同一命令接入了 %d 台", successes.Load())
	}
}
func TestExpiredAndForgedNodeIdentityAreRejected(t *testing.T) {
	c, server, _ := joinFixture(t)
	ticket := createTicket(t, c, server)
	client := ticketClient(t, server, c, ticket)
	report, _ := reportFor(t, "10.50.3.66")
	report.Node.ID = "a"
	var reply registration.Reply
	if client.Request(context.Background(), "register", report, &reply) == nil {
		t.Fatal("伪造已有节点ID")
	}
	ticket.ExpiresAt = time.Now().Add(-time.Second)
	_ = c.saveTicket(ticket)
	var info registration.Info
	if client.Request(context.Background(), "info", struct{}{}, &info) == nil {
		t.Fatal("过期命令仍有效")
	}
	if len(c.nodes()) != 2 {
		t.Fatal("失败接入修改拓扑")
	}
}
