import { useEffect, useRef, useState, type FormEvent } from "react";
import { Check, Loader2, Pencil, X } from "lucide-react";
import { api, errorMessage } from "../lib/api";
import { Alert, Button } from "./ui";

export function NodeNameEditor({ node, onSaved }: {
  node: { id: string; name: string };
  onSaved: () => void;
}) {
  const [displayName, setDisplayName] = useState(node.name);
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const triggerRef = useRef<HTMLButtonElement>(null);
  const wasEditing = useRef(false);
  // 轮询只更新已保存的名字，不能覆盖正在编辑的输入。
  useEffect(() => setDisplayName(node.name), [node.name]);
  useEffect(() => {
    if (!editing && wasEditing.current) triggerRef.current?.focus();
    wasEditing.current = editing;
  }, [editing]);

  function cancel() {
    if (busy) return;
    setEditing(false);
    setError("");
  }

  async function save(event: FormEvent) {
    event.preventDefault();
    if (busy) return;
    const name = draft.trim();
    if (!name || Array.from(name).length > 80) {
      setError("节点名称应为 1–80 个字。");
      return;
    }
    setBusy(true);
    setError("");
    try {
      const saved = await api<{ name: string }>(`/api/nodes/${encodeURIComponent(node.id)}/name`, { method: "PATCH", body: { name } });
      setDisplayName(saved.name);
      setEditing(false);
      onSaved();
    } catch (e) {
      setError(errorMessage(e));
    } finally {
      setBusy(false);
    }
  }

  return <div className="node-name-editor">
    <div className="node-name-row">
      <h2 id={`node-title-${node.id}`} title={displayName}>{displayName}</h2>
      {!editing && <Button ref={triggerRef} variant="ghost" size="icon" className="node-rename" aria-label={`修改 ${displayName} 的名称`} title="修改名称" onClick={() => {
        setDraft(displayName);
        setError("");
        setEditing(true);
      }}><Pencil size={14} aria-hidden="true" /></Button>}
    </div>
    {editing && <form className="node-name-form" onSubmit={event => void save(event)} onKeyDown={event => {
      if (event.key === "Escape" && !busy) { event.preventDefault(); event.stopPropagation(); cancel(); }
    }}>
      <label className="sr-only" htmlFor={`node-name-${node.id}`}>节点名称</label>
      <div className="node-name-controls">
        <input id={`node-name-${node.id}`} className="input" value={draft} onChange={event => setDraft(event.target.value)} maxLength={160} autoFocus disabled={busy} aria-invalid={!!error} aria-describedby={error ? `node-name-error-${node.id}` : undefined} />
        <Button type="submit" size="icon" disabled={busy} aria-label="保存名称" title="保存名称">{busy ? <Loader2 size={16} className="spinning" /> : <Check size={16} />}</Button>
        <Button variant="ghost" size="icon" disabled={busy} onClick={cancel} aria-label="取消改名" title="取消改名"><X size={16} /></Button>
      </div>
      {error && <Alert id={`node-name-error-${node.id}`}>{error}</Alert>}
    </form>}
  </div>;
}
