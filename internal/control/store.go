package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"netshare/internal/model"
)

type State struct {
	Revision uint64             `json:"revision"`
	Mappings []model.Mapping    `json:"mappings"`
	Events   []model.AuditEvent `json:"events"`
}

func loadState(path string) (State, error) {
	s := State{Mappings: []model.Mapping{}, Events: []model.AuditEvent{}}
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return s, nil
	}
	if e != nil {
		return s, e
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&s); e != nil {
		return s, fmt.Errorf("状态文件损坏，服务未覆盖原文件，请从备份恢复: %w", e)
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal(b, &fields); e != nil || fields["revision"] == nil || fields["mappings"] == nil || fields["events"] == nil || s.Mappings == nil || s.Events == nil {
		return s, fmt.Errorf("状态文件缺少必要字段，请从备份恢复")
	}
	return s, nil
}

func atomicJSON(path string, v any) error {
	dir := filepath.Dir(path)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(dir, ".netshare-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(0600); e != nil {
		f.Close()
		return e
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if e = enc.Encode(v); e != nil {
		f.Close()
		return e
	}
	if e = f.Sync(); e != nil {
		f.Close()
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	d, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
