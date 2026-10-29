package registration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"netshare/internal/model"
)

func TestEnvelopeAndInvitationBoundaries(t *testing.T) {
	id, secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	invite := "http://203.0.113.2:18440/ns-0123456789abcdef/api/join/" + id + "#" + secret
	endpoint, key, err := ParseInvitation(invite)
	if err != nil || key != secret || strings.Contains(endpoint, "#") {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(invite, "http:", "file:", 1), invite + "\n", strings.Replace(invite, "/api/join/", "/api/join/../", 1), strings.Replace(invite, "/api/join/", "/%61pi/join/", 1), strings.Replace(invite, "203.0.113.2", "user@203.0.113.2", 1), invite[:len(invite)-1]} {
		if _, _, e := ParseInvitation(bad); e == nil {
			t.Fatal("错误接入码被接受")
		}
	}
	box, err := Seal(secret, id+":register", map[string]string{"value": "服务器私密信息"})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(box)
	if bytes.Contains(data, []byte("服务器私密信息")) || bytes.Contains(data, []byte(secret)) {
		t.Fatal("密文泄露信息")
	}
	var out map[string]string
	if err = Open(secret, id+":register", box, &out); err != nil || out["value"] != "服务器私密信息" {
		t.Fatal(err)
	}
	if Open(secret, id+":info", box, &out) == nil {
		t.Fatal("跨操作重放")
	}
	_, other, _ := NewSecret()
	if Open(other, id+":register", box, &out) == nil {
		t.Fatal("错误密钥")
	}
	box.Data[0] ^= 1
	if Open(secret, id+":register", box, &out) == nil {
		t.Fatal("篡改密文")
	}
}

func TestNetworkNeedsNoManualFields(t *testing.T) {
	inventory := []Interface{{Name: "eth7", Addresses: []string{"203.0.113.83/24"}}, {Name: "eth8", Addresses: []string{"10.42.0.10/22"}}, {Name: "docker0", Addresses: []string{"172.17.0.1/16"}}}
	node, err := SelectNetwork(inventory, "eth7", "a51c467b-6a5c-4f75-8f95-9411d437e37e", NetworkOptions{})
	if err != nil || node.PrivateIP != "10.42.0.10" || node.PublicIP != "203.0.113.83" || node.Name != "节点 83" {
		t.Fatalf("自动识别: %+v %v", node, err)
	}
	inventory = append(inventory, Interface{Name: "ens9", Addresses: []string{"192.168.99.2/24"}})
	if _, err = SelectNetwork(inventory, "eth7", "test", NetworkOptions{}); err == nil {
		t.Fatal("歧义被静默猜测")
	}
	node, err = SelectNetwork(inventory, "eth7", "test", NetworkOptions{Peers: []string{"10.42.0.11"}})
	if err != nil || node.PrivateInterface != "eth8" {
		t.Fatal("未根据现有节点网段自动选择", err)
	}
	if _, err = SelectNetwork(inventory[:1], "eth7", "test", NetworkOptions{}); err == nil {
		t.Fatal("没有私网仍接入")
	}
	inventory[0].Addresses = []string{"192.168.1.2/24"}
	if _, err = SelectNetwork(inventory, "eth7", "test", NetworkOptions{}); err == nil {
		t.Fatal("NAT无来源仍伪造公网")
	}
	node, err = SelectNetwork(inventory, "eth7", "test", NetworkOptions{SourceIP: "203.0.113.88", Peers: []string{"10.42.0.11"}})
	if err != nil || node.PublicIP != "203.0.113.88" {
		t.Fatal("NAT来源识别失败", err)
	}
}

func testIssuer(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err = InitSigner(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}
func testCSR(t *testing.T, node model.Node) ([]byte, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	data, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: node.ID}, IPAddresses: []net.IP{net.ParseIP(node.PrivateIP)}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: data}), key
}
func TestSignerCannotIssueControllerOrPublicIdentity(t *testing.T) {
	dir := testIssuer(t)
	before, _ := os.ReadFile(filepath.Join(dir, "ca-key.pem"))
	if err := InitSigner(dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "ca-key.pem"))
	if !bytes.Equal(before, after) {
		t.Fatal("重新初始化更换了CA")
	}
	node := model.Node{ID: "test-node", Name: "测试", PublicIP: "203.0.113.3", PrivateIP: "10.50.3.2", PublicInterface: "eth7", PrivateInterface: "eth8"}
	csr, _ := testCSR(t, node)
	reply, err := SignCertificate(dir, SignRequest{Node: node, CSR: csr})
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(reply.Certificate)
	cert, _ := x509.ParseCertificate(block.Bytes)
	root, _ := os.ReadFile(filepath.Join(dir, "ca.pem"))
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(root)
	if _, err = cert.Verify(x509.VerifyOptions{Roots: pool, DNSName: node.PrivateIP, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, err = cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err == nil {
		t.Fatal("节点证书能冒充控制器")
	}
	node.PrivateIP = "1.1.1.1"
	csr, _ = testCSR(t, node)
	if _, err = SignCertificate(dir, SignRequest{Node: node, CSR: csr}); err == nil {
		t.Fatal("签发了公网证书")
	}
	if err = os.Symlink(filepath.Join(dir, "ca-key.pem"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err = privateRead(filepath.Join(dir, "link")); err == nil {
		t.Fatal("跟随私钥符号链接")
	}
}

func TestClientKeepsKeyOutOfRequestsAndRefusesRedirects(t *testing.T) {
	id, secret, _ := NewSecret()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.RequestURI, secret) || r.URL.Fragment != "" || r.Header.Get("Cookie") != "" {
			t.Error("请求泄漏接入密钥")
		}
		var box Envelope
		_ = json.NewDecoder(r.Body).Decode(&box)
		var empty struct{}
		if Open(secret, id+":info", box, &empty) != nil {
			t.Error("加密请求无效")
		}
		out, _ := Seal(secret, id+":info:response", Info{Peers: []string{"10.0.0.1"}})
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	client, err := NewClient(server.URL + "/api/join/" + id + "#" + secret)
	if err != nil {
		t.Fatal(err)
	}
	var info Info
	if err = client.Request(context.Background(), "info", struct{}{}, &info); err != nil || !reflect.DeepEqual(info.Peers, []string{"10.0.0.1"}) {
		t.Fatal(err)
	}
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL, 307) }))
	defer redirect.Close()
	client, _ = NewClient(redirect.URL + "/api/join/" + id + "#" + secret)
	if client.Request(context.Background(), "info", struct{}{}, &info) == nil {
		t.Fatal("跟随重定向")
	}
}
