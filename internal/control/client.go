package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"netshare/internal/model"
)

type Agents interface {
	Status(context.Context, model.Node) (model.AgentStatus, error)
	Apply(context.Context, model.Node, model.ApplyRequest) (model.ApplyResult, error)
	Probe(context.Context, model.Node, model.ProbeRequest) (model.ProbeResult, error)
}

type remoteAgents struct{ client *http.Client }

func (a *remoteAgents) request(ctx context.Context, n model.Node, method, path string, body, out any) error {
	var r io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		r = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(n.AgentURL, "/")+path, r)
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	res, e := a.client.Do(req)
	if e != nil {
		return fmt.Errorf("无法连接 %s 的私网代理: %w", n.Name, e)
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if e != nil {
		return e
	}
	if res.StatusCode != http.StatusOK {
		var detail struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &detail)
		if detail.Error == "" {
			detail.Error = fmt.Sprintf("代理返回 HTTP %d", res.StatusCode)
		}
		return fmt.Errorf("%s: %s", n.Name, detail.Error)
	}
	if e = json.Unmarshal(b, out); e != nil {
		return fmt.Errorf("%s 的代理响应无效", n.Name)
	}
	return nil
}
func (a *remoteAgents) Status(ctx context.Context, n model.Node) (s model.AgentStatus, e error) {
	e = a.request(ctx, n, "GET", "/v1/status", nil, &s)
	return
}
func (a *remoteAgents) Apply(ctx context.Context, n model.Node, r model.ApplyRequest) (s model.ApplyResult, e error) {
	if r.AllowedTargets != nil {
		status, err := a.Status(ctx, n)
		if err != nil {
			return s, err
		}
		// 老代理仍保持原白名单；升级后的代理会自动同步，无需重新分发安装包。
		if !status.AutomaticTargets {
			r.AllowedTargets = nil
		}
	}
	e = a.request(ctx, n, "POST", "/v1/apply", r, &s)
	return
}
func (a *remoteAgents) Probe(ctx context.Context, n model.Node, r model.ProbeRequest) (s model.ProbeResult, e error) {
	e = a.request(ctx, n, "POST", "/v1/probe", r, &s)
	return
}
