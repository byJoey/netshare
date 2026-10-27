import { useMemo, useState } from "react";
import {
  AlertCircle,
  Check,
  ChevronLeft,
  ChevronRight,
  ChevronsLeft,
  ChevronsRight,
  History,
  Search,
  X,
} from "lucide-react";
import type { Overview } from "../lib/types";

function formatDate(value: string) {
  const date = new Date(value);
  if (!Number.isFinite(date.getTime())) return { day: "—", time: "" };
  return {
    day: new Intl.DateTimeFormat("zh-CN", {
      year: "numeric",
      month: "2-digit",
      day: "2-digit",
    }).format(date),
    time: new Intl.DateTimeFormat("zh-CN", {
      hour: "2-digit",
      minute: "2-digit",
      second: "2-digit",
      hour12: false,
    }).format(date),
  };
}

export function EventsPage({ overview }: { overview: Overview }) {
  const [search, setSearch] = useState("");
  const [result, setResult] = useState("all");
  const [pageIndex, setPageIndex] = useState(0);
  const [pageSize, setPageSize] = useState(20);
  const events = useMemo(() => {
    const query = search.trim().toLocaleLowerCase();
    return [...(overview.events || [])]
      .filter((event) => {
        if (
          (result === "success" && !event.success) ||
          (result === "failure" && event.success)
        )
          return false;
        return (
          !query ||
          `${event.action} ${event.detail}`.toLocaleLowerCase().includes(query)
        );
      })
      .sort((a, b) => new Date(b.time).getTime() - new Date(a.time).getTime());
  }, [overview.events, search, result]);
  const pageCount = Math.max(1, Math.ceil(events.length / pageSize));
  const currentPage = Math.min(pageIndex, pageCount - 1);
  const currentEvents = events.slice(
    currentPage * pageSize,
    (currentPage + 1) * pageSize,
  );
  const filtered = search.trim().length > 0 || result !== "all";
  const reset = () => {
    setSearch("");
    setResult("all");
    setPageIndex(0);
  };
  return (
    <div className="page-stack">
      <div className="toolbar event-toolbar">
        <div className="search-field">
          <Search size={16} aria-hidden="true" />
          <input
            id="event-search"
            className="input"
            type="search"
            value={search}
            onChange={(event) => {
              setSearch(event.target.value);
              setPageIndex(0);
            }}
            aria-label="搜索操作记录"
            placeholder="搜索操作或详情…"
          />
        </div>
        <label className="sr-only" htmlFor="event-filter">
          操作结果
        </label>
        <select
          id="event-filter"
          className="input filter-select"
          value={result}
          onChange={(event) => {
            setResult(event.target.value);
            setPageIndex(0);
          }}
        >
          <option value="all">全部结果</option>
          <option value="success">成功</option>
          <option value="failure">失败</option>
        </select>
        {filtered && (
          <button
            type="button"
            className="button button-ghost button-sm"
            onClick={reset}
          >
            <X size={14} aria-hidden="true" />
            重置
          </button>
        )}
        <span className="muted toolbar-count">共 {events.length} 条记录</span>
      </div>
      <section className="panel event-panel" aria-label="操作记录列表">
        {currentEvents.length ? (
          <div className="table-wrap">
            <table className="data-table event-table">
              <thead>
                <tr>
                  <th scope="col">时间</th>
                  <th scope="col">操作</th>
                  <th scope="col">详情</th>
                  <th scope="col">结果</th>
                </tr>
              </thead>
              <tbody id="event-rows">
                {currentEvents.map((event) => {
                  const date = formatDate(event.time);
                  return (
                    <tr key={event.id}>
                      <td>
                        <time
                          dateTime={event.time}
                          title={event.time}
                          className="event-time"
                        >
                          <span>{date.day}</span>
                          <span className="muted mono">{date.time}</span>
                        </time>
                      </td>
                      <td className="event-action">{event.action}</td>
                      <td className="event-detail">{event.detail}</td>
                      <td>
                        <span
                          className={`badge ${event.success ? "badge-success" : "badge-destructive"}`}
                        >
                          {event.success ? (
                            <Check size={12} aria-hidden="true" />
                          ) : (
                            <AlertCircle size={12} aria-hidden="true" />
                          )}
                          {event.success ? "成功" : "失败"}
                        </span>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        ) : (
          <div id="event-empty" className="empty-state">
            <History size={28} aria-hidden="true" />
            <h2>{filtered ? "没有匹配的记录" : "暂无操作记录"}</h2>
            <p className="muted">
              {filtered
                ? "换个关键词，或清除筛选再看看。"
                : "创建映射或同步规则后，这里会记录每次操作的结果。"}
            </p>
            {filtered && (
              <button
                type="button"
                className="button button-outline"
                onClick={reset}
              >
                清除筛选
              </button>
            )}
          </div>
        )}
      </section>
      {events.length > 0 && (
        <div className="pagination">
          <span className="muted">
            显示第 {currentPage * pageSize + 1}–
            {Math.min((currentPage + 1) * pageSize, events.length)} 条，共{" "}
            {events.length} 条
          </span>
          <div className="pagination-controls">
            <label htmlFor="event-page-size">每页</label>
            <select
              id="event-page-size"
              className="input page-size"
              value={pageSize}
              onChange={(event) => {
                setPageSize(Number(event.target.value));
                setPageIndex(0);
              }}
            >
              <option value={10}>10 条</option>
              <option value={20}>20 条</option>
              <option value={50}>50 条</option>
            </select>
            <span className="page-number">
              {currentPage + 1} / {pageCount}
            </span>
            <button
              className="button button-outline button-icon"
              type="button"
              aria-label="第一页"
              disabled={currentPage === 0}
              onClick={() => setPageIndex(0)}
            >
              <ChevronsLeft size={16} aria-hidden="true" />
            </button>
            <button
              className="button button-outline button-icon"
              type="button"
              aria-label="上一页"
              disabled={currentPage === 0}
              onClick={() => setPageIndex(currentPage - 1)}
            >
              <ChevronLeft size={16} aria-hidden="true" />
            </button>
            <button
              className="button button-outline button-icon"
              type="button"
              aria-label="下一页"
              disabled={currentPage === pageCount - 1}
              onClick={() => setPageIndex(currentPage + 1)}
            >
              <ChevronRight size={16} aria-hidden="true" />
            </button>
            <button
              className="button button-outline button-icon"
              type="button"
              aria-label="最后一页"
              disabled={currentPage === pageCount - 1}
              onClick={() => setPageIndex(pageCount - 1)}
            >
              <ChevronsRight size={16} aria-hidden="true" />
            </button>
          </div>
        </div>
      )}
      <p className="muted helper">
        时间按当前设备时区显示。记录来自控制器，自动更新。
      </p>
    </div>
  );
}
