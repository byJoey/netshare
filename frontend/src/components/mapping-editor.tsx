import { useEffect, useState, type FormEvent } from "react";
import {
  ArrowDown,
  ArrowLeftRight,
  Globe,
  Network,
  Trash2,
} from "lucide-react";
import type { Mapping, MappingInput, NodeStatus } from "../lib/types";
import { parsePorts } from "../lib/utils";
import { Alert, Badge, Button, Modal, SelectField, Switch } from "./ui";

export function MappingEditor({
  open,
  mapping,
  initial,
  nodes,
  busy,
  error,
  onClose,
  onSave,
  onDelete,
  onReverse,
  onDirtyChange,
}: {
  open: boolean;
  mapping: Mapping | null;
  initial: MappingInput;
  nodes: NodeStatus[];
  busy: boolean;
  error: string;
  onClose: () => void;
  onSave: (draft: MappingInput) => void;
  onDelete: (mapping: Mapping) => void;
  onReverse: (draft: MappingInput) => void;
  onDirtyChange: (dirty: boolean) => void;
}) {
  const [draft, setDraft] = useState(initial);
  const [validation, setValidation] = useState("");
  useEffect(() => {
    if (open) {
      setDraft(initial);
      setValidation("");
    }
  }, [open, initial]);
  const dirty = JSON.stringify(draft) !== JSON.stringify(initial);
  useEffect(() => {
    onDirtyChange(open && dirty);
  }, [open, dirty, onDirtyChange]);
  function update<K extends keyof MappingInput>(
    key: K,
    value: MappingInput[K],
  ) {
    setDraft((current) => {
      const next = { ...current, [key]: value };
      if (key === "source_node" && value === current.target_node)
        next.target_node = nodes.find((n) => n.id !== value)?.id || "";
      return next;
    });
  }
  function submit(event: FormEvent) {
    event.preventDefault();
    setValidation("");
    try {
      if (!draft.name.trim() || Array.from(draft.name.trim()).length > 80)
        throw new Error("映射名称应为 1–80 个字。");
      if (
        !draft.source_node ||
        !draft.target_node ||
        draft.source_node === draft.target_node
      )
        throw new Error("入口和目标必须是两台不同机器。");
      const [start, end] = parsePorts(draft.listen_port),
        [targetStart, targetEnd] = parsePorts(draft.target_port);
      if (end - start !== targetEnd - targetStart)
        throw new Error("入口与目标的端口数量必须一致。");
      const source = nodes.find((n) => n.id === draft.source_node);
      const conflict = [22, ...(source?.reserved_ports || [])].find(
        (port) => start <= port && port <= end,
      );
      if (conflict)
        throw new Error(
          `${source?.name} 的端口 ${conflict} 已保留，不能用于映射。`,
        );
      onSave({
        ...draft,
        name: draft.name.trim(),
        listen_port: draft.listen_port.trim(),
        target_port: draft.target_port.trim(),
      });
    } catch (value) {
      setValidation(value instanceof Error ? value.message : "请检查输入。");
    }
  }
  const source = nodes.find((n) => n.id === draft.source_node),
    target = nodes.find((n) => n.id === draft.target_node);
  return (
    <Modal
      open={open}
      onOpenChange={(value) => {
        if (!value) onClose();
      }}
      id="mapping-editor"
      titleID="editor-title"
      sheet
      closeDisabled={busy}
      title={mapping ? "编辑映射" : "新建映射"}
      description="把公网入口转发到另一台机器的私网服务。"
      footer={
        <>
          <div className="footer-left">
            {mapping && (
              <Button
                id="mapping-delete"
                variant="ghost"
                className="text-danger"
                disabled={busy}
                onClick={() => onDelete(mapping)}
              >
                <Trash2 size={16} />
                删除
              </Button>
            )}
          </div>
          <Button
            id="mapping-cancel"
            variant="outline"
            disabled={busy}
            onClick={onClose}
          >
            取消
          </Button>
          <Button
            id="mapping-save"
            type="submit"
            form="mapping-form"
            disabled={busy}
          >
            {busy ? "正在应用…" : "保存映射"}
          </Button>
        </>
      }
    >
      <form id="mapping-form" onSubmit={submit} className="form-stack">
        {(validation || error) && (
          <Alert id="form-error">{validation || error}</Alert>
        )}
        <div className="field">
          <label htmlFor="mapping-name">映射名称</label>
          <input
            id="mapping-name"
            className="input"
            value={draft.name}
            maxLength={80}
            onChange={(event) => update("name", event.target.value)}
            placeholder="例如：网站、数据库、游戏服务"
            autoComplete="off"
            required
            disabled={busy}
          />
        </div>
        <div className="field">
          <label htmlFor="mapping-protocol">协议</label>
          <SelectField
            id="mapping-protocol"
            label="协议"
            value={draft.protocol}
            onValueChange={(value) => update("protocol", value)}
            options={[
              { value: "tcp", label: "TCP" },
              { value: "udp", label: "UDP" },
              { value: "both", label: "TCP + UDP" },
            ]}
            disabled={busy}
          />
        </div>
        <div className="form-section">
          <div className="form-section-title">
            <Globe size={16} />
            <h3>公网入口</h3>
          </div>
          <div className="field">
            <label htmlFor="mapping-source">入口机器</label>
            <SelectField
              id="mapping-source"
              label="入口机器"
              value={draft.source_node}
              onValueChange={(value) => update("source_node", value)}
              options={nodes.map((n) => ({ value: n.id, label: n.name }))}
              disabled={busy}
            />
          </div>
          <div className="field">
            <label htmlFor="mapping-listen-port">入口端口</label>
            <input
              id="mapping-listen-port"
              className="input mono"
              placeholder="8080 或 8080-8090"
              value={draft.listen_port}
              onChange={(event) => update("listen_port", event.target.value)}
              disabled={busy}
              required
            />
            <p className="field-hint">
              保留端口：
              {[...new Set([22, ...(source?.reserved_ports || [])])]
                .sort((a, b) => a - b)
                .join("、")}
            </p>
          </div>
        </div>
        <div className="form-direction">
          <ArrowDown size={17} />
          <span>经私网转发</span>
        </div>
        <div className="form-section">
          <div className="form-section-title">
            <Network size={16} />
            <h3>私网目标</h3>
          </div>
          <div className="field">
            <label htmlFor="mapping-target">目标机器</label>
            <SelectField
              id="mapping-target"
              label="目标机器"
              value={draft.target_node}
              onValueChange={(value) => update("target_node", value)}
              options={nodes.map((n) => ({
                value: n.id,
                label: n.name,
                disabled: n.id === draft.source_node,
              }))}
              disabled={busy}
            />
          </div>
          <div className="field">
            <label htmlFor="mapping-target-port">目标端口</label>
            <input
              id="mapping-target-port"
              className="input mono"
              placeholder="服务实际监听的端口"
              value={draft.target_port}
              onChange={(event) => update("target_port", event.target.value)}
              disabled={busy}
              required
            />
            <p className="field-hint">
              服务需监听私网 IP 或 0.0.0.0。端口范围与入口等长。
            </p>
          </div>
        </div>
        <div className="route-preview">
          <div>
            <Badge>公网</Badge>
            <code>
              {source?.public_ip || "入口地址"}:{draft.listen_port || "端口"}
            </code>
          </div>
          <ArrowDown size={15} />
          <div>
            <Badge tone="success">私网</Badge>
            <code>
              {target?.private_ip || "目标地址"}:{draft.target_port || "端口"}
            </code>
          </div>
        </div>
        <div className="switch-field">
          <div>
            <label htmlFor="mapping-enabled">保存后启用</label>
            <p className="field-hint">开启后立即下发规则，开始接收新连接。</p>
          </div>
          <Switch
            id="mapping-enabled"
            label="保存后启用"
            checked={draft.enabled}
            onCheckedChange={(value) => update("enabled", value)}
            disabled={busy}
          />
        </div>
        {mapping && (
          <Button
            id="mapping-reverse"
            variant="outline"
            className="full-width"
            disabled={busy}
            onClick={() => onReverse(draft)}
          >
            <ArrowLeftRight size={16} />
            创建反向映射
          </Button>
        )}
      </form>
    </Modal>
  );
}
