import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { CircleCheck, CircleAlert, ExternalLink, RefreshCw } from "lucide-react";
import { get, post, put } from "../../lib/api";
import { toast } from "../../lib/store";
import { Alert, Button, Field, Spinner } from "../../components/ui";
import { useAIStatus, type AIProvider } from "../ai/store";

type PresetID = "anthropic" | "openai" | "openrouter" | "custom";

interface Preset {
  id: PresetID;
  label: string;
  note: string;
  provider: AIProvider;
  base: string;
  keyUrl?: string;
  keyHint?: string;
}

const PRESETS: Preset[] = [
  { id: "anthropic", label: "Anthropic", note: "Claude models, with reasoning and cached schemas", provider: "anthropic", base: "", keyUrl: "https://console.anthropic.com/settings/keys", keyHint: "sk-ant-…" },
  { id: "openai", label: "OpenAI", note: "GPT and o-series models", provider: "openai", base: "https://api.openai.com/v1", keyUrl: "https://platform.openai.com/api-keys", keyHint: "sk-…" },
  { id: "openrouter", label: "OpenRouter", note: "Hundreds of models behind one key", provider: "openai", base: "https://openrouter.ai/api/v1", keyUrl: "https://openrouter.ai/keys", keyHint: "sk-or-…" },
  { id: "custom", label: "Custom endpoint", note: "Ollama, LM Studio, vLLM, LiteLLM or any OpenAI-compatible server", provider: "openai", base: "" },
];

/** Local servers, reached from inside the Rowsmith container. */
const LOCAL = [
  { label: "Ollama", url: "http://host.docker.internal:11434/v1" },
  { label: "LM Studio", url: "http://host.docker.internal:1234/v1" },
  { label: "vLLM", url: "http://host.docker.internal:8000/v1" },
];

const trim = (u: string) => u.trim().replace(/\/+$/, "");

function presetFor(provider: string | undefined, base: string | undefined): PresetID {
  if (provider !== "openai") return "anthropic";
  const b = trim(base ?? "");
  return PRESETS.find((p) => p.id !== "custom" && p.base && trim(p.base) === b)?.id ?? "custom";
}

interface Draft {
  preset: PresetID;
  base: string;
  key: string;
  model: string;
  effort: string;
}

type Check = { state: "idle" } | { state: "running" } | { state: "ok"; text: string } | { state: "fail"; text: string };

export function AISettings() {
  const qc = useQueryClient();
  const settings = useQuery({ queryKey: ["settings"], queryFn: () => get<Record<string, unknown>>("settings") });
  const status = useAIStatus();
  const saved = useMemo(() => {
    const s = settings.data ?? {};
    const str = (k: string) => (typeof s[k] === "string" ? (s[k] as string) : "");
    const provider = str("ai.provider") === "openai" ? "openai" : "anthropic";
    return {
      provider: provider as AIProvider,
      base: provider === "openai" ? str("ai.base_url") : "",
      keySet: (s["ai.api_key"] as { set?: boolean } | undefined)?.set === true,
      model: str("ai.model"),
      effort: str("ai.effort") || "medium",
      enabled: str("ai.enabled") === "true",
      data: str("ai.data") === "data",
      production: str("ai.production") === "true",
    };
  }, [settings.data]);

  const [draft, setDraft] = useState<Draft | null>(null);
  useEffect(() => {
    if (settings.data && !draft) setDraft({ preset: presetFor(saved.provider, saved.base), base: saved.base, key: "", model: saved.model, effort: saved.effort });
  }, [settings.data, saved, draft]);

  const [models, setModels] = useState<{ loading: boolean; list: string[]; error?: string; for?: string }>({ loading: false, list: [] });
  const [check, setCheck] = useState<Check>({ state: "idle" });
  const [saving, setSaving] = useState(false);

  const preset = PRESETS.find((p) => p.id === draft?.preset) ?? PRESETS[0];
  const provider = preset.provider;
  const base = provider === "openai" ? trim(draft?.base ?? "") : "";
  const sameEndpoint = provider === saved.provider && base === trim(saved.base);
  const keySaved = saved.keySet && sameEndpoint;
  const envKey = provider === "anthropic" && !keySaved && status.data?.envKey;
  const hasKey = !!draft?.key || keySaved || !!envKey;
  const endpointOK = provider === "anthropic" || /^https?:\/\/[^/\s]+/i.test(base);
  const anthropicModels = status.data?.anthropicModels ?? [];
  const model = provider === "anthropic" ? (anthropicModels.some((m) => m.ID === draft?.model) ? draft!.model : anthropicModels[0]?.ID ?? "") : draft?.model.trim() ?? "";

  const body = () => ({ provider, baseUrl: base, apiKey: draft?.key ?? "", model });

  // Load the endpoint's model list while the form is filled in.
  const seq = useRef(0);
  const loadModels = async (quiet = false) => {
    if (provider !== "openai" || !endpointOK) return;
    const n = ++seq.current;
    setModels((m) => ({ ...m, loading: true, error: undefined }));
    try {
      const r = await post<{ models: string[] }>("ai/models", body());
      if (n === seq.current) setModels({ loading: false, list: r.models, for: base });
    } catch (e) {
      if (n !== seq.current) return;
      setModels({ loading: false, list: [], error: e instanceof Error ? e.message : String(e), for: base });
      if (!quiet) toast.error("Could not load the model list", e instanceof Error ? e.message : undefined);
    }
  };
  useEffect(() => {
    if (!draft || provider !== "openai" || !endpointOK) return;
    if (preset.id === "openai" && !hasKey) return; // OpenAI lists models only with a key
    const t = setTimeout(() => loadModels(true), 500);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [draft?.preset, base, draft?.key, keySaved]);

  if (settings.isLoading || !draft) return <Spinner large />;

  const set = (p: Partial<Draft>) => {
    setDraft((d) => ({ ...d!, ...p }));
    setCheck({ state: "idle" });
  };
  const choose = (id: PresetID) => {
    if (id === draft.preset) return;
    const p = PRESETS.find((x) => x.id === id)!;
    const back = p.provider === saved.provider && (p.id === "custom" ? presetFor(saved.provider, saved.base) === "custom" : trim(p.base) === trim(saved.base));
    set({ preset: id, base: back ? saved.base : p.base, key: "", model: back ? saved.model : "" });
    setModels({ loading: false, list: [] });
  };

  const problem =
    !endpointOK ? "Enter the endpoint as a full URL, such as http://host.docker.internal:11434/v1." :
    provider === "anthropic" && !hasKey ? "Enter an Anthropic API key." :
    preset.id !== "custom" && !hasKey ? `Enter your ${preset.label} API key.` :
    !model ? "Choose a model." : "";

  const dirty = draft.key !== "" || provider !== saved.provider || base !== trim(saved.base) || model !== (provider === "anthropic" ? saved.model || anthropicModels[0]?.ID : saved.model) || (provider === "anthropic" && draft.effort !== saved.effort);

  const save = async () => {
    setSaving(true);
    try {
      await put("settings", {
        "ai.provider": provider,
        "ai.base_url": provider === "openai" ? base : null,
        ...(draft.key ? { "ai.api_key": draft.key } : {}),
        "ai.model": model || null,
        ...(provider === "anthropic" ? { "ai.effort": draft.effort } : {}),
      });
      await Promise.all([qc.invalidateQueries({ queryKey: ["settings"] }), qc.invalidateQueries({ queryKey: ["ai-status"] })]);
      setDraft((d) => ({ ...d!, key: "" }));
      toast.success("Assistant settings saved", saved.enabled ? "New conversations use them." : "Turn the assistant on to let people use it.");
    } catch (e) {
      toast.error("Could not save", e instanceof Error ? e.message : undefined);
    } finally {
      setSaving(false);
    }
  };

  const test = async () => {
    setCheck({ state: "running" });
    try {
      const r = await post<{ model: string; reply: string }>("ai/check", body());
      setCheck({ state: "ok", text: r.reply ? `${r.model} replied “${r.reply}”.` : `${r.model} answered.` });
    } catch (e) {
      setCheck({ state: "fail", text: e instanceof Error ? e.message : String(e) });
    }
  };

  const removeKey = async () => {
    await put("settings", { "ai.api_key": null });
    await Promise.all([qc.invalidateQueries({ queryKey: ["settings"] }), qc.invalidateQueries({ queryKey: ["ai-status"] })]);
    toast.success("API key removed");
  };

  // Switches save at once, and show the new state before the server answers.
  const toggle = async (values: Record<string, string | null>, done: string) => {
    const before = qc.getQueryData<Record<string, unknown>>(["settings"]);
    qc.setQueryData<Record<string, unknown>>(["settings"], (d) => {
      const next = { ...d };
      for (const [k, v] of Object.entries(values)) {
        if (v === null) delete next[k];
        else next[k] = v;
      }
      return next;
    });
    try {
      await put("settings", values);
      toast.success(done);
    } catch (e) {
      qc.setQueryData(["settings"], before);
      toast.error("Could not save", e instanceof Error ? e.message : undefined);
    }
    await Promise.all([qc.invalidateQueries({ queryKey: ["settings"] }), qc.invalidateQueries({ queryKey: ["ai-status"] })]);
  };

  const keyPlaceholder = keySaved ? "Saved; type a new key to replace it" : envKey ? "Using ANTHROPIC_API_KEY from the server" : preset.id === "custom" ? "Only if the server asks for one" : preset.keyHint;
  const listFresh = models.for === base ? models.list : [];

  return (
    <div className="pgrid aiset">
      <section className="card pcard aiset__main">
        <h2 className="pcard__title">Provider and model</h2>
        <p className="pcard__desc">Bring your own account. Requests go straight from this server to the provider, using your key; people using the assistant never see it.</p>

        <div className="aiprov" role="radiogroup" aria-label="Provider">
          {PRESETS.map((p) => (
            <button key={p.id} role="radio" aria-checked={p.id === draft.preset} className="aiprov__opt" onClick={() => choose(p.id)}>
              <span className="aiprov__label">
                {p.label}
                {status.data?.ready && presetFor(saved.provider, saved.base) === p.id && <span className="aiprov__saved">{saved.enabled ? "in use" : "saved"}</span>}
              </span>
              <span className="aiprov__note">{p.note}</span>
            </button>
          ))}
        </div>

        <div className="aiset__fields">
          {preset.id === "custom" && (
            <Field label="Endpoint" required htmlFor="ai-base" help={<>The base URL of the OpenAI-compatible API, ending before <code className="mono">/chat/completions</code>. Servers on this machine are at host.docker.internal, and must listen beyond 127.0.0.1 (for Ollama, set <code className="mono">OLLAMA_HOST=0.0.0.0</code>).</>}>
              <input id="ai-base" className="input input--mono" value={draft.base} onChange={(e) => set({ base: e.target.value })} placeholder="http://host.docker.internal:11434/v1" spellCheck={false} autoComplete="off" aria-invalid={!!draft.base && !endpointOK} />
              <div className="aiset__quick">
                {LOCAL.map((l) => (
                  <button key={l.label} className="aiset__chip" onClick={() => set({ base: l.url })}>{l.label}</button>
                ))}
              </div>
            </Field>
          )}
          {preset.id !== "custom" && provider === "openai" && (
            <div className="kv"><span>Endpoint</span><code className="mono">{preset.base}</code></div>
          )}

          <Field label="API key" required={preset.id !== "custom" && !envKey} htmlFor="ai-key"
            help={<span className="aiset__keyhelp">
              {preset.keyUrl && <a href={preset.keyUrl} target="_blank" rel="noreferrer noopener">Create a key <ExternalLink size={12} /></a>}
              <span>Stored encrypted with the master key and never shown again.</span>
              {keySaved && <button className="aiset__link" onClick={removeKey}>Remove the saved key</button>}
            </span>}>
            <input id="ai-key" className="input input--mono" type="password" value={draft.key} onChange={(e) => set({ key: e.target.value.trim() })} placeholder={keyPlaceholder} autoComplete="off" spellCheck={false} />
          </Field>

          {provider === "anthropic" ? (
            <>
              <Field label="Model" htmlFor="ai-model">
                <select id="ai-model" className="select" value={model} onChange={(e) => set({ model: e.target.value })}>
                  {anthropicModels.map((m) => <option key={m.ID} value={m.ID}>{m.Name}</option>)}
                </select>
              </Field>
              {!model.startsWith("claude-haiku") && (
                <Field label="Reasoning effort" help="Higher effort thinks longer on hard questions; it costs more and answers more slowly." group>
                  <div className="segmented">
                    {["low", "medium", "high"].map((e) => (
                      <button key={e} aria-pressed={draft.effort === e} onClick={() => set({ effort: e })}>{e[0].toUpperCase() + e.slice(1)}</button>
                    ))}
                  </div>
                </Field>
              )}
            </>
          ) : (
            <Field label="Model" required htmlFor="ai-model"
              error={models.error && models.for === base ? `Could not list models: ${models.error}` : undefined}
              help={listFresh.length ? `${listFresh.length} model${listFresh.length === 1 ? "" : "s"} available. Type to search.` : "Type the model ID, or load the list from the endpoint. Pick one that can call tools for the best answers."}>
              <div className="row gap-3">
                <input id="ai-model" className="input input--mono" value={draft.model} onChange={(e) => set({ model: e.target.value })} list="ai-models" placeholder={preset.id === "custom" ? "e.g. qwen3-coder" : "Choose from the list"} spellCheck={false} autoComplete="off" />
                <Button onClick={() => loadModels(false)} loading={models.loading} disabled={!endpointOK} aria-label="Load the model list"><RefreshCw size={14} /> Load list</Button>
              </div>
              <datalist id="ai-models">{listFresh.map((m) => <option key={m} value={m} />)}</datalist>
            </Field>
          )}
        </div>

        {check.state === "ok" && <Alert kind="success" title="Connected">{check.text}</Alert>}
        {check.state === "fail" && <Alert kind="danger" title="The test failed">{check.text}</Alert>}

        <div className="aiset__actions">
          <span className="faint aiset__problem">{problem}</span>
          <Button onClick={test} loading={check.state === "running"} disabled={!!problem}>Test connection</Button>
          <Button variant="primary" onClick={save} loading={saving} disabled={!!problem || !dirty}>Save</Button>
        </div>
      </section>

      <section className="card pcard">
        <h2 className="pcard__title">Access</h2>
        <p className="pcard__desc">The assistant appears in every query tab. It can look things up, but it never changes data: people run what it writes themselves.</p>
        <div className="aiset__state">
          {status.data?.ready ? <CircleCheck className="ok" /> : <CircleAlert className="warn" />}
          <span>{status.data?.ready ? <>Saved: <b>{status.data.modelName}</b> via {status.data.providerName}</> : "Save a provider and model first."}</span>
        </div>
        <label className="switch">
          <input type="checkbox" checked={saved.enabled} disabled={!status.data?.ready && !saved.enabled} onChange={(e) => toggle({ "ai.enabled": e.target.checked ? "true" : null }, e.target.checked ? "The assistant is on" : "The assistant is off")} />
          Turn on the assistant for everyone
        </label>

        <h3 className="aiset__h3">What it may see</h3>
        <div className="segmented" role="group" aria-label="What the assistant may see">
          <button aria-pressed={!saved.data} onClick={() => saved.data && toggle({ "ai.data": "schema" }, "The assistant sees the schema only")}>Schema only</button>
          <button aria-pressed={saved.data} onClick={() => !saved.data && toggle({ "ai.data": "data" }, "The assistant may read sample rows")}>Schema and sample rows</button>
        </div>
        <p className="aiset__explain muted">
          {saved.data
            ? "It may read a few rows and run small read-only queries to check answers. What it reads is sent to the provider."
            : "Names, types, keys and comments are sent to the provider, never the data in the tables."}
        </p>
        {saved.data && (
          <label className="switch">
            <input type="checkbox" checked={saved.production} onChange={(e) => toggle({ "ai.production": e.target.checked ? "true" : null }, e.target.checked ? "Rows may be read on production too" : "Production connections share the schema only")} />
            Read rows on production connections too
          </label>
        )}
        <ul className="plist">
          <li>Each person's access to connections still applies; the assistant uses a read-only session.</li>
          <li>The audit log records each question's model and token counts, not its text.</li>
          <li>Questions are limited per person to keep costs predictable.</li>
        </ul>
      </section>
    </div>
  );
}
