import { useEffect, useMemo, useState } from "react";
import { getJSON, postJSON, putJSON } from "../lib/api";

interface Setting {
  key: string;
  value: unknown;
  default_value?: unknown;
  description?: string;
  editable?: boolean;
}
type CooldownRule = {
  enabled?: boolean;
  name?: string;
  priority?: number;
  scope?: string;
  mode?: string;
  status_codes?: number[];
  message_pattern?: string;
  cooldown_seconds?: number;
  time_capture?: string;
  time_format?: string;
  time_layout?: string;
  timezone?: string;
};
type PricingEntry = {
  model: string;
  input_price?: number;
  output_price?: number;
  cache_read_price?: number;
  cache_write_price?: number;
  input_price_high?: number;
  output_price_high?: number;
  cache_read_price_high?: number;
  cache_write_price_high?: number;
};
type RuntimeMetric = { key: string; label: string; format?: "integer" | "bytes" | "duration" | "seconds" | "percent" | "boolean" | "timestamp" };
const runtimeGroups: Record<string, RuntimeMetric[]> = {
  process: [{ key: "uptime_seconds", label: "运行时间", format: "duration" }, { key: "concurrency_slots_in_use", label: "当前并发" }, { key: "max_concurrency", label: "最大并发" }, { key: "goroutines", label: "Goroutine" }],
  resources: [{ key: "cpu_usage_percent", label: "CPU", format: "percent" }, { key: "rss_bytes", label: "RSS", format: "bytes" }, { key: "heap_alloc_bytes", label: "堆内存", format: "bytes" }, { key: "gc_count", label: "GC 次数" }],
  http_proxy: [{ key: "active_requests", label: "活动请求" }, { key: "completed_requests", label: "完成请求" }, { key: "streaming_requests", label: "流式请求" }, { key: "response_body_bytes", label: "响应字节", format: "bytes" }],
  logs: [{ key: "backlog_entries", label: "日志积压" }, { key: "dropped_entries", label: "丢弃日志" }, { key: "persistence_failed_entries", label: "持久化失败" }],
  storage: [{ key: "primary_sync_pending", label: "主库同步积压" }, { key: "primary_sync_failures", label: "主库同步失败" }, { key: "sqlite_read_failures", label: "SQLite 读取失败" }, { key: "analytics_reads_primary", label: "统计读取主库", format: "boolean" }],
  responses_websocket: [{ key: "downstream_connections", label: "下游连接" }, { key: "rejected_downstream_connections", label: "拒绝下游连接" }, { key: "ttl_expired", label: "TTL 过期" }, { key: "capacity_rejected", label: "容量拒绝" }],
};
const pricingFields = [
  "input_price",
  "output_price",
  "cache_read_price",
  "cache_write_price",
  "input_price_high",
  "output_price_high",
  "cache_read_price_high",
  "cache_write_price_high",
] as const;
const groupOf = (key: string) =>
  key.startsWith("channel_")
    ? "渠道"
    : key.startsWith("cooldown_") || key.includes("cooldown")
      ? "冷却"
      : key.startsWith("log_") || key.startsWith("debug_")
        ? "日志"
        : key.startsWith("auth_")
          ? "访问控制"
          : key.includes("timeout")
            ? "超时"
            : key.includes("pricing")
              ? "计费"
              : "高级";
const jsonObject = (value: string, fallback: Record<string, unknown>) => {
  try {
    const parsed = JSON.parse(value);
    return parsed && typeof parsed === "object" && !Array.isArray(parsed)
      ? (parsed as Record<string, unknown>)
      : fallback;
  } catch {
    return fallback;
  }
};
const readCooldownRules = (value: string): CooldownRule[] => {
  const parsed = jsonObject(value, {});
  return Array.isArray(parsed.rules) ? (parsed.rules as CooldownRule[]) : [];
};
const readFallbackRules = (
  value: string,
): Array<{ from: string; to: string }> => {
  const parsed = jsonObject(value, {});
  return Object.entries(parsed).map(([from, to]) => ({
    from,
    to: String(to ?? ""),
  }));
};
const readPricing = (value: string): PricingEntry[] => {
  const parsed = jsonObject(value, {});
  return Object.entries(parsed).map(([model, raw]) => ({
    model,
    ...(raw && typeof raw === "object" && !Array.isArray(raw)
      ? (raw as object)
      : {}),
  }));
};
const runtimeValue = (value: unknown, format?: RuntimeMetric["format"]) => {
  if (value == null || value === "") return "-";
  const number = Number(value);
  if (format === "boolean") return value === true || value === "true" ? "是" : "否";
  if (!Number.isFinite(number)) return String(value);
  if (format === "bytes") {
    const units = ["B", "KiB", "MiB", "GiB"]; let amount = number; let index = 0;
    while (amount >= 1024 && index < units.length - 1) { amount /= 1024; index += 1; }
    return `${amount.toFixed(index ? 1 : 0)} ${units[index]}`;
  }
  if (format === "duration") { const seconds = Math.round(number); if (seconds >= 3600) return `${Math.floor(seconds / 3600)} 小时 ${Math.floor(seconds % 3600 / 60)} 分`; if (seconds >= 60) return `${Math.floor(seconds / 60)} 分 ${seconds % 60} 秒`; return `${seconds} 秒`; }
  if (format === "percent") return `${number.toFixed(1)}%`;
  return Number.isInteger(number) ? number.toLocaleString() : number.toFixed(2);
};
function RuntimeMetricsView({ value }: { value: unknown }) {
  if (!value || typeof value !== "object") return <div className="muted">运行指标不可用</div>;
  const payload = value as Record<string, unknown>;
  return <div className="runtime-metrics-grid">{Object.entries(runtimeGroups).flatMap(([domain, metrics]) => {
    const source = payload[domain];
    if (!source || typeof source !== "object") return [];
    const stats = source as Record<string, unknown>;
    return [<section className="runtime-metrics-section" key={domain}><h3>{domain === "responses_websocket" ? "Responses / WebSocket" : domain}</h3><div className="runtime-metrics-cards">{metrics.map((metric) => <div className="runtime-metric-card" key={metric.key}><span className="muted">{metric.label}</span><strong>{runtimeValue(stats[metric.key], metric.format)}</strong><code>{metric.key}</code></div>)}</div></section>];
  })}</div>;
}

export function SettingsPage() {
  const [settings, setSettings] = useState<Setting[]>([]);
  const [drafts, setDrafts] = useState<Record<string, string>>({});
  const [group, setGroup] = useState("all");
  const [runtime, setRuntime] = useState<unknown>(null);
  const [pricing, setPricing] = useState("");
  const [cooldown, setCooldown] = useState("");
  const [fallback, setFallback] = useState("");
  const [typeSafeKey, setTypeSafeKey] = useState("");
  const [result, setResult] = useState<unknown>(null);
  const [loading, setLoading] = useState(false);
  const [saving, setSaving] = useState(false);
  const [cooldownRows, setCooldownRows] = useState<CooldownRule[]>([]);
  const [fallbackRows, setFallbackRows] = useState<
    Array<{ from: string; to: string }>
  >([]);
  const [pricingRows, setPricingRows] = useState<PricingEntry[]>([]);
  const [modelOptions, setModelOptions] = useState<string[]>([]);
  const load = async () => {
    setLoading(true);
    try {
      const rows = await getJSON<Setting[]>("/admin/settings");
      setSettings(rows);
      const values = Object.fromEntries(
        rows.map((row) => [row.key, String(row.value ?? "")]),
      );
      setDrafts(values);
      setCooldown(values.global_cooldown_detection_rules ?? '{"rules":[]}');
      setFallback(values.model_multimodal_fallback ?? "{}");
      setCooldownRows(
        readCooldownRules(values.global_cooldown_detection_rules ?? ""),
      );
      setFallbackRows(
        readFallbackRules(values.model_multimodal_fallback ?? ""),
      );
      setPricingRows(readPricing(values.model_custom_pricing ?? "{}"));
    } finally {
      setLoading(false);
    }
  };
  useEffect(() => {
    void load();
    void getJSON<{ models?: string[] }>("/admin/channels/filter-options", {
      status: "enabled",
    })
      .then((value) => setModelOptions(value.models ?? []))
      .catch(() => setModelOptions([]));
  }, []);
  const visible = useMemo(
    () =>
      settings.filter((row) => group === "all" || groupOf(row.key) === group),
    [settings, group],
  );
  const dirtySettings = settings.filter(
    (row) =>
      row.editable !== false &&
      (drafts[row.key] ?? String(row.value ?? "")) !== String(row.value ?? ""),
  );
  const saveAll = async () => {
    if (!dirtySettings.length) {
      setResult({ message: "没有待保存的修改" });
      return;
    }
    const updates = Object.fromEntries(
      dirtySettings.map((row) => [
        row.key,
        drafts[row.key] ?? String(row.value ?? ""),
      ]),
    );
    const minCooldown = Number(updates.cooldown_min_seconds ?? drafts.cooldown_min_seconds ?? NaN);
    const maxCooldown = Number(updates.cooldown_max_seconds ?? drafts.cooldown_max_seconds ?? NaN);
    if (Number.isFinite(minCooldown) && Number.isFinite(maxCooldown) && (minCooldown < 1 || maxCooldown < 1 || minCooldown > maxCooldown)) {
      setResult({ error: "冷却最小秒数必须小于或等于最大秒数，且都必须大于 0" });
      return;
    }
    if (!window.confirm(`确定保存 ${dirtySettings.length} 项设置吗？`)) return;
    setSaving(true);
    try {
      const response = await postJSON("/admin/settings/batch", updates);
      setResult(response);
      await load();
    } catch (cause) {
      setResult({
        error: cause instanceof Error ? cause.message : "批量保存失败",
      });
    } finally {
      setSaving(false);
    }
  };
  const saveOne = async (key: string) => {
    setSaving(true);
    try {
      const response = await putJSON(
        `/admin/settings/${encodeURIComponent(key)}`,
        { value: drafts[key] ?? "" },
      );
      setResult(response);
      await load();
    } catch (cause) {
      setResult({ error: cause instanceof Error ? cause.message : "保存失败" });
    } finally {
      setSaving(false);
    }
  };
  const resetOne = async (key: string) => {
    if (!window.confirm(`确定将 ${key} 恢复为默认值吗？`)) return;
    setSaving(true);
    try {
      const response = await postJSON(
        `/admin/settings/${encodeURIComponent(key)}/reset`,
      );
      setResult(response);
      await load();
    } catch (cause) {
      setResult({ error: cause instanceof Error ? cause.message : "重置失败" });
    } finally {
      setSaving(false);
    }
  };
  const saveJSON = async (key: string, value: string) => {
    try {
      JSON.parse(value);
    } catch {
      window.alert("JSON 格式无效");
      return;
    }
    await postJSON("/admin/settings/batch", { [key]: value });
    await load();
  };
  const saveCooldownRows = async () => {
    const rules = cooldownRows.map((rule, priority) => ({ ...rule, priority }));
    const value = JSON.stringify({ rules });
    setCooldownRows(rules);
    setCooldown(value);
    await saveJSON("global_cooldown_detection_rules", value);
  };
  const saveFallbackRows = async () => {
    const value = JSON.stringify(
      Object.fromEntries(
        fallbackRows
          .filter((row) => row.from.trim() && row.to.trim())
          .map((row) => [row.from.trim(), row.to.trim()]),
      ),
    );
    setFallback(value);
    await saveJSON("model_multimodal_fallback", value);
  };
  const savePricingRows = async () => {
    const value = JSON.stringify(
      Object.fromEntries(
        pricingRows
          .filter((row) => row.model.trim())
          .map((row) => [
            row.model.trim(),
            Object.fromEntries(
              pricingFields.flatMap((key) => {
                const item = row[key];
                return item === undefined ? [] : [[key, Number(item)]];
              }),
            ),
          ]),
      ),
    );
    setPricing(value);
    await saveJSON("model_custom_pricing", value);
  };
  return (
    <>
      <header className="page-header">
        <div>
          <h1>系统设置</h1>
          <p className="muted">运行参数、冷却策略、模型价格和多模态回退</p>
        </div>
        <button className="btn" onClick={() => void load()} disabled={loading || saving}>
          刷新
        </button>
      </header>
      <div className="toolbar">
        <select
          className="select"
          value={group}
          onChange={(event) => setGroup(event.target.value)}
        >
          <option value="all">全部分组</option>
          <option value="渠道">渠道</option>
          <option value="冷却">冷却</option>
          <option value="日志">日志</option>
          <option value="访问控制">访问控制</option>
          <option value="超时">超时</option>
          <option value="计费">计费</option>
          <option value="高级">高级</option>
        </select>
        <button className="btn btn-primary" onClick={() => void saveAll()} disabled={loading || saving}>
          {saving ? "保存中..." : `保存全部修改${dirtySettings.length ? ` (${dirtySettings.length})` : ""}`}
        </button>
        <button
          className="btn"
          onClick={() =>
            void getJSON("/admin/runtime-metrics").then(setRuntime)
          }
        >
          运行指标
        </button>
        <button
          className="btn"
          onClick={() =>
            void postJSON("/admin/update/check", {}).then(setResult)
          }
        >
          检查更新
        </button>
      </div>
      {runtime !== null && (
        <div className="card">
          <div className="toolbar">
            <h2>运行指标</h2>
            <button className="btn" onClick={() => setRuntime(null)}>
              关闭
            </button>
          </div>
          <RuntimeMetricsView value={runtime} />
        </div>
      )}
      <div className="advanced-grid">
        <div className="card">
          <h2>全局冷却规则</h2>
          <div className="table-wrap">
            <table>
              <thead>
                <tr>
                  <th>规则名</th>
                  <th>范围</th>
                  <th>模式</th>
                  <th>状态码</th>
                  <th>消息正则与重置时间</th>
                  <th>冷却秒数</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {cooldownRows.map((row, index) => (
                  <tr key={index}>
                    <td>
                      <input
                        className="input"
                        value={row.name ?? ""}
                        placeholder="规则名"
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? { ...item, name: event.target.value }
                                : item,
                            ),
                          )
                        }
                      />
                    </td>
                    <td>
                      <select
                        className="select"
                        value={row.scope ?? "key"}
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? { ...item, scope: event.target.value }
                                : item,
                            ),
                          )
                        }
                      >
                        <option value="key">Key</option>
                        <option value="model">模型</option>
                        <option value="channel">渠道</option>
                      </select>
                    </td>
                    <td>
                      <select
                        className="select"
                        value={row.mode ?? "fixed"}
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? { ...item, mode: event.target.value }
                                : item,
                            ),
                          )
                        }
                      >
                        <option value="fixed">固定</option>
                        <option value="reset_time">重置时间</option>
                      </select>
                    </td>
                    <td>
                      <input
                        className="input"
                        value={(row.status_codes ?? []).join(",")}
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? {
                                    ...item,
                                    status_codes: event.target.value
                                      .split(",")
                                      .map(Number)
                                      .filter(Number.isFinite),
                                  }
                                : item,
                            ),
                          )
                        }
                      />
                    </td>
                    <td>
                      <input
                        className="input wide"
                        value={row.message_pattern ?? ""}
                        placeholder="匹配消息正则"
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? {
                                    ...item,
                                    message_pattern: event.target.value,
                                  }
                                : item,
                            ),
                          )
                        }
                      />
                      <input
                        className="input"
                        value={row.time_capture ?? ""}
                        placeholder="命名捕获组"
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? { ...item, time_capture: event.target.value }
                                : item,
                            ),
                          )
                        }
                      />
                      <select
                        className="select"
                        value={row.time_format ?? "datetime"}
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? { ...item, time_format: event.target.value }
                                : item,
                            ),
                          )
                        }
                      >
                        <option value="datetime">datetime</option>
                        <option value="time_of_day">time_of_day</option>
                        <option value="unix">unix</option>
                        <option value="unix_ms">unix_ms</option>
                        <option value="duration_seconds">
                          duration_seconds
                        </option>
                      </select>
                      <input
                        className="input"
                        value={row.time_layout ?? ""}
                        placeholder="时间布局"
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? { ...item, time_layout: event.target.value }
                                : item,
                            ),
                          )
                        }
                      />
                      <input
                        className="input"
                        value={row.timezone ?? ""}
                        placeholder="时区"
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? { ...item, timezone: event.target.value }
                                : item,
                            ),
                          )
                        }
                      />
                    </td>
                    <td>
                      <input
                        className="input compact"
                        type="number"
                        min="1"
                        value={Number(row.cooldown_seconds ?? 60)}
                        onChange={(event) =>
                          setCooldownRows((items) =>
                            items.map((item, itemIndex) =>
                              itemIndex === index
                                ? {
                                    ...item,
                                    cooldown_seconds: Number(
                                      event.target.value,
                                    ),
                                  }
                                : item,
                            ),
                          )
                        }
                      />
                    </td>
                    <td>
                      <button
                        className="btn"
                        onClick={() =>
                          setCooldownRows((items) =>
                            items.filter((_, itemIndex) => itemIndex !== index),
                          )
                        }
                      >
                        删除
                      </button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <div className="toolbar">
            <button
              className="btn"
              onClick={() =>
                setCooldownRows((items) => [
                  ...items,
                  {
                    enabled: true,
                    name: "新规则",
                    priority: items.length,
                    scope: "key",
                    mode: "fixed",
                    status_codes: [429],
                    cooldown_seconds: 60,
                  },
                ])
              }
            >
              添加规则
            </button>
            <button className="btn" onClick={() => void saveCooldownRows()}>
              保存冷却规则
            </button>
          </div>
        </div>
        <div className="card">
          <h2>多模态回退映射</h2>
          <datalist id="fallback-model-options">
            {modelOptions.map((model) => (
              <option key={model} value={model} />
            ))}
          </datalist>
          {fallbackRows.map((row, index) => (
            <div className="toolbar" key={index}>
              <input
                className="input"
                list="fallback-model-options"
                value={row.from}
                placeholder="原模型"
                onChange={(event) =>
                  setFallbackRows((items) =>
                    items.map((item, itemIndex) =>
                      itemIndex === index
                        ? { ...item, from: event.target.value }
                        : item,
                    ),
                  )
                }
              />
              <span>→</span>
              <input
                className="input"
                list="fallback-model-options"
                value={row.to}
                placeholder="回退模型"
                onChange={(event) =>
                  setFallbackRows((items) =>
                    items.map((item, itemIndex) =>
                      itemIndex === index
                        ? { ...item, to: event.target.value }
                        : item,
                    ),
                  )
                }
              />
              <button
                className="btn"
                onClick={() =>
                  setFallbackRows((items) =>
                    items.filter((_, itemIndex) => itemIndex !== index),
                  )
                }
              >
                删除
              </button>
            </div>
          ))}
          <div className="toolbar">
            <button
              className="btn"
              onClick={() =>
                setFallbackRows((items) => [...items, { from: "", to: "" }])
              }
            >
              添加映射
            </button>
            <button className="btn" onClick={() => void saveFallbackRows()}>
              保存回退映射
            </button>
          </div>
        </div>
      </div>
      <div className="card">
        <h2>模型价格</h2>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>模型</th>
                <th>输入</th>
                <th>输出</th>
                <th>缓存读</th>
                <th>缓存写</th>
                <th>高上下文输入</th>
                <th>高上下文输出</th>
                <th>高上下文缓存读</th>
                <th>高上下文缓存写</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {pricingRows.map((row, index) => (
                <tr key={index}>
                  <td>
                    <input
                      className="input"
                      value={row.model}
                      onChange={(event) =>
                        setPricingRows((items) =>
                          items.map((item, itemIndex) =>
                            itemIndex === index
                              ? { ...item, model: event.target.value }
                              : item,
                          ),
                        )
                      }
                    />
                  </td>
                  {pricingFields.map((key) => (
                    <td key={key}>
                      <input
                        className="input compact"
                        type="number"
                        min="0"
                        step="any"
                        value={row[key] ?? ""}
                        onChange={(event) =>
                          setPricingRows((items) =>
                            items.map((item, itemIndex) => {
                              if (itemIndex !== index) return item;
                              const next = { ...item };
                              if (event.target.value === "") delete next[key];
                              else next[key] = Number(event.target.value);
                              return next;
                            }),
                          )
                        }
                      />
                    </td>
                  ))}
                  <td>
                    <button
                      className="btn"
                      onClick={() =>
                        setPricingRows((items) =>
                          items.filter((_, itemIndex) => itemIndex !== index),
                        )
                      }
                    >
                      删除
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <div className="toolbar">
          <button
            className="btn"
            onClick={() =>
              setPricingRows((items) => [
                ...items,
                { model: "", input_price: 0, output_price: 0 },
              ])
            }
          >
            添加模型
          </button>
          <button className="btn" onClick={() => void savePricingRows()}>
            保存价格
          </button>
        </div>
      </div>
      <div className="card">
        <h2>TypeSafe 测试</h2>
        <div className="toolbar">
          <input
            className="input wide"
            value={typeSafeKey}
            onChange={(event) => setTypeSafeKey(event.target.value)}
            placeholder="API Key"
          />
          <button
            className="btn"
            disabled={!typeSafeKey}
            onClick={() =>
              void postJSON("/admin/typesafe/test", {
                api_key: typeSafeKey,
              }).then(setResult)
            }
          >
            测试
          </button>
        </div>
      </div>
      {result !== null && (
        <pre className="result-pre">{JSON.stringify(result, null, 2)}</pre>
      )}
      <div className="table-wrap">
        <table>
          <thead>
            <tr>
              <th>分组</th>
              <th>配置项</th>
              <th>当前值</th>
              <th>说明</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={5}>加载中...</td>
              </tr>
            ) : (
              visible.map((row) => (
                <tr key={row.key}>
                  <td>{groupOf(row.key)}</td>
                  <td>
                    <code>{row.key}</code>
                  </td>
                  <td>
                    <input
                      className="input"
                      disabled={row.editable === false}
                      value={drafts[row.key] ?? String(row.value ?? "")}
                      onChange={(event) =>
                        setDrafts({ ...drafts, [row.key]: event.target.value })
                      }
                    />
                  </td>
                  <td className="muted">{row.description ?? "-"}</td>
                  <td>
                    <button
                      className="btn"
                      disabled={row.editable === false || saving}
                      onClick={() => void saveOne(row.key)}
                    >
                      保存
                    </button>
                    <button
                      className="btn"
                      disabled={row.editable === false || saving || row.default_value === undefined}
                      onClick={() => void resetOne(row.key)}
                    >
                      重置
                    </button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </>
  );
}
