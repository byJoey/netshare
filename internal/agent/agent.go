// Package agent 通过私网中的双向 TLS 接收映射配置，只管理自身的 nftables 表。
package agent

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"netshare/internal/model"
)

const (
	maxBodyBytes       = 256 << 10
	maxRules           = 200
	maxExpandedPorts   = 2000
	defaultSysctlFile  = "/etc/sysctl.d/90-netshare-agent.conf"
	defaultForwardFile = "/proc/sys/net/ipv4/ip_forward"
)

type Config struct {
	Node           model.Node `json:"node"`
	Listen         string     `json:"listen"`
	CAFile         string     `json:"ca_file"`
	CertFile       string     `json:"cert_file"`
	KeyFile        string     `json:"key_file"`
	StateFile      string     `json:"state_file"`
	ControllerCN   string     `json:"controller_cn"`
	AllowedTargets []string   `json:"allowed_targets"`
}

// CommandRunner 不通过 shell 执行命令，测试可替换它来验证失败和回退。
type CommandRunner interface {
	Run(context.Context, string, []string, []byte) ([]byte, error)
}

type ExecRunner struct{}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, fmt.Errorf("命令输出超过限制")
	}
	return b.Buffer.Write(p)
}

func (ExecRunner) Run(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	output := &limitedBuffer{limit: 8 << 20}
	cmd.Stdout = output
	cmd.Stderr = output
	err := cmd.Run()
	if ctx.Err() != nil {
		return output.Bytes(), ctx.Err()
	}
	return output.Bytes(), err
}

type diskState struct {
	NodeID             string            `json:"node_id"`
	Revision           uint64            `json:"revision"`
	Rules              []model.AgentRule `json:"rules"`
	AppliedAt          time.Time         `json:"applied_at"`
	OriginalForwarding *bool             `json:"original_forwarding,omitempty"`
}

type Server struct {
	cfg            Config
	runner         CommandRunner
	mu             sync.Mutex
	state          diskState
	started        time.Time
	allowed        map[string]bool
	readFile       func(string) ([]byte, error)
	writeFile      func(string, []byte, os.FileMode) error
	removeFile     func(string) error
	forwardFile    string
	sysctlFile     string
	restoreWarning string
}

type operationError struct {
	status int
	err    error
}

func (e *operationError) Error() string { return e.err.Error() }
func bad(status int, format string, args ...any) error {
	return &operationError{status, fmt.Errorf(format, args...)}
}

func decodeStrict(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("JSON 后还有多余内容")
	}
	return nil
}

func NewServer(cfg Config, runner CommandRunner) (*Server, error) {
	if err := model.ValidateNode(cfg.Node); err != nil {
		return nil, err
	}
	if cfg.ControllerCN == "" {
		cfg.ControllerCN = "controller"
	}
	host, p, err := net.SplitHostPort(cfg.Listen)
	if err != nil || host != cfg.Node.PrivateIP {
		return nil, fmt.Errorf("节点代理必须只监听本机私网 IP")
	}
	port, err := strconv.Atoi(p)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("节点代理端口无效")
	}
	if !filepath.IsAbs(cfg.StateFile) {
		return nil, fmt.Errorf("节点状态文件必须使用绝对路径")
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	s := &Server{cfg: cfg, runner: runner, started: time.Now(), allowed: map[string]bool{}, readFile: os.ReadFile, writeFile: atomicWrite, removeFile: os.Remove, forwardFile: defaultForwardFile, sysctlFile: defaultSysctlFile}
	for _, ip := range cfg.AllowedTargets {
		a, e := netip.ParseAddr(ip)
		if e != nil || !a.Is4() || !a.IsPrivate() || a.String() != ip || ip == cfg.Node.PrivateIP {
			return nil, fmt.Errorf("允许的目标地址必须是其他机器的规范私网 IPv4")
		}
		s.allowed[ip] = true
	}
	if len(s.allowed) == 0 {
		return nil, fmt.Errorf("至少配置一个允许的私网目标")
	}
	s.state = diskState{NodeID: cfg.Node.ID, Rules: []model.AgentRule{}}
	data, err := os.ReadFile(cfg.StateFile)
	if err == nil {
		if len(data) > maxBodyBytes*2 {
			return nil, fmt.Errorf("节点状态文件过大")
		}
		if e := decodeStrict(data, &s.state); e != nil {
			return nil, fmt.Errorf("读取节点状态失败：%w", e)
		}
		if s.state.NodeID != cfg.Node.ID {
			return nil, fmt.Errorf("状态文件属于另一台机器")
		}
		if err := s.validateRules(s.state.Rules, nil); err != nil {
			return nil, fmt.Errorf("已保存的节点配置无效：%w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("读取节点状态失败：%w", err)
	}
	return s, nil
}

func (s *Server) command(ctx context.Context, name string, args []string, stdin []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	output, err := s.runner.Run(ctx, name, args, stdin)
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if len(detail) > 1000 {
			detail = detail[:1000]
		}
		if detail != "" {
			return output, fmt.Errorf("%s 执行失败：%v；%s", name, err, detail)
		}
		return output, fmt.Errorf("%s 执行失败：%w", name, err)
	}
	return output, nil
}

func (s *Server) validateRules(rules []model.AgentRule, listeners []model.Listener) error {
	if len(rules) > maxRules {
		return bad(400, "一台机器最多保存 %d 条映射", maxRules)
	}
	seen := map[string]bool{}
	expanded := 0
	_, agentPortText, _ := net.SplitHostPort(s.cfg.Listen)
	agentPort, _ := strconv.Atoi(agentPortText)
	reserved := append([]int{22, agentPort}, s.cfg.Node.ReservedPorts...)
	for i, r := range rules {
		if !model.ValidID(r.ID) || seen[r.ID] {
			return bad(400, "映射 ID 无效或重复")
		}
		seen[r.ID] = true
		if strings.TrimSpace(r.Name) == "" || utf8.RuneCountInString(r.Name) > 100 || strings.IndexFunc(r.Name, unicode.IsControl) >= 0 {
			return bad(400, "映射名称不能为空、超过 100 字或包含控制字符")
		}
		if err := model.ValidatePorts(r.Protocol, r.ListenPort, r.TargetPort); err != nil {
			return bad(400, "映射 %s：%v", r.Name, err)
		}
		if !s.allowed[r.TargetIP] {
			return bad(400, "目标 %s 不在本节点的私网白名单中", r.TargetIP)
		}
		p, _ := model.ParsePorts(r.ListenPort)
		for _, port := range reserved {
			if p.Contains(port) {
				return bad(409, "入口端口 %d 是保留端口，不能用于映射", port)
			}
		}
		if !r.Enabled {
			continue
		}
		expanded += (p.End - p.Start + 1) * len(model.Protocols(r.Protocol))
		if expanded > maxExpandedPorts {
			return bad(400, "启用的映射展开后最多包含 %d 个协议端口", maxExpandedPorts)
		}
		for _, l := range listeners {
			if model.ProtocolsOverlap(r.Protocol, l.Protocol) && p.Contains(l.Port) && publicListener(l.Address, s.cfg.Node.PublicIP) {
				return bad(409, "入口 %s/%d 已被本机服务 %s 占用", l.Protocol, l.Port, l.Process)
			}
		}
		for _, other := range rules[:i] {
			if !other.Enabled || !model.ProtocolsOverlap(r.Protocol, other.Protocol) {
				continue
			}
			q, _ := model.ParsePorts(other.ListenPort)
			if p.Overlaps(q) {
				return bad(409, "映射 %s 与 %s 的入口端口冲突", r.Name, other.Name)
			}
		}
	}
	return nil
}

func publicListener(addr, publicIP string) bool {
	if addr == "*" || addr == "0.0.0.0" || addr == "::" || addr == "" {
		return true
	}
	a, err := netip.ParseAddr(strings.Trim(addr, "[]"))
	if err != nil {
		return true
	}
	return a.Unmap().String() == publicIP
}

func canonicalRules(rules []model.AgentRule) []model.AgentRule {
	result := append([]model.AgentRule{}, rules...)
	for i := range result {
		p, e := model.ParsePorts(result[i].ListenPort)
		if e == nil {
			result[i].ListenPort = p.String()
		}
		p, e = model.ParsePorts(result[i].TargetPort)
		if e == nil {
			result[i].TargetPort = p.String()
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func hasEnabled(rules []model.AgentRule) bool {
	for _, r := range rules {
		if r.Enabled {
			return true
		}
	}
	return false
}

func (s *Server) Apply(ctx context.Context, req model.ApplyRequest) (model.ApplyResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyLocked(ctx, req, false)
}

func (s *Server) applyLocked(ctx context.Context, req model.ApplyRequest, restore bool) (model.ApplyResult, error) {
	if err := s.validateRules(req.Rules, nil); err != nil {
		return model.ApplyResult{}, err
	}
	req.Rules = canonicalRules(req.Rules)
	old := s.state
	if !restore && (req.Revision < old.Revision || (req.Revision == old.Revision && !reflect.DeepEqual(req.Rules, canonicalRules(old.Rules)))) {
		return model.ApplyResult{}, bad(409, "配置版本过旧或相同版本包含不同内容，请刷新后重试")
	}
	listeners, err := s.listeners(ctx)
	if err != nil {
		return model.ApplyResult{}, err
	}
	if err = s.validateRules(req.Rules, listeners); err != nil {
		return model.ApplyResult{}, err
	}
	if err = s.checkAddresses(ctx); err != nil {
		return model.ApplyResult{}, err
	}
	checked := map[string]bool{}
	for _, r := range req.Rules {
		if r.Enabled && !checked[r.TargetIP] {
			if err = s.checkRoute(ctx, r.TargetIP); err != nil {
				return model.ApplyResult{}, err
			}
			checked[r.TargetIP] = true
		}
	}
	if err = s.ensureOwnership(ctx); err != nil {
		return model.ApplyResult{}, err
	}
	next := diskState{NodeID: s.cfg.Node.ID, Revision: req.Revision, Rules: req.Rules, AppliedAt: time.Now().UTC(), OriginalForwarding: old.OriginalForwarding}
	var forwarding *forwardSnapshot
	if hasEnabled(req.Rules) {
		forwarding, err = s.enableForwarding()
		if err != nil {
			return model.ApplyResult{}, err
		}
		if next.OriginalForwarding == nil {
			v := strings.TrimSpace(string(forwarding.runtime)) == "1"
			next.OriginalForwarding = &v
		}
	}
	rollbackForward := func() error {
		if forwarding != nil {
			return s.restoreForwarding(forwarding)
		}
		return nil
	}
	script := renderRules(s.cfg.Node, next.Revision, next.Rules)
	if _, err = s.command(ctx, "nft", []string{"-c", "-f", "-"}, script); err != nil {
		return model.ApplyResult{}, combineRollback(err, rollbackForward())
	}
	if _, err = s.command(ctx, "nft", []string{"-f", "-"}, script); err != nil {
		return model.ApplyResult{}, combineRollback(err, rollbackForward())
	}
	data, _ := json.MarshalIndent(next, "", "  ")
	if err = s.writeFile(s.cfg.StateFile, append(data, '\n'), 0600); err != nil {
		// 状态写失败时，磁盘与内核都恢复到上一版，不能返回伪成功。
		_, kernelErr := s.command(context.WithoutCancel(ctx), "nft", []string{"-f", "-"}, renderRules(s.cfg.Node, old.Revision, old.Rules))
		oldData, _ := json.MarshalIndent(old, "", "  ")
		diskErr := s.writeFile(s.cfg.StateFile, append(oldData, '\n'), 0600)
		return model.ApplyResult{}, combineRollback(fmt.Errorf("保存节点配置失败：%w", err), errors.Join(kernelErr, diskErr, rollbackForward()))
	}
	s.state = next
	s.restoreWarning = ""
	return model.ApplyResult{Revision: next.Revision, AppliedAt: next.AppliedAt}, nil
}

func combineRollback(main, rollback error) error {
	if rollback != nil {
		return fmt.Errorf("%v；回退未完全成功，需检查节点：%w", main, rollback)
	}
	return main
}

// Restore 在开始接受控制请求前恢复上次已成功提交的映射。
func (s *Server) Restore(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.applyLocked(ctx, model.ApplyRequest{Revision: s.state.Revision, Rules: s.state.Rules}, true)
	if err != nil {
		s.restoreWarning = "启动恢复映射失败：" + err.Error()
	}
	return err
}

func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		var result any
		var err error
		switch r.URL.Path {
		case "/v1/status":
			if r.Method != http.MethodGet {
				writeError(w, bad(405, "此操作只接受 GET"))
				return
			}
			result, err = s.Status(r.Context())
		case "/v1/apply":
			if r.Method != http.MethodPost {
				writeError(w, bad(405, "此操作只接受 POST"))
				return
			}
			var req model.ApplyRequest
			if err = readJSON(w, r, &req); err == nil {
				result, err = s.Apply(r.Context(), req)
			}
		case "/v1/probe":
			if r.Method != http.MethodPost {
				writeError(w, bad(405, "此操作只接受 POST"))
				return
			}
			var req model.ProbeRequest
			if err = readJSON(w, r, &req); err == nil {
				result, err = s.Probe(r.Context(), req)
			}
		default:
			err = bad(404, "没有这个节点接口")
		}
		if err != nil {
			writeError(w, err)
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	})
}

func readJSON(w http.ResponseWriter, r *http.Request, out any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return bad(400, "请求内容过大或无法读取")
	}
	if err = decodeStrict(data, out); err != nil {
		return bad(400, "JSON 请求无效：%v", err)
	}
	return nil
}

func writeError(w http.ResponseWriter, err error) {
	status := 500
	var oe *operationError
	if errors.As(err, &oe) {
		status = oe.status
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func Run(ctx context.Context, configPath string) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("节点代理需要在 Linux 上运行")
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("读取节点配置失败：%w", err)
	}
	var cfg Config
	if err = decodeStrict(data, &cfg); err != nil {
		return fmt.Errorf("节点配置格式错误：%w", err)
	}
	s, err := NewServer(cfg, nil)
	if err != nil {
		return err
	}
	for _, binary := range []string{"nft", "ip", "ss"} {
		if _, err = exec.LookPath(binary); err != nil {
			return fmt.Errorf("缺少系统命令 %s，请安装 nftables 和 iproute2", binary)
		}
	}
	tlsConfig, err := loadTLS(s.cfg)
	if err != nil {
		return err
	}
	if err = s.Restore(ctx); err != nil {
		slog.Warn("恢复节点映射失败，保留管理接口以便修复配置", "node", cfg.Node.ID, "error", err)
	}
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	httpServer := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: tlsConfig}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()
	err = httpServer.Serve(tls.NewListener(listener, tlsConfig))
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func loadTLS(cfg Config) (*tls.Config, error) {
	info, err := os.Stat(cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("读取节点私钥失败：%w", err)
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("节点私钥权限必须为 0600")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("读取节点证书失败：%w", err)
	}
	ca, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("读取节点 CA 失败：%w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("节点 CA 无效")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 || cs.PeerCertificates[0].Subject.CommonName != cfg.ControllerCN {
			return fmt.Errorf("客户端证书不属于控制台")
		}
		return nil
	}}, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".netshare-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(temp, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
