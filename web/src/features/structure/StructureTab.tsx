import { useQuery } from "@tanstack/react-query";
import { KeyRound, Link2, Table2, TerminalSquare, Copy, CornerDownRight, CornerUpLeft, Zap, ShieldCheck, Hash, Sparkles, FileCode2 } from "lucide-react";
import { get, qs } from "../../lib/api";
import { useDescribe, useDriver, qualified } from "../../lib/queries";
import type { Tab } from "../../lib/store";
import { toast } from "../../lib/store";
import type { Connection, ForeignKey } from "../../lib/types";
import { bytes, int } from "../../lib/format";
import { Alert, Button, Spinner, Tip } from "../../components/ui";
import { SqlEditor } from "../query/SqlEditor";
import { newQueryTab, openObject } from "../workspace/actions";
import "./structure.css";

export function StructureTab({ tab, conn }: { tab: Tab; conn: Connection }) {
  const ref = tab.ref!;
  const drv = useDriver(conn.driver);
  const d = useDescribe(conn.id, ref);
  const q = drv?.quoteChar ?? '"';
  if (d.isLoading) return <div className="structure__center"><Spinner large /></div>;
  if (d.error) return <div className="structure__pad"><Alert kind="danger" title="Could not describe">{(d.error as Error).message}</Alert></div>;
  const t = d.data!;
  const fkByCol = new Map<string, ForeignKey>();
  for (const fk of t.foreignKeys ?? []) for (const c of fk.columns) fkByCol.set(c, fk);
  const opts = Object.entries(t.options ?? {});

  const openRef = (fk: ForeignKey, incoming: boolean) => {
    const target = incoming ? fk.table! : fk.refTable;
    openObject(conn, { database: target.database ?? ref.database, schema: target.schema ?? ref.schema, name: target.name, kind: "table" }, "structure");
  };

  return (
    <div className="structure">
      <header className="structure__head">
        <div className="grow" style={{ minWidth: 0 }}>
          <div className="eyebrow">{t.kind.replace("_", " ")}{ref.schema ? ` · ${ref.schema}` : ref.database ? ` · ${ref.database}` : ""}</div>
          <h1 className="structure__title display truncate">{ref.name}</h1>
          <div className="structure__facts">
            {t.rowEstimate !== undefined && <span><b className="tnum">{int(t.rowEstimate)}</b> rows (est.)</span>}
            {t.size !== undefined && <span><b>{bytes(t.size)}</b> on disk</span>}
            <span><b className="tnum">{t.columns.length}</b> columns</span>
            {opts.map(([k, v]) => <span key={k} className="mono">{k.replace("_", " ")}: {v}</span>)}
          </div>
          {t.comment && <p className="structure__comment">{t.comment}</p>}
        </div>
        <div className="row gap-3">
          <Button onClick={() => openObject(conn, ref, "browse")}><Table2 /> Data</Button>
          <Button onClick={() => newQueryTab(conn, { sql: `SELECT ${t.columns.slice(0, 12).map((c) => c.name).join(", ")}\nFROM ${qualified(ref, q)}\n${drv?.dialect === "mssql" ? "" : "LIMIT 100"}`, database: ref.database, schema: ref.schema })}>
            <TerminalSquare /> Query
          </Button>
        </div>
      </header>

      <section className="ssection">
        <h2 className="ssection__title">Columns</h2>
        <div className="stable-wrap">
          <table className="table stable">
            <thead>
              <tr><th className="num">#</th><th>Name</th><th>Type</th><th>Null</th><th>Default</th><th>Extra</th><th>Comment</th></tr>
            </thead>
            <tbody>
              {t.columns.map((c, i) => {
                const fk = fkByCol.get(c.name);
                return (
                  <tr key={c.name}>
                    <td className="num faint tnum">{i + 1}</td>
                    <td className="stable__name">
                      {c.primaryKey && <Tip label="Primary key"><KeyRound className="sicon sicon--pk" /></Tip>}
                      {fk && <Tip label={`References ${fk.refTable.name}.${fk.refColumns.join(", ")}`}><Link2 className="sicon sicon--fk" /></Tip>}
                      <span>{c.name}</span>
                    </td>
                    <td className="mono stable__type">{c.type}{c.enum?.length && !c.type.includes("(") ? <span className="faint"> ({c.enum.join(", ")})</span> : null}</td>
                    <td>{c.nullable ? <span className="faint">yes</span> : <span className="stable__notnull">not null</span>}</td>
                    <td className="mono stable__default">{c.default ?? ""}</td>
                    <td className="stable__extra">
                      {c.autoIncrement && <span className="badge"><Hash /> auto</span>}
                      {c.generated && <Tip label={c.generated}><span className="badge badge--info"><Sparkles /> generated</span></Tip>}
                      {c.onUpdate && <span className="badge">on update {c.onUpdate}</span>}
                      {c.srid ? <span className="badge">SRID {c.srid}</span> : null}
                      {c.collation && <span className="faint mono">{c.collation}</span>}
                    </td>
                    <td className="stable__comment">{c.comment}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      </section>

      {(t.indexes?.length ?? 0) > 0 && (
        <section className="ssection">
          <h2 className="ssection__title">Indexes <span className="faint">{t.indexes!.length}</span></h2>
          <div className="stable-wrap">
            <table className="table stable">
              <thead><tr><th>Name</th><th>Columns</th><th>Kind</th><th>Method</th><th>Condition</th></tr></thead>
              <tbody>
                {t.indexes!.map((ix) => (
                  <tr key={ix.name}>
                    <td className="stable__name mono">{ix.name}</td>
                    <td className="mono">{ix.columns.map((c, i) => `${c}${ix.lengths?.[i] ? `(${ix.lengths[i]})` : ""}${ix.desc?.[i] ? " desc" : ""}`).join(", ")}</td>
                    <td>{ix.primary ? <span className="badge badge--accent">primary</span> : ix.unique ? <span className="badge">unique</span> : <span className="faint">index</span>}</td>
                    <td className="faint mono">{ix.type}</td>
                    <td className="mono faint">{ix.where}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </section>
      )}

      {((t.foreignKeys?.length ?? 0) > 0 || (t.referenced?.length ?? 0) > 0) && (
        <section className="ssection">
          <h2 className="ssection__title">Relationships</h2>
          <div className="relations">
            {(t.foreignKeys ?? []).map((fk) => (
              <button key={"o" + fk.name} className="relation" onClick={() => openRef(fk, false)}>
                <CornerDownRight className="relation__icon relation__icon--out" />
                <span className="mono">{fk.columns.join(", ")}</span>
                <span className="relation__arrow">→</span>
                <span className="mono relation__target">{fk.refTable.schema && fk.refTable.schema !== ref.schema ? fk.refTable.schema + "." : ""}{fk.refTable.name}({fk.refColumns.join(", ")})</span>
                <span className="spacer" />
                <span className="faint relation__rules">{[fk.onDelete && fk.onDelete !== "NO ACTION" && `on delete ${fk.onDelete.toLowerCase()}`, fk.onUpdate && fk.onUpdate !== "NO ACTION" && `on update ${fk.onUpdate.toLowerCase()}`].filter(Boolean).join(" · ")}</span>
              </button>
            ))}
            {(t.referenced ?? []).map((fk) => (
              <button key={"i" + fk.name + fk.table?.name} className="relation" onClick={() => openRef(fk, true)}>
                <CornerUpLeft className="relation__icon relation__icon--in" />
                <span className="mono relation__target">{fk.table?.name}({fk.columns.join(", ")})</span>
                <span className="relation__arrow">→</span>
                <span className="mono">{fk.refColumns.join(", ")}</span>
                <span className="spacer" />
                <span className="faint relation__rules">referenced by</span>
              </button>
            ))}
          </div>
        </section>
      )}

      {((t.checks?.length ?? 0) > 0 || (t.triggers?.length ?? 0) > 0) && (
        <section className="ssection ssection--two">
          {(t.checks?.length ?? 0) > 0 && (
            <div>
              <h2 className="ssection__title">Checks</h2>
              {t.checks!.map((c) => (
                <div key={c.name} className="rule"><ShieldCheck /><span className="mono">{c.name}</span><code>{c.expression}</code></div>
              ))}
            </div>
          )}
          {(t.triggers?.length ?? 0) > 0 && (
            <div>
              <h2 className="ssection__title">Triggers</h2>
              {t.triggers!.map((tr) => (
                <div key={tr.name} className="rule"><Zap /><span className="mono">{tr.name}</span><span className="faint">{tr.timing} {tr.event}</span></div>
              ))}
            </div>
          )}
        </section>
      )}

      {t.ddl && (
        <section className="ssection">
          <div className="row gap-3">
            <h2 className="ssection__title">Definition</h2>
            <span className="spacer" />
            <Button size="sm" variant="ghost" onClick={() => { navigator.clipboard?.writeText(t.ddl!); toast.success("DDL copied"); }}><Copy /> Copy</Button>
            <Button size="sm" variant="ghost" onClick={() => newQueryTab(conn, { sql: t.ddl!, database: ref.database, schema: ref.schema, title: `DDL ${ref.name}` })}><FileCode2 /> Open in editor</Button>
          </div>
          <div className="ddlbox">
            <SqlEditor value={t.ddl} onChange={() => {}} readOnly dialect={drv?.dialect} driverId={conn.driver} />
          </div>
        </section>
      )}
    </div>
  );
}

export function DefinitionTab({ tab, conn }: { tab: Tab; conn: Connection }) {
  const ref = tab.ref!;
  const drv = useDriver(conn.driver);
  const d = useQuery({
    queryKey: ["definition", conn.id, ref.database ?? "", ref.schema ?? "", ref.name, ref.kind ?? ""],
    queryFn: () => get<{ definition: string }>(`c/${conn.id}/definition${qs({ db: ref.database, schema: ref.schema, name: ref.name, kind: ref.kind })}`),
    retry: false,
  });
  return (
    <div className="structure structure--def">
      <header className="structure__head">
        <div className="grow">
          <div className="eyebrow">{(ref.kind ?? "object").replace("_", " ")}</div>
          <h1 className="structure__title display">{ref.name}</h1>
        </div>
        {d.data && (
          <div className="row gap-3">
            <Button onClick={() => { navigator.clipboard?.writeText(d.data!.definition); toast.success("Definition copied"); }}><Copy /> Copy</Button>
            <Button variant="primary" onClick={() => newQueryTab(conn, { sql: d.data!.definition, database: ref.database, schema: ref.schema, title: ref.name })}><FileCode2 /> Edit in query tab</Button>
          </div>
        )}
      </header>
      {d.isLoading && <Spinner large />}
      {d.error && <Alert kind="danger">{(d.error as Error).message}</Alert>}
      {d.data && (
        <div className="ddlbox ddlbox--tall">
          <SqlEditor value={d.data.definition} onChange={() => {}} readOnly dialect={drv?.dialect} driverId={conn.driver} />
        </div>
      )}
    </div>
  );
}
