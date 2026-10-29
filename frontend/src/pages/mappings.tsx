import { JoinNodeButton } from "../components/join-node";
import { useEffect, useMemo, useState } from "react";
import {
  flexRender,
  getCoreRowModel,
  getFilteredRowModel,
  getPaginationRowModel,
  getSortedRowModel,
  useReactTable,
  type ColumnDef,
  type SortingState,
} from "@tanstack/react-table";
import {
  ArrowDownUp,
  ArrowLeftRight,
  ArrowRight,
  Check,
  ChevronLeft,
  ChevronRight,
  ChevronsLeft,
  ChevronsRight,
  Copy,
  Download,
  Globe,
  MoreHorizontal,
  Pencil,
  Plus,
  Radio,
  RefreshCw,
  Search,
  Server,
  SlidersHorizontal,
  Trash2,
  X,
} from "lucide-react";
import type { Mapping, NodeStatus, Overview, ProbeResult } from "../lib/types";
import { mappingState, protocolLabel } from "../lib/utils";
import {
  Badge,
  Button,
  Empty,
  Menu,
  MenuContent,
  MenuItem,
  MenuSeparator,
  MenuTrigger,
  SelectField,
  Switch,
} from "../components/ui";

interface MappingMeta {
  node: (id: string) => NodeStatus | undefined;
  nodes: NodeStatus[];
  busy: boolean;
  onEdit: (mapping: Mapping) => void;
  onReverse: (mapping: Mapping) => void;
  onDelete: (mapping: Mapping) => void;
  onToggle: (mapping: Mapping) => void;
  onProbe: (mapping: Mapping) => void;
  onCopy: (mapping: Mapping) => Promise<void>;
}
// 列与单元格类型保持稳定，轮询刷新时不卸载菜单或开关。
const columns: ColumnDef<Mapping>[] = [
  {
    accessorKey: "name",
    header: ({ column }) => (
      <button
        className="table-sort"
        onClick={() => column.toggleSorting(column.getIsSorted() === "asc")}
      >
        映射名称
        <ArrowDownUp size={14} />
      </button>
    ),
    cell: ({ row, table }) => {
      const { node, onEdit } = table.options.meta as MappingMeta;
      return (
        <div className="mapping-identity">
          <button className="mapping-name" onClick={() => onEdit(row.original)}>
            {row.original.name}
          </button>
          <span className="muted small">
            {node(row.original.source_node)?.name || row.original.source_node}
            <ArrowRight size={12} />
            {node(row.original.target_node)?.name || row.original.target_node}
          </span>
        </div>
      );
    },
  },
  {
    id: "source",
    header: "公网入口",
    cell: ({ row, table }) => {
      const { node } = table.options.meta as MappingMeta;
      return (
        <div className="endpoint">
          <code>
            {node(row.original.source_node)?.public_ip}
            <span>:{row.original.listen_port}</span>
          </code>
          <span className="muted small">
            {node(row.original.source_node)?.public_interface || "—"}
          </span>
        </div>
      );
    },
  },
  {
    id: "target",
    header: "私网目标",
    cell: ({ row, table }) => {
      const { node } = table.options.meta as MappingMeta;
      return (
        <div className="endpoint">
          <code>
            {node(row.original.target_node)?.private_ip}
            <span>:{row.original.target_port}</span>
          </code>
          <span className="muted small">
            {node(row.original.target_node)?.private_interface || "—"}
          </span>
        </div>
      );
    },
  },
  {
    accessorKey: "protocol",
    header: "协议",
    cell: ({ row }) => <Badge>{protocolLabel(row.original.protocol)}</Badge>,
  },
  {
    id: "status",
    header: "应用状态",
    cell: ({ row, table }) => {
      const { nodes } = table.options.meta as MappingMeta;
      const state = mappingState(row.original, nodes);
      return (
        <Badge tone={state.tone}>
          <span className={`status-dot ${state.tone}`} />
          {state.label}
        </Badge>
      );
    },
  },
  {
    accessorKey: "enabled",
    header: "启用",
    cell: ({ row, table }) => {
      const { busy, onToggle } = table.options.meta as MappingMeta;
      return (
        <Switch
          checked={row.original.enabled}
          label={`${row.original.enabled ? "停用" : "启用"} ${row.original.name}`}
          onCheckedChange={() => onToggle(row.original)}
          disabled={busy}
        />
      );
    },
  },
  {
    id: "actions",
    header: () => <span className="sr-only">操作</span>,
    cell: ({ row, table }) => {
      const { node, busy, onEdit, onReverse, onCopy, onProbe, onDelete } = table
        .options.meta as MappingMeta;
      return (
        <Menu>
          <MenuTrigger asChild>
            <Button
              variant="ghost"
              size="icon"
              disabled={busy}
              aria-label={`操作 ${row.original.name}`}
            >
              <MoreHorizontal size={18} />
            </Button>
          </MenuTrigger>
          <MenuContent>
            <MenuItem onSelect={() => onEdit(row.original)}>
              <Pencil />
              编辑
            </MenuItem>
            <MenuItem onSelect={() => onReverse(row.original)}>
              <ArrowLeftRight />
              反向
            </MenuItem>
            <MenuItem onSelect={() => void onCopy(row.original)}>
              <Copy />
              复制公网入口
            </MenuItem>
            <MenuItem
              disabled={
                row.original.protocol === "udp" ||
                !node(row.original.source_node)?.online
              }
              onSelect={() => onProbe(row.original)}
            >
              <Radio />
              检查目标
            </MenuItem>
            <MenuSeparator />
            <MenuItem danger onSelect={() => onDelete(row.original)}>
              <Trash2 />
              删除
            </MenuItem>
          </MenuContent>
        </Menu>
      );
    },
  },
];

export function MappingsPage({
  overview,
  busy,
  onCreate,
  onJoined,
  onEdit,
  onReverse,
  onDelete,
  onToggle,
  onProbe,
  probes,
  onExport,
  onReconcile,
  notify,
}: {
  overview: Overview;
  busy: boolean;
  onCreate: () => void;
  onJoined: () => void;
  onEdit: (mapping: Mapping) => void;
  onReverse: (mapping: Mapping) => void;
  onDelete: (mapping: Mapping) => void;
  onToggle: (mapping: Mapping) => void;
  onProbe: (mapping: Mapping) => void;
  probes: Record<string, ProbeResult>;
  onExport: () => void;
  onReconcile: () => void;
  notify: (message: string, error?: boolean) => void;
}) {
  const [search, setSearch] = useState(""),
    [source, setSource] = useState("all"),
    [status, setStatus] = useState("all"),
    [protocol, setProtocol] = useState("all");
  const [sorting, setSorting] = useState<SortingState>([]);
  const node = (id: string) => overview.nodes.find((n) => n.id === id);
  const filtered = useMemo(
    () =>
      overview.mappings.filter(
        (mapping) =>
          (source === "all" || mapping.source_node === source) &&
          (status === "all" ||
            (status === "pending"
              ? mappingState(mapping, overview.nodes).tone === "warning"
              : status === "offline"
                ? mappingState(mapping, overview.nodes).tone === "destructive"
                : mapping.enabled === (status === "enabled"))) &&
          (protocol === "all" || mapping.protocol === protocol),
      ),
    [overview.mappings, overview.nodes, source, status, protocol],
  );
  const hasFilters =
    !!search || source !== "all" || status !== "all" || protocol !== "all";
  function clear() {
    setSearch("");
    setSource("all");
    setStatus("all");
    setProtocol("all");
  }
  async function copy(mapping: Mapping) {
    try {
      await navigator.clipboard.writeText(
        `${node(mapping.source_node)?.public_ip}:${mapping.listen_port}`,
      );
      notify("公网入口已复制。");
    } catch {
      notify("复制失败，请手动复制入口地址。", true);
    }
  }
  const table = useReactTable({
    data: filtered,
    columns,
    getRowId: (mapping) => mapping.id,
    meta: {
      node,
      nodes: overview.nodes,
      busy,
      onEdit,
      onReverse,
      onDelete,
      onToggle,
      onProbe,
      onCopy: copy,
    } satisfies MappingMeta,
    state: { sorting, globalFilter: search },
    onSortingChange: setSorting,
    onGlobalFilterChange: setSearch,
    globalFilterFn: (row, _, value) =>
      [
        row.original.name,
        row.original.listen_port,
        row.original.target_port,
        node(row.original.source_node)?.name,
        node(row.original.target_node)?.name,
        node(row.original.source_node)?.public_ip,
        node(row.original.target_node)?.private_ip,
      ]
        .join(" ")
        .toLowerCase()
        .includes(String(value).toLowerCase()),
    autoResetPageIndex: false,
    getCoreRowModel: getCoreRowModel(),
    getFilteredRowModel: getFilteredRowModel(),
    getSortedRowModel: getSortedRowModel(),
    getPaginationRowModel: getPaginationRowModel(),
    initialState: { pagination: { pageSize: 10 } },
  });
  useEffect(() => {
    table.setPageIndex(0);
  }, [search, source, status, protocol, table]);
  useEffect(() => {
    const lastPage = Math.max(0, table.getPageCount() - 1);
    if (table.getState().pagination.pageIndex > lastPage)
      table.setPageIndex(lastPage);
  }, [filtered, search, table]);
  const onlineCount = overview.nodes.filter((n) => n.online).length;
  return (
    <div className="page-stack">
      <div className="page-heading">
        <div>
          <h1>端口映射</h1>
          <p>管理公网入口，让服务经私网互通。</p>
        </div>
        <div className="page-actions">
          <JoinNodeButton onJoined={onJoined} disabled={busy || overview.nodes.length >= 32} />
          <Button
            id="export"
            variant="outline"
            onClick={onExport}
            disabled={busy}
          >
            <Download size={16} />
            <span>导出配置</span>
          </Button>
          <Button
            id="new-mapping"
            onClick={onCreate}
            disabled={busy || overview.nodes.length < 2}
          >
            <Plus size={17} />
            新建映射
          </Button>
        </div>
      </div>
      <section className="network-panel" aria-label="已接入网络">
        <div className="network-panel-heading">
          <div>
            <NetworkLabel />
            <span>已接入网络</span>
            <Badge
              tone={
                onlineCount === overview.nodes.length && onlineCount > 0
                  ? "success"
                  : "warning"
              }
            >
              {onlineCount}/{overview.nodes.length} 在线
            </Badge>
          </div>
          <button
            className="text-link"
            data-view="nodes"
            onClick={() => {
              location.hash = "nodes";
            }}
          >
            查看机器
            <ArrowRight size={14} />
          </button>
        </div>
        <div className="network-nodes">
          {overview.nodes.map((entry, index) => (
            <div key={entry.id} className="network-node-item">
              {index > 0 && (
                <span className="network-connector">
                  <ArrowLeftRight size={16} />
                  <span>私网</span>
                </span>
              )}
              <div className="network-node">
                <div className="network-node-top">
                  <span className="server-mark">
                    <Server size={18} />
                  </span>
                  <strong>{entry.name}</strong>
                  <span
                    className={`status-dot ${entry.online ? "success" : "destructive"}`}
                  />
                  <span className="muted small">
                    {entry.online ? "在线" : "离线"}
                  </span>
                </div>
                <div className="network-address">
                  <span>
                    <Globe size={13} />
                    公网
                  </span>
                  <code>{entry.public_ip}</code>
                </div>
                <div className="network-address">
                  <span>
                    <ArrowLeftRight size={13} />
                    私网
                  </span>
                  <code>{entry.private_ip}</code>
                </div>
              </div>
            </div>
          ))}
          {overview.nodes.length === 0 && (
            <p className="muted">先点“接入节点”，在每台服务器执行生成的命令。接入两台后就能创建映射。</p>
          )}
        </div>
      </section>
      <section className="mapping-section">
        <div className="section-heading">
          <div>
            <h2>
              全部映射{" "}
              <span className="count-label">{overview.mappings.length}</span>
            </h2>
            <p>每条规则独立控制，两边可同时转发。</p>
          </div>
          <Button
            id="reconcile"
            variant="outline"
            size="sm"
            disabled={busy}
            onClick={onReconcile}
          >
            <RefreshCw size={14} />
            同步规则
          </Button>
        </div>
        <div className="toolbar mapping-toolbar">
          <div className="search-field">
            <Search size={16} />
            <input
              id="mapping-search"
              aria-label="搜索映射"
              value={search}
              onChange={(event) => setSearch(event.target.value)}
              placeholder="搜索名称、地址或端口…"
              className="input"
            />
            <kbd>/</kbd>
          </div>
          <SelectField
            id="source-filter"
            label="筛选入口"
            value={source}
            onValueChange={setSource}
            options={[
              { value: "all", label: "全部入口" },
              ...overview.nodes.map((n) => ({ value: n.id, label: n.name })),
            ]}
          />
          <SelectField
            id="status-filter"
            label="筛选状态"
            value={status}
            onValueChange={setStatus}
            options={[
              { value: "all", label: "全部状态" },
              { value: "enabled", label: "已启用" },
              { value: "disabled", label: "已停用" },
              { value: "pending", label: "待同步" },
              { value: "offline", label: "入口离线" },
            ]}
          />
          <Menu>
            <MenuTrigger asChild>
              <Button
                variant="outline"
                className="filter-button"
                aria-label="筛选协议"
              >
                <SlidersHorizontal size={15} />
                {protocol === "all" ? "协议" : protocolLabel(protocol)}
              </Button>
            </MenuTrigger>
            <MenuContent>
              <MenuItem onSelect={() => setProtocol("all")}>
                {protocol === "all" && <Check />}全部协议
              </MenuItem>
              {["tcp", "udp", "both"].map((value) => (
                <MenuItem key={value} onSelect={() => setProtocol(value)}>
                  {protocol === value && <Check />}
                  {protocolLabel(value)}
                </MenuItem>
              ))}
            </MenuContent>
          </Menu>
          {hasFilters && (
            <Button
              id="clear-filters"
              variant="ghost"
              size="sm"
              onClick={clear}
            >
              <X size={14} />
              重置
            </Button>
          )}
        </div>
        <div className="panel table-panel">
          <div className="table-wrap">
            <table
              className="data-table mapping-table"
              aria-describedby="mapping-scroll-hint"
            >
              <thead>
                {table.getHeaderGroups().map((group) => (
                  <tr key={group.id}>
                    {group.headers.map((header) => (
                      <th key={header.id}>
                        {flexRender(
                          header.column.columnDef.header,
                          header.getContext(),
                        )}
                      </th>
                    ))}
                  </tr>
                ))}
              </thead>
              <tbody id="mapping-rows">
                {table.getRowModel().rows.map((row) => (
                  <tr key={row.id} data-mapping-id={row.original.id}>
                    {row.getVisibleCells().map((cell) => (
                      <td key={cell.id}>
                        {flexRender(
                          cell.column.columnDef.cell,
                          cell.getContext(),
                        )}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {table.getRowModel().rows.length === 0 && (
            <Empty
              id="mapping-empty"
              icon={<ArrowLeftRight size={24} />}
              title={hasFilters ? "没有匹配的映射" : "创建第一条映射"}
              action={
                hasFilters ? (
                  <Button
                    id="clear-filters-empty"
                    variant="outline"
                    onClick={clear}
                  >
                    清除筛选
                  </Button>
                ) : (
                  <Button
                    id="empty-new"
                    variant="outline"
                    onClick={onCreate}
                    disabled={overview.nodes.length < 2}
                  >
                    <Plus size={16} />
                    新建映射
                  </Button>
                )
              }
            >
              {hasFilters
                ? "试试其他关键词，或清除筛选条件。"
                : "选一台作为公网入口，再选择另一台的目标服务。"}
            </Empty>
          )}
        </div>
        <p id="mapping-scroll-hint" className="table-scroll-hint">
          <ArrowLeftRight size={13} />
          左右滑动查看目标与操作
        </p>
        <div className="pagination">
          <span className="muted">
            共 {table.getFilteredRowModel().rows.length} 条映射
          </span>
          <div className="pagination-controls">
            <span className="page-size">
              每页{" "}
              <SelectField
                label="每页条数"
                value={String(table.getState().pagination.pageSize)}
                onValueChange={(value) => table.setPageSize(Number(value))}
                options={[10, 20, 50].map((value) => ({
                  value: String(value),
                  label: String(value),
                }))}
              />
            </span>
            <span className="page-number">
              第 {table.getState().pagination.pageIndex + 1} /{" "}
              {Math.max(1, table.getPageCount())} 页
            </span>
            <Button
              variant="outline"
              size="icon"
              aria-label="第一页"
              disabled={!table.getCanPreviousPage()}
              onClick={() => table.setPageIndex(0)}
            >
              <ChevronsLeft size={16} />
            </Button>
            <Button
              variant="outline"
              size="icon"
              aria-label="上一页"
              disabled={!table.getCanPreviousPage()}
              onClick={() => table.previousPage()}
            >
              <ChevronLeft size={16} />
            </Button>
            <Button
              variant="outline"
              size="icon"
              aria-label="下一页"
              disabled={!table.getCanNextPage()}
              onClick={() => table.nextPage()}
            >
              <ChevronRight size={16} />
            </Button>
            <Button
              variant="outline"
              size="icon"
              aria-label="最后一页"
              disabled={!table.getCanNextPage()}
              onClick={() => table.setPageIndex(table.getPageCount() - 1)}
            >
              <ChevronsRight size={16} />
            </Button>
          </div>
        </div>
        {Object.entries(probes).map(([id, result]) => {
          const mapping = overview.mappings.find((m) => m.id === id);
          return mapping ? (
            <div
              className={`probe-notice ${result.reachable ? "" : "probe-failure"}`}
              key={id}
            >
              <Radio size={16} />
              <div>
                <strong>
                  {mapping.name}：
                  {result.reachable ? "目标 TCP 可达" : "目标 TCP 不可达"}
                </strong>
                <p>
                  {result.message}
                  {result.interface && ` · ${result.interface}`} ·
                  仅检查目标私网首个 TCP 端口，公网入口请用实际客户端验证。
                </p>
              </div>
            </div>
          ) : null;
        })}
      </section>
      <div className="page-footnote">
        <Globe size={14} />
        <p>
          访客访问入口公网；两台机器之间经私网转发。公网流量仍计入入口机器。
        </p>
      </div>
    </div>
  );
}
function NetworkLabel() {
  return (
    <span className="small-icon">
      <ArrowLeftRight size={15} />
    </span>
  );
}
