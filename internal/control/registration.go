package control

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"netshare/internal/model"
	"netshare/internal/registration"
)

type joinTicket struct {
	ID        string              `json:"id"`
	Secret    string              `json:"secret"`
	ExpiresAt time.Time           `json:"expires_at"`
	Phase     string              `json:"phase"`
	Message   string              `json:"message"`
	KeyID     string              `json:"key_id,omitempty"`
	NodeID    string              `json:"node_id,omitempty"`
	Reply     *registration.Reply `json:"reply,omitempty"`
}

func (c *Controller) ticketPath(id string) string {
	return filepath.Join(filepath.Dir(c.cfg.StateFile), "node-joins", id+".json")
}

func (c *Controller) readTicket(id string, live bool) (joinTicket, error) {
	var ticket joinTicket
	if !registration.ValidToken(id) {
		return ticket, fmt.Errorf("接入命令无效，请回面板重新生成")
	}
	file := c.ticketPath(id)
	if err := registration.SafePath(file); err != nil {
		return ticket, fmt.Errorf("接入资料不可用")
	}
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 128<<10 || info.Mode().Perm()&0077 != 0 {
		return ticket, fmt.Errorf("接入命令不存在或已失效，请回面板重新生成")
	}
	data, err := os.ReadFile(file)
	if err != nil || registration.Decode(data, &ticket) != nil || ticket.ID != id {
		return ticket, fmt.Errorf("接入资料不可用")
	}
	if live && (ticket.Phase == "revoked" || !time.Now().Before(ticket.ExpiresAt)) {
		return ticket, fmt.Errorf("接入命令已过期或撤销，请回面板重新生成")
	}
	return ticket, nil
}

func (c *Controller) saveTicket(ticket joinTicket) error {
	data, err := json.Marshal(ticket)
	if err != nil {
		return err
	}
	return registration.WritePrivate(c.ticketPath(ticket.ID), data)
}

func (c *Controller) joinInfo(r *http.Request) (registration.Info, error) {
	var info registration.Info
	if c.cfg.RegistrationSocket == "" || c.cfg.RegistrationCAFile == "" {
		return info, fmt.Errorf("请在控制台服务器运行最新版安装脚本，选择3升级控制台以启用接入")
	}
	var err error
	info.ControllerCA, err = os.ReadFile(c.cfg.CAFile)
	if err != nil {
		return info, fmt.Errorf("控制台证书资料不可用")
	}
	info.IssuerCA, err = os.ReadFile(c.cfg.RegistrationCAFile)
	if err != nil {
		return info, fmt.Errorf("节点接入服务尚未就绪，请用最新版安装脚本升级控制台")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(info.ControllerCA) || !pool.AppendCertsFromPEM(info.IssuerCA) {
		return info, fmt.Errorf("控制台证书资料无效")
	}
	for _, node := range c.nodes() {
		info.Peers = append(info.Peers, node.PrivateIP)
	}
	// 空控制台也能让节点优先选择与本机相通的私网。
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addresses, _ := iface.Addrs()
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.To4() != nil && ip.IsPrivate() {
				info.Peers = append(info.Peers, ip.String())
			}
		}
	}
	if r != nil {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		info.SourceIP = host
	}
	return info, nil
}

func quoteShell(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func (c *Controller) handleCreateJoin(w http.ResponseWriter, r *http.Request) {
	if _, err := c.joinInfo(nil); err != nil {
		apiError(w, 503, err.Error())
		return
	}
	c.joinMu.Lock()
	defer c.joinMu.Unlock()
	if len(c.nodes()) >= 32 {
		apiError(w, 409, "最多接入32台机器")
		return
	}
	entries, _ := os.ReadDir(filepath.Dir(c.ticketPath("placeholder")))
	active := 0
	for _, entry := range entries {
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
			old, err := c.readTicket(strings.TrimSuffix(entry.Name(), ".json"), false)
			if err == nil && old.Phase != "revoked" && time.Now().Before(old.ExpiresAt) {
				active++
			}
		}
	}
	if active >= 24 {
		apiError(w, 429, "待使用的命令较多，请先使用或撤销已有命令")
		return
	}
	id, secret, err := registration.NewSecret()
	if err != nil {
		apiError(w, 500, "生成接入命令失败，请重试")
		return
	}
	ticket := joinTicket{ID: id, Secret: secret, ExpiresAt: time.Now().UTC().Add(15 * time.Minute), Phase: "waiting", Message: "等待在新服务器执行命令"}
	if err = c.saveTicket(ticket); err != nil {
		apiError(w, 500, "保存接入命令失败，请检查磁盘")
		return
	}
	invitation := strings.TrimRight(c.cfg.ExternalURL, "/") + c.cfg.AccessPath + "/api/join/" + id + "#" + secret
	command := "bash <(curl -fsSL https://github.com/byJoey/netshare/releases/latest/download/install.sh) install agent --join " + quoteShell(invitation)
	writeJSON(w, 200, map[string]any{"id": id, "command": command, "expires_at": ticket.ExpiresAt, "phase": ticket.Phase, "message": ticket.Message})
}

func (c *Controller) handleJoinStatus(w http.ResponseWriter, r *http.Request) {
	c.joinMu.Lock()
	ticket, err := c.readTicket(r.PathValue("id"), false)
	c.joinMu.Unlock()
	if err != nil {
		apiError(w, 404, err.Error())
		return
	}
	phase, message := ticket.Phase, ticket.Message
	if phase != "revoked" && ticket.NodeID != "" {
		if node, ok := c.node(ticket.NodeID); ok {
			child, cancel := context.WithTimeout(r.Context(), 3*time.Second)
			status, e := c.agents.Status(child, node)
			cancel()
			if e == nil && status.NodeID == node.ID {
				phase, message = "online", "已接入，可以创建映射了"
			} else if time.Now().Before(ticket.ExpiresAt) {
				phase, message = "installing", "已收到服务器信息，正在完成安装"
			} else {
				phase, message = "expired", "未完成连接，请重新生成命令并在原服务器执行"
			}
		}
	} else if phase != "revoked" && !time.Now().Before(ticket.ExpiresAt) {
		phase, message = "expired", "这条命令已过期，请重新生成"
	}
	writeJSON(w, 200, map[string]any{"id": ticket.ID, "phase": phase, "message": message, "node_id": ticket.NodeID, "expires_at": ticket.ExpiresAt})
}

func (c *Controller) handleRevokeJoin(w http.ResponseWriter, r *http.Request) {
	c.joinMu.Lock()
	defer c.joinMu.Unlock()
	ticket, err := c.readTicket(r.PathValue("id"), false)
	if err != nil {
		apiError(w, 404, err.Error())
		return
	}
	ticket.Phase, ticket.Message = "revoked", "接入命令已撤销"
	if c.saveTicket(ticket) != nil {
		apiError(w, 500, "撤销未完成，请重试")
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (c *Controller) previousIdentity(report registration.Report, csr *x509.CertificateRequest, keyID string) error {
	if len(report.PreviousCertificate) == 0 {
		if report.Node.ID != "node-"+keyID[:24] {
			return fmt.Errorf("节点身份不匹配")
		}
		return nil
	}
	block, rest := pem.Decode(report.PreviousCertificate)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return fmt.Errorf("原节点身份证明无效")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil || cert.Subject.CommonName != report.Node.ID {
		return fmt.Errorf("原节点身份证明无效")
	}
	expected, _ := registration.KeyID(cert.PublicKey)
	if expected != keyID {
		return fmt.Errorf("原节点私钥不匹配，已保留配置")
	}
	info, err := c.joinInfo(nil)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(info.ControllerCA)
	pool.AppendCertsFromPEM(info.IssuerCA)
	if _, err = cert.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return fmt.Errorf("原节点不属于当前控制台")
	}
	if !reflect.DeepEqual(cert.PublicKey, csr.PublicKey) {
		return fmt.Errorf("节点身份证明不一致")
	}
	return nil
}

func (c *Controller) registerNode(ctx context.Context, ticket *joinTicket, report registration.Report) (registration.Reply, error) {
	var empty registration.Reply
	if len([]rune(report.Node.Name)) > 80 || strings.IndexFunc(report.Node.Name, unicode.IsControl) >= 0 || !utf8.ValidString(report.Node.Name) {
		return empty, fmt.Errorf("服务器名称无效")
	}
	csr, err := registration.ParseCSR(report.Node, report.CSR)
	if err != nil {
		return empty, err
	}
	keyID, err := registration.KeyID(csr.PublicKey)
	if err != nil {
		return empty, err
	}
	if err = c.previousIdentity(report, csr, keyID); err != nil {
		return empty, err
	}
	if ticket.KeyID != "" && ticket.KeyID != keyID {
		return empty, fmt.Errorf("这条命令已经用于另一台服务器，请回面板重新生成")
	}
	if ticket.Reply != nil {
		if report.Node.ID != ticket.Reply.Node.ID || report.Node.PrivateIP != ticket.Reply.Node.PrivateIP || report.Node.PublicIP != ticket.Reply.Node.PublicIP || report.Node.PrivateInterface != ticket.Reply.Node.PrivateInterface || report.Node.PublicInterface != ticket.Reply.Node.PublicInterface {
			return empty, fmt.Errorf("服务器信息已变化，请重新生成接入命令")
		}
		return *ticket.Reply, nil
	}
	c.mutation.Lock()
	defer c.mutation.Unlock()
	if c.configPath == "" {
		return empty, fmt.Errorf("控制台配置不可写，无法登记节点")
	}
	nodes := c.nodes()
	index := -1
	for i, node := range nodes {
		if node.ID == report.Node.ID {
			index = i
			continue
		}
		if node.PrivateIP == report.Node.PrivateIP {
			return empty, fmt.Errorf("这台私网地址已属于其他节点，请检查私网配置")
		}
	}
	if index >= 0 && len(report.PreviousCertificate) == 0 && ticket.KeyID != keyID {
		return empty, fmt.Errorf("节点已存在，请使用原机接入命令")
	}
	if index < 0 && len(nodes) >= 32 {
		return empty, fmt.Errorf("最多接入32台机器")
	}
	node := report.Node
	node.AgentURL = "https://" + net.JoinHostPort(node.PrivateIP, "31880")
	node.ReservedPorts = []int{22, 31880}
	if index >= 0 {
		node.Name = nodes[index].Name
		node.ReservedPorts = nodes[index].ReservedPorts
	}
	ticket.KeyID, ticket.NodeID, ticket.Phase, ticket.Message = keyID, node.ID, "registering", "正在登记服务器"
	if err = c.saveTicket(*ticket); err != nil {
		return empty, fmt.Errorf("保存接入进度失败，请重试同一条命令")
	}
	var signed registration.SignReply
	if c.signNode != nil {
		signed, err = c.signNode(ctx, registration.SignRequest{Node: node, CSR: report.CSR})
	} else {
		signed, err = registration.Sign(ctx, c.cfg.RegistrationSocket, registration.SignRequest{Node: node, CSR: report.CSR})
	}
	if err != nil {
		return empty, err
	}
	info, err := c.joinInfo(nil)
	if err != nil {
		return empty, err
	}
	reply := registration.Reply{Node: node, Certificate: signed.Certificate, TrustBundle: append(append([]byte{}, info.ControllerCA...), info.IssuerCA...), AllowedTargets: []string{}}
	for _, peer := range nodes {
		if peer.ID != node.ID {
			reply.AllowedTargets = append(reply.AllowedTargets, peer.PrivateIP)
		}
	}
	if index >= 0 {
		nodes[index] = node
	} else {
		nodes = append(nodes, node)
	}
	c.mu.RLock()
	cfg := c.cfg
	c.mu.RUnlock()
	cfg.Nodes = nodes
	if err = atomicJSON(c.configPath, cfg); err != nil {
		return empty, fmt.Errorf("保存节点失败，请检查磁盘后重试")
	}
	c.mu.Lock()
	c.cfg.Nodes = nodes
	c.mu.Unlock()
	ticket.Reply, ticket.Phase, ticket.Message = &reply, "installing", "已收到服务器信息，正在完成安装"
	if err = c.saveTicket(*ticket); err != nil {
		return empty, fmt.Errorf("保存接入结果失败，请重试同一条命令")
	}
	state := c.snapshot()
	appendEvent(&state, "接入节点", node.Name+" 已登记，等待节点启动", true)
	if err = c.persist(state); err != nil {
		return empty, fmt.Errorf("保存操作记录失败，请检查磁盘")
	}
	return reply, nil
}

func (c *Controller) handleJoinProtocol(w http.ResponseWriter, r *http.Request) {
	id, action := r.PathValue("id"), r.PathValue("action")
	if action != "info" && action != "register" && action != "problem" && action != "complete" {
		apiError(w, 404, "接入操作不存在")
		return
	}
	c.joinMu.Lock()
	defer c.joinMu.Unlock()
	ticket, err := c.readTicket(id, true)
	if err != nil {
		apiError(w, 410, err.Error())
		return
	}
	var box registration.Envelope
	if !decodeJSON(w, r, &box) {
		return
	}
	var out any
	switch action {
	case "info":
		var in struct{}
		if registration.Open(ticket.Secret, id+":"+action, box, &in) != nil {
			apiError(w, 401, "接入认证失败，请重新复制整条命令")
			return
		}
		out, err = c.joinInfo(r)
	case "register":
		var in registration.Report
		if registration.Open(ticket.Secret, id+":"+action, box, &in) != nil {
			apiError(w, 401, "接入认证失败，请重新复制整条命令")
			return
		}
		out, err = c.registerNode(r.Context(), &ticket, in)
	case "problem":
		var in struct {
			Code string `json:"code"`
		}
		if registration.Open(ticket.Secret, id+":"+action, box, &in) != nil {
			apiError(w, 401, "接入认证失败")
			return
		}
		messages := map[string]string{"no_private": "服务器还没有可用私网，请先在服务商面板启用私网，再执行同一条命令", "ambiguous_private": "服务器发现多张私网网卡，请在终端选择连接其他节点的那一张", "no_public": "服务器没有识别到公网入口，NAT主机需要指定公网IP"}
		message, ok := messages[in.Code]
		if !ok {
			apiError(w, 400, "状态无效")
			return
		}
		ticket.Phase, ticket.Message = "needs_network", message
		err = c.saveTicket(ticket)
		out = struct{}{}
	case "complete":
		var in struct{}
		if registration.Open(ticket.Secret, id+":"+action, box, &in) != nil {
			apiError(w, 401, "接入认证失败")
			return
		}
		if ticket.NodeID == "" {
			apiError(w, 409, "服务器尚未登记，请重试安装命令")
			return
		}
		node, ok := c.node(ticket.NodeID)
		if !ok {
			apiError(w, 409, "服务器尚未登记")
			return
		}
		status, e := c.agents.Status(r.Context(), node)
		if e != nil || status.NodeID != node.ID {
			apiError(w, 409, "节点尚未启动，原命令可以重试")
			return
		}
		// 接入已成功，离线旧节点由后台在恢复后补齐，不把它们变成新节点的安装失败。
		go func() {
			syncCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			_ = c.Reconcile(syncCtx)
		}()
		ticket.Phase, ticket.Message = "online", "已接入，可以创建映射了"
		err = c.saveTicket(ticket)
		out = map[string]bool{"ok": true}
	}
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	sealed, err := registration.Seal(ticket.Secret, id+":"+action+":response", out)
	if err != nil {
		apiError(w, 500, "生成接入结果失败，请重试")
		return
	}
	writeJSON(w, 200, sealed)
}

func (c *Controller) registrationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/node-joins", c.protected(c.handleCreateJoin, true))
	mux.HandleFunc("GET /api/node-joins/{id}", c.protected(c.handleJoinStatus, false))
	mux.HandleFunc("DELETE /api/node-joins/{id}", c.protected(c.handleRevokeJoin, true))
	mux.HandleFunc("POST /api/join/{id}/{action}", c.handleJoinProtocol)
}

func sameTargets(a, b []string) bool {
	a = append([]string{}, a...)
	b = append([]string{}, b...)
	sort.Strings(a)
	sort.Strings(b)
	return reflect.DeepEqual(a, b)
}

func (c *Controller) maintainTopology(ctx context.Context) {
	if c.cfg.RegistrationSocket == "" {
		return
	}
	ticker := time.NewTicker(12 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		statuses := c.nodeStatuses(ctx)
		changed := false
		state := c.snapshot()
		for _, node := range statuses {
			if node.Online && node.Status != nil && node.Status.AutomaticTargets && (!sameTargets(node.Status.AllowedTargets, c.targets(node.ID)) || !sameRules(node.Status.Rules, c.rules(state.Mappings, node.ID))) {
				changed = true
				break
			}
		}
		if changed {
			child, cancel := context.WithTimeout(ctx, 90*time.Second)
			_ = c.Reconcile(child)
			cancel()
		}
	}
}

func sameRules(a, b []model.AgentRule) bool {
	a = append([]model.AgentRule{}, a...)
	b = append([]model.AgentRule{}, b...)
	sort.Slice(a, func(i, j int) bool { return a[i].ID < a[j].ID })
	sort.Slice(b, func(i, j int) bool { return b[i].ID < b[j].ID })
	return reflect.DeepEqual(a, b)
}
