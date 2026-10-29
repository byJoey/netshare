package registration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"netshare/internal/model"
)

type Client struct {
	Endpoint string
	Secret   string
	HTTP     *http.Client
}

func NewClient(invitation string) (*Client, error) {
	endpoint, secret, err := ParseInvitation(invitation)
	if err != nil {
		return nil, err
	}
	return &Client{Endpoint: endpoint, Secret: secret, HTTP: &http.Client{Timeout: 100 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13}, DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("接入服务不接受重定向") }}}, nil
}

func (c *Client) Request(ctx context.Context, action string, input, output any) error {
	id := filepath.Base(c.Endpoint)
	box, err := Seal(c.Secret, id+":"+action, input)
	if err != nil {
		return err
	}
	data, _ := json.Marshal(box)
	req, err := http.NewRequestWithContext(ctx, "POST", c.Endpoint+"/"+action, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("接入地址无效")
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("连接控制台失败，请确认管理端口可达后重试")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 192<<10))
	if err != nil {
		return fmt.Errorf("读取接入结果失败，请重试同一条命令")
	}
	if res.StatusCode != 200 {
		var detail struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &detail) == nil && detail.Error != "" {
			return fmt.Errorf("%s", detail.Error)
		}
		return fmt.Errorf("接入未完成，请回面板查看状态或重新生成命令")
	}
	if err := Decode(body, &box); err != nil {
		return err
	}
	return Open(c.Secret, id+":"+action+":response", box, output)
}

func publicBundle(info Info) ([]byte, *x509.CertPool, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(info.ControllerCA) || !pool.AppendCertsFromPEM(info.IssuerCA) {
		return nil, nil, fmt.Errorf("控制台信任资料不完整")
	}
	bundle := append(append([]byte{}, info.ControllerCA...), info.IssuerCA...)
	return bundle, pool, nil
}

func keyAndPrevious(dir, existing string, roots *x509.CertPool) (*ecdsa.PrivateKey, []byte, string, error) {
	if data, err := privateRead(filepath.Join(dir, "server-key.pem")); err == nil {
		key, e := parseKey(data)
		return key, nil, "", e
	} else if _, statErr := os.Stat(filepath.Join(dir, "server-key.pem")); statErr == nil || !os.IsNotExist(statErr) {
		return nil, nil, "", err
	}
	if existing != "" {
		if _, err := os.Stat(filepath.Join(existing, "agent.json")); err == nil {
			certData, err := privateRead(filepath.Join(existing, "server.pem"))
			if err != nil {
				return nil, nil, "", err
			}
			keyData, err := privateRead(filepath.Join(existing, "server-key.pem"))
			if err != nil {
				return nil, nil, "", err
			}
			pair, err := tls.X509KeyPair(certData, keyData)
			if err != nil || len(pair.Certificate) == 0 {
				return nil, nil, "", fmt.Errorf("本机已有节点资料不完整，已保留原配置")
			}
			cert, err := x509.ParseCertificate(pair.Certificate[0])
			if err != nil {
				return nil, nil, "", err
			}
			if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
				return nil, nil, "", fmt.Errorf("本机已接入另一控制台，不会覆盖现有节点")
			}
			key, err := parseKey(keyData)
			return key, certData, cert.Subject.CommonName, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	return key, nil, "", err
}

func parseKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("节点私钥格式无效")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*ecdsa.PrivateKey)
	if err != nil || !ok || key.Curve != elliptic.P256() {
		return nil, fmt.Errorf("节点私钥格式无效")
	}
	return key, nil
}

func Join(ctx context.Context, client *Client, dir, existing string, opt NetworkOptions) (model.Node, error) {
	if err := SafePath(dir); err != nil {
		return model.Node{}, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return model.Node{}, err
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm()&0077 != 0 {
		return model.Node{}, fmt.Errorf("接入缓存目录必须仅root可访问")
	}
	h := sha256.Sum256([]byte(client.Endpoint + "#" + client.Secret))
	binding := hex.EncodeToString(h[:])
	identityFile := filepath.Join(dir, "identity.json")
	var identity struct {
		Binding  string `json:"binding"`
		ID       string `json:"id"`
		Previous []byte `json:"previous,omitempty"`
	}
	if data, err := privateRead(identityFile); err == nil {
		if Decode(data, &identity) != nil || identity.Binding != binding {
			return model.Node{}, fmt.Errorf("接入缓存属于另一条命令，已保留原资料")
		}
	} else if _, err := os.Stat(identityFile); err == nil || !os.IsNotExist(err) {
		return model.Node{}, fmt.Errorf("无法安全读取接入缓存")
	}
	var info Info
	if err := client.Request(ctx, "info", struct{}{}, &info); err != nil {
		return model.Node{}, err
	}
	bundle, roots, err := publicBundle(info)
	if err != nil {
		return model.Node{}, err
	}
	opt.Peers, opt.SourceIP = info.Peers, info.SourceIP
	node, err := DetectNetwork(opt)
	if err != nil {
		var problem *NetworkProblem
		if errors.As(err, &problem) {
			if problem.Code == "ambiguous_private" {
				if tty, openErr := os.OpenFile("/dev/tty", os.O_RDWR, 0); openErr == nil {
					fmt.Fprintln(tty, "检测到多张私网网卡，请选择连接其他节点的那一张：")
					for i, n := range problem.Candidates {
						fmt.Fprintf(tty, "%d  %s  %s\n", i+1, n.PrivateInterface, n.PrivateIP)
					}
					fmt.Fprint(tty, "选择编号：")
					var choice int
					_, readErr := fmt.Fscanln(tty, &choice)
					tty.Close()
					if readErr == nil && choice >= 1 && choice <= len(problem.Candidates) {
						opt.PrivateInterface, opt.PrivateIP = problem.Candidates[choice-1].PrivateInterface, problem.Candidates[choice-1].PrivateIP
						node, err = DetectNetwork(opt)
					}
				}
			}
			if err != nil {
				var out struct{}
				_ = client.Request(ctx, "problem", struct {
					Code string `json:"code"`
				}{problem.Code}, &out)
			}
		}
		if err != nil {
			return model.Node{}, err
		}
	}
	key, previous, oldID, err := keyAndPrevious(dir, existing, roots)
	if err != nil {
		return model.Node{}, err
	}
	keyID, err := KeyID(&key.PublicKey)
	if err != nil {
		return model.Node{}, err
	}
	node.ID = "node-" + keyID[:24]
	if oldID != "" {
		node.ID = oldID
	}
	if identity.ID != "" {
		node.ID, previous = identity.ID, identity.Previous
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return model.Node{}, err
	}
	if err = WritePrivate(filepath.Join(dir, "server-key.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})); err != nil {
		return model.Node{}, err
	}
	identity.Binding, identity.ID, identity.Previous = binding, node.ID, previous
	data, _ := json.Marshal(identity)
	if err = WritePrivate(identityFile, data); err != nil {
		return model.Node{}, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: node.ID}, IPAddresses: []net.IP{net.ParseIP(node.PrivateIP)}}, key)
	if err != nil {
		return model.Node{}, err
	}
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})
	var reply Reply
	if err = client.Request(ctx, "register", Report{Node: node, CSR: csr, PreviousCertificate: previous}, &reply); err != nil {
		return model.Node{}, err
	}
	if reply.Node.ID != node.ID || reply.Node.PrivateIP != node.PrivateIP || !bytes.Equal(reply.TrustBundle, bundle) {
		return model.Node{}, fmt.Errorf("接入结果与本机不匹配，已保留原配置")
	}
	pair, err := tls.X509KeyPair(reply.Certificate, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	if err != nil || len(pair.Certificate) != 1 {
		return model.Node{}, fmt.Errorf("节点证书与本机私钥不匹配")
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil || cert.Subject.CommonName != node.ID {
		return model.Node{}, fmt.Errorf("节点证书身份无效")
	}
	if _, err = cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: node.PrivateIP, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return model.Node{}, fmt.Errorf("节点证书验证失败")
	}
	cfg := map[string]any{"node": reply.Node, "listen": net.JoinHostPort(node.PrivateIP, "31880"), "ca_file": "/etc/netshare-agent/ca.pem", "cert_file": "/etc/netshare-agent/server.pem", "key_file": "/etc/netshare-agent/server-key.pem", "state_file": "/var/lib/netshare-agent/state.json", "controller_cn": "controller", "allowed_targets": reply.AllowedTargets}
	data, _ = json.MarshalIndent(cfg, "", "  ")
	for name, content := range map[string][]byte{"agent.json": data, "ca.pem": bundle, "server.pem": reply.Certificate} {
		if err = WritePrivate(filepath.Join(dir, name), content); err != nil {
			return model.Node{}, err
		}
	}
	return reply.Node, nil
}

func JoinCommand(ctx context.Context, args []string) error {
	if len(args) == 0 || (args[0] != "join" && args[0] != "complete") {
		return fmt.Errorf("用法：netshare node join|complete --invitation-stdin --dir 私密目录")
	}
	flags := flag.NewFlagSet("node "+args[0], flag.ContinueOnError)
	stdin := flags.Bool("invitation-stdin", false, "从标准输入读取完整接入码")
	dir := flags.String("dir", "", "root专用恢复目录")
	existing := flags.String("existing", "/etc/netshare-agent", "本机现有节点目录")
	var opt NetworkOptions
	flags.StringVar(&opt.PublicIP, "public-ip", "", "仅NAT等高级部署指定公网IP")
	flags.StringVar(&opt.PrivateInterface, "private-interface", "", "高级部署指定私网网卡")
	flags.StringVar(&opt.Name, "name", "", "可选节点名称")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if !*stdin || *dir == "" || flags.NArg() != 0 {
		return fmt.Errorf("需要接入码和私密恢复目录")
	}
	data, err := io.ReadAll(io.LimitReader(os.Stdin, 8193))
	if err != nil || len(data) > 8192 {
		return fmt.Errorf("接入码过长或无法读取")
	}
	client, err := NewClient(strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r"))
	if err != nil {
		return err
	}
	if args[0] == "complete" {
		var out struct {
			OK bool `json:"ok"`
		}
		return client.Request(ctx, "complete", struct{}{}, &out)
	}
	node, err := Join(ctx, client, *dir, *existing, opt)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stdout, "已识别本机并取得安装配置：%s。正在完成安装…\n", node.Name)
	return nil
}
