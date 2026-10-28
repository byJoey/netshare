package control

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/crypto/bcrypt"
	"netshare/internal/model"
)

type Config struct {
	Listen       string       `json:"listen"`
	ExternalURL  string       `json:"external_url"`
	AccessPath   string       `json:"access_path,omitempty"`
	Username     string       `json:"username"`
	PasswordHash string       `json:"password_hash"`
	StateFile    string       `json:"state_file"`
	CAFile       string       `json:"ca_file"`
	CertFile     string       `json:"cert_file"`
	KeyFile      string       `json:"key_file"`
	CookieSecure bool         `json:"cookie_secure"`
	Nodes        []model.Node `json:"nodes"`
}

func LoadConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("读取控制台配置失败: %w", err)
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("控制台配置无效: %w", err)
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return cfg, fmt.Errorf("配置文件中存在多余内容")
	}
	if !filepath.IsAbs(cfg.StateFile) {
		return cfg, fmt.Errorf("状态文件必须使用绝对路径")
	}
	if cfg.Listen == "" || cfg.Username == "" || cfg.StateFile == "" {
		return cfg, fmt.Errorf("控制台监听、用户名和状态路径不能为空")
	}
	if _, err = bcrypt.Cost([]byte(cfg.PasswordHash)); err != nil {
		return cfg, fmt.Errorf("管理员密码摘要无效")
	}
	u, err := url.Parse(cfg.ExternalURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" && u.Path != "/" {
		return cfg, fmt.Errorf("external_url 必须是完整的 HTTP(S) 站点地址")
	}
	if err = model.ValidateAccessPath(cfg.AccessPath); err != nil {
		return cfg, err
	}
	if cfg.CookieSecure && u.Scheme != "https" {
		return cfg, fmt.Errorf("线上安全会话必须使用 HTTPS external_url")
	}
	host, port, err := net.SplitHostPort(cfg.Listen)
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		return cfg, fmt.Errorf("控制台需要明确的 IP 监听地址")
	}
	if !ip.IsLoopback() {
		p, portErr := strconv.Atoi(port)
		if ip.To4() == nil || ip.IsMulticast() || portErr != nil || p < 1 || p > 65535 || u.Scheme != "http" || cfg.CookieSecure {
			return cfg, fmt.Errorf("公网 IPv4 直连必须使用 HTTP external_url 和非 Secure 会话；HTTPS 反向代理模式请监听回环地址")
		}
	}
	// 空控制台先完成安全安装，配对两台机器后即可创建映射。
	if len(cfg.Nodes) > 32 {
		return cfg, fmt.Errorf("最多配置 32 台机器")
	}
	seen := map[string]bool{}
	ips := map[string]bool{}
	for _, n := range cfg.Nodes {
		if err = model.ValidateNode(n); err != nil {
			return cfg, err
		}
		if seen[n.ID] || ips[n.PrivateIP] {
			return cfg, fmt.Errorf("机器 ID 与私网地址不能重复")
		}
		seen[n.ID] = true
		ips[n.PrivateIP] = true
		a, e := url.Parse(n.AgentURL)
		if e != nil || a.Scheme != "https" || a.Hostname() != n.PrivateIP || a.User != nil || a.RawQuery != "" || a.Fragment != "" || a.Path != "" && a.Path != "/" {
			return cfg, fmt.Errorf("机器 %s 的代理地址必须使用其私网 IP 和 HTTPS", n.Name)
		}
	}
	return cfg, nil
}

func agentHTTPClient(cfg Config) (*http.Client, error) {
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("代理 CA 无效")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}}, DialContext: (&net.Dialer{Timeout: 4 * time.Second, KeepAlive: 30 * time.Second}).DialContext, MaxIdleConns: 64, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second, ResponseHeaderTimeout: 18 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("代理不允许重定向") }}, nil
}
