import { useEffect, useState } from "react";
import { getJSON, getPaginated, postJSON } from "../lib/api";
import { getSession, getToken } from "../lib/auth";
import type { Channel, Session } from "../types";

type Entry = { model: string; redirect_model?: string; disabled?: boolean };
type TestMode = "channel" | "model" | "chat" | "image";
type Key = {
  key_index?: number;
  api_key?: string;
  disabled?: boolean;
  note?: string;
};
type ChatBlock =
  | { type: "text"; text: string }
  | { type: "image_url"; image_url: { url: string } };
type ChatMessage = {
  role: string;
  content: string | ChatBlock[];
  thinking?: string;
};
type PendingImage = { id: string; name: string; dataUrl: string };
const CHAT_STORAGE_KEY = "ccload_model_test_chat_messages";
const CHAT_OPTIONS_KEY = "ccload_model_test_chat_advanced_options";
const readStoredFlag = (key: string, fallback: boolean) => {
  try {
    const value = localStorage.getItem(key);
    return value == null ? fallback : value === "1";
  } catch {
    return fallback;
  }
};
const readStoredText = (key: string) => {
  try {
    return localStorage.getItem(key) ?? "";
  } catch {
    return "";
  }
};
const readSavedChat = (): { sessionId: string; messages: ChatMessage[] } => {
  try {
    const value = JSON.parse(
      localStorage.getItem(CHAT_STORAGE_KEY) ?? "{}",
    ) as { session_id?: string; messages?: ChatMessage[] };
    return {
      sessionId: value.session_id || crypto.randomUUID(),
      messages: Array.isArray(value.messages) ? value.messages : [],
    };
  } catch {
    return { sessionId: crypto.randomUUID(), messages: [] };
  }
};
const readAdvancedOptions = () => {
  try {
    return JSON.parse(localStorage.getItem(CHAT_OPTIONS_KEY) ?? "{}") as {
      systemPrompt?: string;
      temperature?: number | null;
      topP?: number | null;
      contextMessages?: number | null;
      maxTokens?: number | null;
    };
  } catch {
    return {};
  }
};

function extractSSEText(value: unknown): string {
  if (!value || typeof value !== "object")
    return typeof value === "string" ? value : "";
  const data = value as Record<string, unknown>;
  const choices = Array.isArray(data.choices) ? data.choices : [];
  const choice = (choices[0] ?? {}) as Record<string, unknown>;
  const delta = (choice.delta ?? {}) as Record<string, unknown>;
  const message = (choice.message ?? {}) as Record<string, unknown>;
  return String(
    delta.content ??
      message.content ??
      choice.text ??
      data.content ??
      data.text ??
      "",
  );
}

const chatMessageText = (message: ChatMessage) =>
  typeof message.content === "string"
    ? message.content
    : message.content
        .filter(
          (part): part is Extract<ChatBlock, { type: "text" }> =>
            part.type === "text",
        )
        .map((part) => part.text)
        .join("\n");

const readPendingImages = (files: File[]) =>
  Promise.all(
    files.map(
      (file) =>
        new Promise<PendingImage>((resolve, reject) => {
          const reader = new FileReader();
          reader.onload = () =>
            resolve({
              id: crypto.randomUUID(),
              name: file.name || "clipboard-image",
              dataUrl: String(reader.result ?? ""),
            });
          reader.onerror = () => reject(reader.error);
          reader.readAsDataURL(file);
        }),
    ),
  );

function findGeneratedImages(
  value: unknown,
): Array<{ src: string; label: string }> {
  if (!value || typeof value !== "object") return [];
  const data = value as Record<string, unknown>;
  const list = Array.isArray(data.data)
    ? data.data
    : Array.isArray(data.images)
      ? data.images
      : [];
  return list.flatMap((item, index) => {
    if (!item || typeof item !== "object") return [];
    const image = item as Record<string, unknown>;
    const src =
      typeof image.url === "string"
        ? image.url
        : typeof image.b64_json === "string"
          ? `data:image/png;base64,${image.b64_json}`
          : typeof image.base64 === "string"
            ? `data:${String(image.mime_type ?? "image/png")};base64,${image.base64}`
            : "";
    return src
      ? [
          {
            src,
            label: String(image.revised_prompt ?? `生成图片 ${index + 1}`),
          },
        ]
      : [];
  });
}

export function ModelTestPage() {
  const [savedChat] = useState(readSavedChat);
  const [sessionId, setSessionId] = useState(savedChat.sessionId);
  const [session, setSession] = useState<Session | null>(null);
  const [tokenModel, setTokenModel] = useState("");
  const [tokenProtocol, setTokenProtocol] = useState("openai");
  const [tokenContent, setTokenContent] = useState("Hello");
  const [tokenStream, setTokenStream] = useState(true);
  const [tokenModels, setTokenModels] = useState<string[]>([]);
  const [tokenResult, setTokenResult] = useState("");
  const [tokenBusy, setTokenBusy] = useState(false);
  const [channelRows, setChannelRows] = useState<Channel[]>([]);
  const [testMode, setTestMode] = useState<TestMode>("channel");
  const [channelId, setChannelId] = useState<number | "">("");
  const [model, setModel] = useState("");
  const [protocol, setProtocol] = useState("openai");
  const [content, setContent] = useState("hi");
  const [chatInput, setChatInput] = useState("");
  const [stream, setStream] = useState(true);
  const [chatStream, setChatStream] = useState(() =>
    readStoredFlag("ccload_model_test_chat_stream_enabled", true),
  );
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<unknown>(null);
  const [chat, setChat] = useState<ChatMessage[]>(savedChat.messages);
  const [selected, setSelected] = useState<string[]>([]);
  const [modelModeSelected, setModelModeSelected] = useState<string[]>([]);
  const [modelModeFilter, setModelModeFilter] = useState("");
  const [manualModels, setManualModels] = useState("");
  const [entries, setEntries] = useState<Entry[]>([]);
  const [batch, setBatch] = useState<Array<Record<string, unknown>>>([]);
  const [batchProgress, setBatchProgress] = useState({ done: 0, total: 0 });
  const [batchStatus, setBatchStatus] = useState("");
  const [concurrency, setConcurrency] = useState(5);
  const [keys, setKeys] = useState<Key[]>([]);
  const [keyIndex, setKeyIndex] = useState("");
  const [image, setImage] = useState({
    generation_api: "images",
    size: "auto",
    quality: "auto",
    background: "auto",
    output_format: "auto",
  });
  const [advanced, setAdvanced] = useState({
    systemPrompt: String(readAdvancedOptions().systemPrompt ?? ""),
    temperature:
      readAdvancedOptions().temperature == null
        ? ""
        : String(readAdvancedOptions().temperature),
    topP:
      readAdvancedOptions().topP == null
        ? ""
        : String(readAdvancedOptions().topP),
    maxTokens:
      readAdvancedOptions().maxTokens == null
        ? ""
        : String(readAdvancedOptions().maxTokens),
    contextMessages:
      readAdvancedOptions().contextMessages == null
        ? "0"
        : String(readAdvancedOptions().contextMessages),
  });
  const [showAdvanced, setShowAdvanced] = useState(false);
  const [priority, setPriority] = useState("0");
  const [thinkingEffort, setThinkingEffort] = useState(() =>
    readStoredText("ccload_model_test_chat_thinking_effort"),
  );
  const [builtinSearch, setBuiltinSearch] = useState(() =>
    readStoredFlag("ccload_model_test_chat_builtin_search", false),
  );
  const [pendingImages, setPendingImages] = useState<PendingImage[]>([]);
  useEffect(() => {
    void getSession().then(async (value) => {
      setSession(value);
      if (value?.role === "api_token") {
        const allowed = Array.isArray(value.allowed_models)
          ? value.allowed_models.map(String)
          : [];
        const choices = allowed.length
          ? allowed
          : ((
              await getJSON<{ models?: string[] }>("/dashboard/models", {
                range: "this_month",
              }).catch(() => ({ models: [] }))
            ).models ?? []);
        setTokenModels(choices);
        setTokenModel(choices[0] ?? "");
        setTokenContent(String(value.default_test_content ?? "Hello"));
        return;
      }
      const settings = await getJSON<Array<{ key: string; value?: string }>>(
        "/admin/settings",
      ).catch(() => []);
      const configuredContent = settings
        .find((item) => item.key === "channel_test_content")
        ?.value?.split("|")
        .map((item) => item.trim())
        .find(Boolean);
      if (configuredContent) setContent(configuredContent);
      const valueChannels = await getPaginated<Channel>("/admin/channels", {
        limit: 500,
        offset: 0,
      }).catch(() => ({ data: [], count: 0 }));
      setChannelRows(valueChannels.data);
    });
  }, []);
  useEffect(() => {
    try {
      localStorage.setItem(
        CHAT_STORAGE_KEY,
        JSON.stringify({ session_id: sessionId, messages: chat }),
      );
    } catch {
      /* Storage can be unavailable or full. */
    }
  }, [chat, sessionId]);
  useEffect(() => {
    try {
      localStorage.setItem(
        CHAT_OPTIONS_KEY,
        JSON.stringify({
          systemPrompt: advanced.systemPrompt,
          temperature:
            advanced.temperature === "" ? null : Number(advanced.temperature),
          topP: advanced.topP === "" ? null : Number(advanced.topP),
          contextMessages:
            advanced.contextMessages === ""
              ? null
              : Number(advanced.contextMessages),
          maxTokens:
            advanced.maxTokens === "" ? null : Number(advanced.maxTokens),
        }),
      );
    } catch {
      /* Storage can be unavailable. */
    }
  }, [advanced]);
  useEffect(() => {
    try {
      localStorage.setItem(
        "ccload_model_test_chat_stream_enabled",
        chatStream ? "1" : "0",
      );
      localStorage.setItem(
        "ccload_model_test_chat_thinking_effort",
        thinkingEffort,
      );
      localStorage.setItem(
        "ccload_model_test_chat_builtin_search",
        builtinSearch ? "1" : "0",
      );
    } catch {
      /* Storage can be unavailable. */
    }
  }, [chatStream, thinkingEffort, builtinSearch]);
  useEffect(() => {
    if (!channelId) {
      setKeys([]);
      return;
    }
    void getJSON<Key[]>(`/admin/channels/${channelId}/keys`)
      .then((value) => {
        setKeys(value);
        const first = value.find((item) => !item.disabled);
        setKeyIndex(first?.key_index == null ? "" : String(first.key_index));
      })
      .catch(() => setKeys([]));
  }, [channelId]);
  const allModels = Array.from(
    new Set(
      channelRows.flatMap((row) =>
        Array.isArray(row.models) ? row.models.map(String) : [],
      ),
    ),
  );
  const modelModeRows = channelRows.flatMap((row) => (Array.isArray(row.models) ? row.models : []).map((entry) => {
    const modelName = typeof entry === "string" ? entry : String((entry as { model?: unknown }).model ?? "");
    const modelEntry = typeof entry === "string" ? { model: entry } : entry as Entry;
    return { key: `${row.id}:${modelName}`, channelId: row.id, channel: row.name, model: modelName, disabled: Boolean(modelEntry.disabled), redirect_model: modelEntry.redirect_model };
  })).filter((entry) => entry.model && (!modelModeFilter.trim() || entry.model.toLowerCase().includes(modelModeFilter.trim().toLowerCase()) || entry.channel.toLowerCase().includes(modelModeFilter.trim().toLowerCase())));
  const generatedImages = findGeneratedImages(result);
  const loadEntries = async () => {
    if (!channelId) return;
    const value = await getJSON<{ models?: Array<string | Entry> }>(
      `/admin/channels/${channelId}/models/fetch`,
    );
    setEntries(
      (value.models ?? []).map((entry) =>
        typeof entry === "string" ? { model: entry } : entry,
      ),
    );
  };
  const fetchAndAddModels = async () => {
    if (!channelId) return;
    setBusy(true);
    try {
      const fetched = await getJSON<{ models?: Array<string | Entry> }>(
        `/admin/channels/${channelId}/models/fetch`,
      );
      const current = channelRows.find((row) => row.id === channelId);
      const existing = new Set((current?.models ?? []).map(String));
      const added = (fetched.models ?? [])
        .map((entry) => (typeof entry === "string" ? { model: entry } : entry))
        .filter((entry) => entry.model && !existing.has(entry.model));
      if (added.length) {
        await postJSON(`/admin/channels/${channelId}/models`, {
          models: added,
        });
        setChannelRows((rows) =>
          rows.map((row) =>
            row.id === channelId
              ? {
                  ...row,
                  models: [
                    ...(row.models ?? []),
                    ...added.map((entry) => entry.model),
                  ],
                }
              : row,
          ),
        );
      }
      setEntries((currentEntries) => [
        ...currentEntries,
        ...added.filter(
          (entry) =>
            !currentEntries.some(
              (currentEntry) => currentEntry.model === entry.model,
            ),
        ),
      ]);
      setResult({ fetched: fetched.models?.length ?? 0, added: added.length });
    } catch (cause) {
      setResult({
        error: cause instanceof Error ? cause.message : "获取模型失败",
      });
    } finally {
      setBusy(false);
    }
  };
  const addManualModels = async () => {
    if (!channelId) return;
    const names = [
      ...new Set(
        manualModels
          .split(/[\n,]/)
          .map((item) => item.trim())
          .filter(Boolean),
      ),
    ];
    if (!names.length) return;
    const newEntries = names
      .filter((name) => !entries.some((entry) => entry.model === name))
      .map((name) => ({ model: name }));
    if (!newEntries.length) return;
    await postJSON(`/admin/channels/${channelId}/models`, {
      models: newEntries,
    });
    setEntries((current) => [...current, ...newEntries]);
    setChannelRows((rows) =>
      rows.map((row) =>
        row.id === channelId
          ? {
              ...row,
              models: [
                ...(row.models ?? []),
                ...newEntries.map((entry) => entry.model),
              ],
            }
          : row,
      ),
    );
    setManualModels("");
  };
  const requestBody = (targetModel: string) => ({
    model: targetModel,
    content,
    stream,
    client_protocol: protocol,
    key_index: keyIndex === "" ? undefined : Number(keyIndex),
    system_prompt: advanced.systemPrompt || undefined,
    temperature:
      advanced.temperature === "" ? undefined : Number(advanced.temperature),
    top_p: advanced.topP === "" ? undefined : Number(advanced.topP),
    max_tokens:
      advanced.maxTokens === "" ? undefined : Number(advanced.maxTokens),
  });
  const test = async (targetChannel = channelId, targetModel = model) => {
    if (!targetChannel || !targetModel) return null;
    const body = requestBody(targetModel);
    if (targetChannel !== channelId) body.key_index = undefined;
    return postJSON<Record<string, unknown>>(
      `/admin/channels/${targetChannel}/test`,
      body,
    );
  };
  const run = async () => {
    setBusy(true);
    try {
      setResult(await test());
    } finally {
      setBusy(false);
    }
  };
  const send = async () => {
    const text = chatInput.trim();
    if (!channelId || !model || (!text && pendingImages.length === 0) || busy)
      return;
    const userContent: string | ChatBlock[] = pendingImages.length
      ? [
          ...(text ? [{ type: "text" as const, text }] : []),
          ...pendingImages.map((item) => ({
            type: "image_url" as const,
            image_url: { url: item.dataUrl },
          })),
        ]
      : text;
    const messages = [...chat, { role: "user", content: userContent }];
    const contextCount = Math.max(0, Number(advanced.contextMessages) || 0);
    const context = contextCount > 0 ? messages.slice(-contextCount) : messages;
    const options = {
      model,
      messages: context,
      stream: chatStream,
      client_protocol: protocol,
      key_index: keyIndex === "" ? undefined : Number(keyIndex),
      session_id: sessionId,
      thinking_effort: thinkingEffort || undefined,
      builtin_search: builtinSearch,
      system_prompt: advanced.systemPrompt.trim() || undefined,
      temperature:
        advanced.temperature === "" ? undefined : Number(advanced.temperature),
      top_p: advanced.topP === "" ? undefined : Number(advanced.topP),
      max_tokens:
        advanced.maxTokens === "" ? undefined : Number(advanced.maxTokens),
    };
    setChat(messages);
    setChatInput("");
    setPendingImages([]);
    setBusy(true);
    try {
      const response = await fetch(`/admin/channels/${channelId}/chat`, {
        method: "POST",
        credentials: "include",
        headers: {
          "Content-Type": "application/json",
          Accept: "text/event-stream",
          ...(getToken() ? { Authorization: `Bearer ${getToken()}` } : {}),
        },
        body: JSON.stringify(options),
      });
      if (!response.ok || !response.body)
        throw new Error(`聊天请求失败 (${response.status})`);
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      let assistant = "";
      let thinking = "";
      const consume = (chunk: string, flush = false) => {
        buffer += chunk;
        const events = buffer.split(/\r?\n\r?\n/);
        buffer = flush ? "" : (events.pop() ?? "");
        for (const event of events) {
          const line = event
            .split(/\r?\n/)
            .find((item) => item.startsWith("data:"));
          if (!line) continue;
          const raw = line.slice(5).trim();
          if (!raw || raw === "[DONE]") continue;
          let parsed: Record<string, unknown>;
          try {
            parsed = JSON.parse(raw) as Record<string, unknown>;
          } catch {
            assistant += raw;
            setResult({ stream: assistant });
            continue;
          }
          if (parsed.error) throw new Error(String(parsed.error));
          if (typeof parsed.delta === "string") assistant += parsed.delta;
          if (typeof parsed.thinking_delta === "string")
            thinking += parsed.thinking_delta;
          if (
            typeof parsed.delta !== "string" &&
            typeof parsed.thinking_delta !== "string"
          )
            assistant += extractSSEText(parsed);
          setResult({ stream: assistant, thinking, event: parsed });
        }
      };
      while (true) {
        const part = await reader.read();
        if (part.done) break;
        consume(decoder.decode(part.value, { stream: true }));
      }
      consume(decoder.decode(), true);
      setChat((current) => [
        ...current,
        {
          role: "assistant",
          content: assistant,
          thinking: thinking || undefined,
        },
      ]);
    } catch (cause) {
      setResult({
        error: cause instanceof Error ? cause.message : "聊天请求失败",
      });
    } finally {
      setBusy(false);
    }
  };
  const imageGenerate = async () => {
    if (!channelId || !model) return;
    setBusy(true);
    try {
      setResult(
        await postJSON(`/admin/channels/${channelId}/images/generations`, {
          model,
          prompt: content,
          n: 1,
          key_index: keyIndex === "" ? undefined : Number(keyIndex),
          ...Object.fromEntries(
            Object.entries(image).filter(([, value]) => value !== "auto"),
          ),
        }),
      );
    } finally {
      setBusy(false);
    }
  };
  const runBatch = async () => {
    const targets = testMode === "model"
      ? modelModeRows.filter((row) => modelModeSelected.includes(row.key)).map(({ key: _key, disabled: _disabled, redirect_model: _redirect, ...target }) => target)
      : selected.flatMap((name) => channelRows.filter((row) => Array.isArray(row.models) && row.models.map(String).includes(name)).map((row) => ({ channelId: row.id, channel: row.name, model: name })));
    setBusy(true);
    setBatch([]);
    setBatchStatus("");
    setBatchProgress({ done: 0, total: targets.length });
    const values: Array<Record<string, unknown>> = [];
    let cursor = 0;
    const workers = Array.from(
      { length: Math.min(concurrency, targets.length) },
      async () => {
        while (cursor < targets.length) {
          const target = targets[cursor++];
          try {
            let result: Record<string, unknown> | null = null;
            for (let attempt = 0; attempt < 10; attempt += 1) {
              result = await test(target.channelId, target.model);
              if (result?.rpm_limited !== true) break;
              if (attempt === 9) throw new Error("RPM 限流后重试次数已达上限");
              const retryAfter = Math.max(1000, Number(result.retry_after_ms) || 60_000);
              const until = Date.now() + retryAfter;
              while (Date.now() < until) {
                const seconds = Math.max(1, Math.ceil((until - Date.now()) / 1000));
                setBatchStatus(`${target.channel} / ${target.model} 遇到 RPM 限流，${seconds} 秒后重试`);
                await new Promise((resolve) => window.setTimeout(resolve, Math.min(1000, until - Date.now())));
              }
            }
            values.push({
              ...target,
              ...(result ?? {}),
              success: result?.success !== false,
            });
          } catch (cause) {
            values.push({
              ...target,
              success: false,
              error: cause instanceof Error ? cause.message : "失败",
            });
          } finally {
            setBatchProgress((progress) => ({
              ...progress,
              done: progress.done + 1,
            }));
          }
        }
      },
    );
    await Promise.all(workers);
    setBatch(values);
    setBatchStatus("");
    setBusy(false);
  };
  const saveEntry = async (entry: Entry) => {
    if (!channelId) return;
    await postJSON(`/admin/channels/${channelId}/models`, { models: [entry] });
    await loadEntries();
  };
  const deleteSelectedEntries = async () => {
    if (!channelId || !selected.length) return;
    const channelName =
      channelRows.find((row) => row.id === channelId)?.name ??
      String(channelId);
    if (
      !window.confirm(
        `将从渠道“${channelName}”删除 ${selected.length} 个模型：\n${selected.join("\n")}`,
      )
    )
      return;
    await postJSON("/admin/channels/models/batch-delete", {
      operations: [{ channel_id: channelId, models: selected }],
    });
    setSelected([]);
    await loadEntries();
  };
  const savePriority = async () => {
    if (!channelId) return;
    await postJSON("/admin/channels/batch-priority", {
      updates: [{ id: channelId, priority: Number(priority) || 0 }],
    });
    setChannelRows((rows) =>
      rows.map((row) =>
        row.id === channelId
          ? { ...row, priority: Number(priority) || 0 }
          : row,
      ),
    );
  };
  const exportChat = (format: "md" | "html") => {
    const body = chat
      .map((item) => `## ${item.role}\n\n${chatMessageText(item)}`)
      .join("\n\n");
    const contentText =
      format === "html"
        ? `<html><body><pre>${body.replaceAll("&", "&amp;").replaceAll("<", "&lt;")}</pre></body></html>`
        : body;
    const link = document.createElement("a");
    link.href = URL.createObjectURL(
      new Blob([contentText], {
        type: format === "html" ? "text/html" : "text/markdown",
      }),
    );
    link.download = `chat.${format}`;
    link.click();
    window.setTimeout(() => URL.revokeObjectURL(link.href), 1000);
  };
  const runTokenTest = async () => {
    const normalized = ["anthropic", "openai", "codex", "gemini"].includes(
      tokenProtocol,
    )
      ? tokenProtocol
      : "openai";
    const endpoint =
      normalized === "anthropic"
        ? "/dashboard/v1/messages"
        : normalized === "codex"
          ? "/dashboard/v1/responses"
          : normalized === "gemini"
            ? `/dashboard/v1beta/models/${encodeURIComponent(tokenModel)}:${tokenStream ? "streamGenerateContent" : "generateContent"}`
            : "/dashboard/v1/chat/completions";
    const payload =
      normalized === "anthropic"
        ? {
            model: tokenModel,
            max_tokens: 1024,
            messages: [{ role: "user", content: tokenContent.trim() }],
            stream: tokenStream,
          }
        : normalized === "codex"
          ? {
              model: tokenModel,
              input: tokenContent.trim(),
              stream: tokenStream,
            }
          : normalized === "gemini"
            ? {
                contents: [
                  { role: "user", parts: [{ text: tokenContent.trim() }] },
                ],
              }
            : {
                model: tokenModel,
                messages: [{ role: "user", content: tokenContent.trim() }],
                stream: tokenStream,
              };
    if (!tokenModel.trim() || !tokenContent.trim()) {
      setTokenResult("模型和测试内容不能为空");
      return;
    }
    if (
      session?.allowed_models?.length &&
      !session.allowed_models.some(
        (item) => item.toLowerCase() === tokenModel.toLowerCase(),
      )
    ) {
      setTokenResult("此模型不在令牌允许范围内");
      return;
    }
    setTokenBusy(true);
    setTokenResult("请求中...");
    try {
      const response = await fetch(endpoint, {
        method: "POST",
        credentials: "include",
        headers: {
          "Content-Type": "application/json",
          ...(getToken() ? { Authorization: `Bearer ${getToken()}` } : {}),
        },
        body: JSON.stringify(payload),
      });
      const raw = await response.text();
      if (!response.ok) throw new Error(raw || `HTTP ${response.status}`);
      try {
        setTokenResult(JSON.stringify(JSON.parse(raw), null, 2));
      } catch {
        setTokenResult(raw);
      }
    } catch (cause) {
      setTokenResult(cause instanceof Error ? cause.message : "请求失败");
    } finally {
      setTokenBusy(false);
    }
  };
  if (session?.role === "api_token")
    return (
      <>
        <header className="page-header">
          <div>
            <h1>API Token 模型测试</h1>
            <p className="muted">{String(session.description ?? "")}</p>
          </div>
        </header>
        <div className="card">
          <div className="toolbar">
            <select
              className="select"
              value={tokenProtocol}
              onChange={(event) => setTokenProtocol(event.target.value)}
            >
              <option value="openai">OpenAI</option>
              <option value="anthropic">Anthropic</option>
              <option value="codex">Codex</option>
              <option value="gemini">Gemini</option>
            </select>
            <input
              className="input"
              list="token-models"
              value={tokenModel}
              onChange={(event) => setTokenModel(event.target.value)}
              placeholder="模型名称"
            />
            <datalist id="token-models">
              {tokenModels.map((item) => (
                <option key={item} value={item} />
              ))}
            </datalist>
            <label className="muted">
              <input
                type="checkbox"
                checked={tokenStream}
                onChange={(event) => setTokenStream(event.target.checked)}
              />{" "}
              流式
            </label>
            <button
              className="btn btn-primary"
              disabled={tokenBusy}
              onClick={() => void runTokenTest()}
            >
              {tokenBusy ? "测试中..." : "开始测试"}
            </button>
          </div>
          <textarea
            className="input wide test-content"
            value={tokenContent}
            onChange={(event) => setTokenContent(event.target.value)}
          />
          <pre className="result-pre" role="status">
            {tokenResult || "响应将显示在这里"}
          </pre>
        </div>
      </>
    );
  return (
    <>
      <header className="page-header">
        <div>
          <h1>模型测试</h1>
          <p className="muted">连通性、聊天、图片生成和模型批量测试</p>
        </div>
      </header>
      <div className="mode-tabs" role="tablist" aria-label="模型测试模式">
        {([['channel', '渠道模式'], ['model', '模型模式'], ['chat', '聊天模式'], ['image', '图片模式']] as const).map(([value, label]) => (
          <button key={value} type="button" role="tab" aria-selected={testMode === value} className={testMode === value ? 'mode-tab active' : 'mode-tab'} onClick={() => setTestMode(value)}>{label}</button>
        ))}
      </div>
      <div className="card">
        <div className="toolbar">
          <select
            className="select"
            value={channelId}
            onChange={(event) => {
              const value = event.target.value
                ? Number(event.target.value)
                : "";
              setChannelId(value);
              setModel("");
              setEntries([]);
              setPriority(
                String(
                  channelRows.find((row) => row.id === value)?.priority ?? 0,
                ),
              );
            }}
          >
            <option value="">选择渠道</option>
            {channelRows.map((row) => (
              <option key={row.id} value={row.id}>
                {row.name}
              </option>
            ))}
          </select>
          <select
            className="select"
            value={keyIndex}
            onChange={(event) => setKeyIndex(event.target.value)}
          >
            <option value="">自动选择 Key</option>
            {keys.map((key, index) => (
              <option
                key={key.key_index ?? index}
                value={key.key_index ?? index}
                disabled={key.disabled}
              >
                {key.note || `Key #${(key.key_index ?? index) + 1}`}{" "}
                {key.disabled ? "（停用）" : ""}
              </option>
            ))}
          </select>
          <input
            className="input"
            list="model-options"
            value={model}
            onChange={(event) => setModel(event.target.value)}
            placeholder="模型名称"
          />
          <datalist id="model-options">
            {allModels.map((value) => (
              <option key={value} value={value} />
            ))}
          </datalist>
          <select
            className="select"
            value={protocol}
            onChange={(event) => setProtocol(event.target.value)}
          >
            <option value="openai">OpenAI</option>
            <option value="anthropic">Anthropic</option>
            <option value="gemini">Gemini</option>
            <option value="codex">Codex</option>
          </select>
          <label>
            <input
              type="checkbox"
              checked={stream}
              onChange={(event) => setStream(event.target.checked)}
            />{" "}
            流式
          </label>
          <button
            className="btn"
            disabled={!channelId}
            onClick={() => void loadEntries()}
          >
            加载模型
          </button>
          <button
            className="btn btn-primary"
            disabled={busy || !channelId || !model}
            onClick={() => void run()}
          >
            连通性测试
          </button>
          <button
            className="btn"
            disabled={busy || !channelId || !model}
            onClick={() => void send()}
            style={{ display: testMode === "chat" ? "none" : undefined }}
          >
            聊天请求
          </button>
        </div>
        <div className="toolbar" style={{ display: testMode === "image" ? undefined : "none" }}>
          <input
            className="input compact"
            type="number"
            value={priority}
            onChange={(event) => setPriority(event.target.value)}
            placeholder="优先级"
          />
          <button
            className="btn"
            disabled={!channelId}
            onClick={() => void savePriority()}
          >
            保存渠道优先级
          </button>
          <button
            className="btn"
            onClick={() => setShowAdvanced((value) => !value)}
          >
            高级聊天参数
          </button>
        </div>
        {showAdvanced && (
          <div className="advanced-grid">
            <label>
              System Prompt
              <textarea
                className="input wide advanced-textarea"
                value={advanced.systemPrompt}
                onChange={(event) =>
                  setAdvanced({ ...advanced, systemPrompt: event.target.value })
                }
              />
            </label>
            <label>
              Temperature
              <input
                className="input"
                type="number"
                min="0"
                max="2"
                step="0.1"
                value={advanced.temperature}
                onChange={(event) =>
                  setAdvanced({ ...advanced, temperature: event.target.value })
                }
              />
            </label>
            <label>
              Top P
              <input
                className="input"
                type="number"
                min="0"
                max="1"
                step="0.05"
                value={advanced.topP}
                onChange={(event) =>
                  setAdvanced({ ...advanced, topP: event.target.value })
                }
              />
            </label>
            <label>
              Max Tokens
              <input
                className="input"
                type="number"
                min="1"
                value={advanced.maxTokens}
                onChange={(event) =>
                  setAdvanced({ ...advanced, maxTokens: event.target.value })
                }
              />
            </label>
            <label>
              上下文消息数
              <input
                className="input"
                type="number"
                min="0"
                value={advanced.contextMessages}
                onChange={(event) =>
                  setAdvanced({
                    ...advanced,
                    contextMessages: event.target.value,
                  })
                }
              />
            </label>
          </div>
        )}
        <textarea
          className="input wide test-content"
          value={content}
          onChange={(event) => setContent(event.target.value)}
        />
        <div className="toolbar">
          <label className="btn">
            添加聊天图片
            <input
              hidden
              type="file"
              accept="image/*"
              multiple
              onChange={(event) => {
                const files = Array.from(event.target.files ?? []).filter(
                  (file) => file.size <= 10 * 1024 * 1024,
                );
                void readPendingImages(files)
                  .then((items) =>
                    setPendingImages((current) => [...current, ...items]),
                  )
                  .catch(() => setResult({ error: "图片读取失败" }));
                event.target.value = "";
              }}
            />
          </label>
          {pendingImages.map((item) => (
            <span className="muted" key={item.id}>
              {item.name}
              <button
                className="btn"
                type="button"
                aria-label="移除图片"
                onClick={() =>
                  setPendingImages((current) =>
                    current.filter((imageItem) => imageItem.id !== item.id),
                  )
                }
              >
                ×
              </button>
            </span>
          ))}
        </div>
        <div className="toolbar">
          <select
            className="select"
            value={image.generation_api}
            onChange={(event) =>
              setImage({ ...image, generation_api: event.target.value })
            }
          >
            <option value="images">Images API</option>
            <option value="chat_completions">Chat Completions</option>
          </select>
          <select
            className="select"
            value={image.size}
            onChange={(event) =>
              setImage({ ...image, size: event.target.value })
            }
          >
            <option value="auto">自动尺寸</option>
            <option value="1024x1024">1024x1024</option>
            <option value="1536x1024">1536x1024</option>
            <option value="1024x1536">1024x1536</option>
            <option value="16:9@1k">16:9 · 1K</option>
          </select>
          <select
            className="select"
            value={image.quality}
            onChange={(event) =>
              setImage({ ...image, quality: event.target.value })
            }
          >
            <option value="auto">自动质量</option>
            <option value="low">低</option>
            <option value="medium">中</option>
            <option value="high">高</option>
            <option value="hd">高清</option>
          </select>
          <select
            className="select"
            value={image.background}
            onChange={(event) =>
              setImage({ ...image, background: event.target.value })
            }
          >
            <option value="auto">自动背景</option>
            <option value="opaque">不透明</option>
            <option value="transparent">透明</option>
          </select>
          <select
            className="select"
            value={image.output_format}
            onChange={(event) =>
              setImage({ ...image, output_format: event.target.value })
            }
          >
            <option value="auto">自动格式</option>
            <option value="png">PNG</option>
            <option value="jpeg">JPEG</option>
            <option value="webp">WebP</option>
          </select>
          <button
            className="btn"
            disabled={!channelId || !model}
            onClick={() => void imageGenerate()}
          >
            图片生成
          </button>
        </div>
        {generatedImages.length > 0 && (
          <div className="image-results">
            {generatedImages.map((item, index) => (
              <figure key={`${item.src.slice(0, 48)}-${index}`}>
                <img src={item.src} alt={item.label} />
                <figcaption>{item.label}</figcaption>
              </figure>
            ))}
          </div>
        )}
        {result !== null && (
          <pre className="result-pre">{JSON.stringify(result, null, 2)}</pre>
        )}
      </div>
      <div className="card" style={{ display: testMode === "chat" ? undefined : "none" }}>
        <div className="toolbar">
          <h2>聊天记录</h2>
          <label className="muted">
            <input
              type="checkbox"
              checked={chatStream}
              onChange={(event) => setChatStream(event.target.checked)}
            />{" "}
            流式
          </label>
          <select
            className="select"
            value={thinkingEffort}
            onChange={(event) => setThinkingEffort(event.target.value)}
            aria-label="思考等级"
          >
            <option value="">默认思考</option>
            <option value="none">关闭思考</option>
            <option value="minimal">最少</option>
            <option value="low">低</option>
            <option value="medium">中</option>
            <option value="high">高</option>
            <option value="xhigh">超高</option>
            <option value="max">最高</option>
          </select>
          <label className="muted">
            <input
              type="checkbox"
              checked={builtinSearch}
              onChange={(event) => setBuiltinSearch(event.target.checked)}
            />{" "}
            内置搜索
          </label>
          <button
            className="btn"
            onClick={() => {
              setChat([]);
              setSessionId(crypto.randomUUID());
            }}
          >
            清空
          </button>
          <button className="btn" onClick={() => exportChat("md")}>
            导出 Markdown
          </button>
          <button className="btn" onClick={() => exportChat("html")}>
            导出 HTML
          </button>
        </div>
        <textarea
          className="input wide"
          value={chatInput}
          onChange={(event) => setChatInput(event.target.value)}
          onPaste={(event) => {
            const files = Array.from(event.clipboardData.files).filter(
              (file) =>
                file.type.startsWith("image/") && file.size <= 10 * 1024 * 1024,
            );
            if (!files.length) return;
            event.preventDefault();
            void readPendingImages(files)
              .then((items) =>
                setPendingImages((current) => [...current, ...items]),
              )
              .catch(() => setResult({ error: "图片读取失败" }));
          }}
          onKeyDown={(event) => {
            if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
              event.preventDefault();
              void send();
            }
          }}
          placeholder="输入聊天消息"
        />
        <button
          className="btn btn-primary"
          disabled={
            busy ||
            !channelId ||
            !model ||
            (!chatInput.trim() && pendingImages.length === 0)
          }
          onClick={() => void send()}
        >
          发送
        </button>
        <div className="chat-transcript">
          {chat.map((item, index) => (
            <div className={`chat-message chat-${item.role}`} key={index}>
              <strong>{item.role}</strong>
              <span>
                {item.thinking && (
                  <details className="chat-thinking">
                    <summary>思考过程</summary>
                    <pre>{item.thinking}</pre>
                  </details>
                )}
                {typeof item.content === "string"
                  ? item.content
                  : item.content.map((part, partIndex) =>
                      part.type === "text" ? (
                        <span key={partIndex}>{part.text}</span>
                      ) : (
                        <img
                          key={partIndex}
                          className="chat-image-preview-thumb"
                          src={part.image_url.url}
                          alt="聊天图片"
                        />
                      ),
                    )}
              </span>
            </div>
          ))}
        </div>
      </div>
      <div className="card" style={{ display: testMode === "model" ? undefined : "none" }}>
        <h2>模型目录编辑</h2>
        <div className="toolbar">
          <input className="input" value={modelModeFilter} onChange={(event) => setModelModeFilter(event.target.value)} placeholder="筛选渠道或模型" />
          <label className="muted"><input type="checkbox" checked={modelModeRows.length > 0 && modelModeSelected.length === modelModeRows.length} onChange={(event) => setModelModeSelected(event.target.checked ? modelModeRows.map((row) => row.key) : [])} /> 全选结果</label>
        </div>
        <div className="table-wrap">
          <table><thead><tr><th>选择</th><th>渠道</th><th>请求模型</th><th>重定向</th><th>状态</th></tr></thead><tbody>{modelModeRows.map((row) => <tr key={row.key}><td><input type="checkbox" checked={modelModeSelected.includes(row.key)} onChange={(event) => setModelModeSelected((current) => event.target.checked ? [...new Set([...current, row.key])] : current.filter((value) => value !== row.key))} /></td><td>{row.channel}</td><td>{row.model}</td><td>{row.redirect_model || '-'}</td><td>{row.disabled ? '停用' : '启用'}</td></tr>)}{modelModeRows.length === 0 && <tr><td colSpan={5}>没有匹配的渠道模型</td></tr>}</tbody></table>
        </div>
        <div className="toolbar"><button className="btn btn-primary" disabled={busy || !modelModeSelected.length} onClick={() => void runBatch()}>批量测试选中渠道模型</button><span className="muted">已选择 {modelModeSelected.length} / {modelModeRows.length}</span></div>
        <div className="toolbar">
          <button
            className="btn"
            disabled={!channelId}
            onClick={() => void loadEntries()}
          >
            刷新模型目录
          </button>
          <button
            className="btn"
            disabled={!channelId || busy}
            onClick={() => void fetchAndAddModels()}
          >
            获取并添加上游模型
          </button>
          <button
            className="btn"
            disabled={!selected.length}
            onClick={() => void deleteSelectedEntries()}
          >
            批量删除模型
          </button>
        </div>
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>选择</th>
                <th>模型</th>
                <th>重定向</th>
                <th>启用</th>
                <th>操作</th>
              </tr>
            </thead>
            <tbody>
              {entries.map((entry) => (
                <tr key={entry.model}>
                  <td>
                    <input
                      type="checkbox"
                      checked={selected.includes(entry.model)}
                      onChange={(event) =>
                        setSelected((current) =>
                          event.target.checked
                            ? [...current, entry.model]
                            : current.filter((value) => value !== entry.model),
                        )
                      }
                    />
                  </td>
                  <td>{entry.model}</td>
                  <td>
                    <input
                      className="input"
                      value={entry.redirect_model ?? ""}
                      onChange={(event) =>
                        setEntries((current) =>
                          current.map((item) =>
                            item.model === entry.model
                              ? { ...item, redirect_model: event.target.value }
                              : item,
                          ),
                        )
                      }
                    />
                  </td>
                  <td>
                    <input
                      type="checkbox"
                      checked={!entry.disabled}
                      onChange={(event) =>
                        setEntries((current) =>
                          current.map((item) =>
                            item.model === entry.model
                              ? { ...item, disabled: !event.target.checked }
                              : item,
                          ),
                        )
                      }
                    />
                  </td>
                  <td>
                    <button
                      className="btn"
                      onClick={() => void saveEntry(entry)}
                    >
                      保存
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
        <button
          className="btn btn-primary"
          disabled={busy || !selected.length}
          onClick={() => void runBatch()}
        >
          批量测试选中模型
        </button>
        <label>
          并发数
          <input
            className="input compact"
            type="number"
            min={1}
            max={20}
            value={concurrency}
            disabled={busy}
            onChange={(event) =>
              setConcurrency(
                Math.min(20, Math.max(1, Number(event.target.value) || 1)),
              )
            }
          />
        </label>
        {batchProgress.total > 0 && (
          <span className="muted" role="status">
            {busy
              ? `已完成 ${batchProgress.done} / ${batchProgress.total}`
              : `完成 ${batchProgress.done} 项`}
          </span>
        )}
        {batchStatus && <span className="muted" role="status">{batchStatus}</span>}
        <div className="toolbar">
          <input
            className="input wide"
            value={manualModels}
            onChange={(event) => setManualModels(event.target.value)}
            placeholder="手动添加模型名称，逗号或换行分隔"
          />
          <button
            className="btn"
            disabled={!channelId || !manualModels.trim()}
            onClick={() => void addManualModels()}
          >
            添加模型
          </button>
        </div>
      </div>
      {batch.length > 0 && (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>渠道</th>
                <th>模型</th>
                <th>结果</th>
                <th>错误</th>
              </tr>
            </thead>
            <tbody>
              {batch.map((row, index) => (
                <tr key={index}>
                  <td>{String(row.channelId)}</td>
                  <td>{String(row.model)}</td>
                  <td>{row.success ? "成功" : "失败"}</td>
                  <td>{String(row.error ?? "")}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
