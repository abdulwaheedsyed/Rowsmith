import { Fragment, type ReactNode } from "react";

// A small Markdown renderer for assistant answers. It builds React elements
// (never HTML strings), so nothing in an answer can inject markup. Code fences
// that are still open while an answer streams render as code already.

export type CodeRenderer = (code: string, lang: string, closed: boolean) => ReactNode;

type Block =
  | { t: "code"; lang: string; code: string; closed: boolean }
  | { t: "h"; level: number; text: string }
  | { t: "list"; ordered: boolean; items: string[] }
  | { t: "quote"; text: string }
  | { t: "table"; head: string[]; rows: string[][] }
  | { t: "hr" }
  | { t: "p"; text: string };

function parse(src: string): Block[] {
  const lines = src.replace(/\r\n?/g, "\n").split("\n");
  const out: Block[] = [];
  let i = 0;
  const isTableSep = (l: string) => /^\s*\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?\s*$/.test(l);
  const cells = (l: string) => l.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((c) => c.trim());
  while (i < lines.length) {
    const line = lines[i];
    const fence = line.match(/^\s*(```|~~~)\s*([\w+-]*)\s*$/);
    if (fence) {
      const body: string[] = [];
      i++;
      let closed = false;
      while (i < lines.length) {
        if (lines[i].trim().startsWith(fence[1])) {
          closed = true;
          i++;
          break;
        }
        body.push(lines[i++]);
      }
      out.push({ t: "code", lang: fence[2].toLowerCase(), code: body.join("\n"), closed });
      continue;
    }
    if (!line.trim()) {
      i++;
      continue;
    }
    const h = line.match(/^(#{1,4})\s+(.*)$/);
    if (h) {
      out.push({ t: "h", level: h[1].length, text: h[2] });
      i++;
      continue;
    }
    if (/^\s*([-*_])\s*\1\s*\1[\s\-*_]*$/.test(line)) {
      out.push({ t: "hr" });
      i++;
      continue;
    }
    if (line.includes("|") && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const head = cells(line);
      const rows: string[][] = [];
      i += 2;
      while (i < lines.length && lines[i].includes("|") && lines[i].trim()) rows.push(cells(lines[i++]));
      out.push({ t: "table", head, rows });
      continue;
    }
    const li = line.match(/^\s*([-*+]|\d+[.)])\s+(.*)$/);
    if (li) {
      const ordered = /\d/.test(li[1]);
      const items: string[] = [];
      while (i < lines.length) {
        const m = lines[i].match(/^\s*([-*+]|\d+[.)])\s+(.*)$/);
        if (m && /\d/.test(m[1]) === ordered) {
          items.push(m[2]);
          i++;
        } else if (lines[i].trim() && /^\s{2,}/.test(lines[i]) && items.length) {
          items[items.length - 1] += " " + lines[i++].trim(); // continuation
        } else break;
      }
      out.push({ t: "list", ordered, items });
      continue;
    }
    if (line.startsWith(">")) {
      const q: string[] = [];
      while (i < lines.length && lines[i].startsWith(">")) q.push(lines[i++].replace(/^>\s?/, ""));
      out.push({ t: "quote", text: q.join(" ") });
      continue;
    }
    const p: string[] = [];
    while (i < lines.length && lines[i].trim() && !/^\s*(```|~~~|#{1,4}\s|[-*+]\s|\d+[.)]\s|>)/.test(lines[i])) p.push(lines[i++]);
    if (p.length === 0) p.push(lines[i++]);
    out.push({ t: "p", text: p.join(" ") });
  }
  return out;
}

/** Inline markup: `code`, **bold**, *emphasis*, [links](https://…). */
export function inline(text: string, key = "i"): ReactNode[] {
  const out: ReactNode[] = [];
  const re = /(`[^`]+`)|(\*\*[^*]+\*\*)|(\*[^*\s][^*]*\*)|(_[^_\s][^_]*_)|(\[[^\]]+\]\((https?:\/\/[^)\s]+)\))/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let n = 0;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    const k = `${key}-${n++}`;
    const s = m[0];
    if (m[1]) out.push(<code key={k} className="md-code">{s.slice(1, -1)}</code>);
    else if (m[2]) out.push(<strong key={k}>{inline(s.slice(2, -2), k)}</strong>);
    else if (m[3] || m[4]) out.push(<em key={k}>{inline(s.slice(1, -1), k)}</em>);
    else if (m[5]) {
      const label = s.slice(1, s.indexOf("]("));
      out.push(<a key={k} href={m[6]} target="_blank" rel="noopener noreferrer nofollow">{label}</a>);
    }
    last = m.index + s.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

export function Markdown({ text, code }: { text: string; code: CodeRenderer }) {
  const blocks = parse(text);
  return (
    <div className="md">
      {blocks.map((b, i) => {
        const k = `b${i}`;
        switch (b.t) {
          case "code":
            return <Fragment key={k}>{code(b.code, b.lang, b.closed)}</Fragment>;
          case "h":
            return b.level <= 2 ? <h3 key={k} className="md-h">{inline(b.text, k)}</h3> : <h4 key={k} className="md-h md-h--sub">{inline(b.text, k)}</h4>;
          case "list": {
            const L = b.ordered ? "ol" : "ul";
            return <L key={k} className="md-list">{b.items.map((it, j) => <li key={j}>{inline(it, `${k}-${j}`)}</li>)}</L>;
          }
          case "quote":
            return <blockquote key={k} className="md-quote">{inline(b.text, k)}</blockquote>;
          case "table":
            return (
              <div key={k} className="md-table">
                <table>
                  <thead><tr>{b.head.map((h, j) => <th key={j}>{inline(h, `${k}h${j}`)}</th>)}</tr></thead>
                  <tbody>{b.rows.map((r, ri) => <tr key={ri}>{r.map((c, j) => <td key={j}>{inline(c, `${k}r${ri}${j}`)}</td>)}</tr>)}</tbody>
                </table>
              </div>
            );
          case "hr":
            return <hr key={k} className="md-hr" />;
          default:
            return <p key={k}>{inline(b.text, k)}</p>;
        }
      })}
    </div>
  );
}

const KEYWORDS = new Set(("select from where and or not in is null as join left right inner outer full cross on group by order having limit offset " +
  "insert into values update set delete create table alter drop index view with union all distinct case when then else end exists between like ilike " +
  "asc desc primary key foreign references default returning over partition window count sum avg min max coalesce cast true false interval " +
  "begin commit rollback explain analyze top fetch first rows only nulls lateral using natural recursive filter").split(" "));

/** Tokenizes SQL for display: keywords, strings, numbers, comments. */
export function highlightSQL(code: string): ReactNode[] {
  const out: ReactNode[] = [];
  const re = /(--[^\n]*|\/\*[\s\S]*?\*\/)|('(?:[^']|'')*'?)|("(?:[^"]|"")*"|`[^`]*`|\[[^\]\n]*\])|(\b\d+(?:\.\d+)?\b)|([A-Za-z_][A-Za-z0-9_$]*)/g;
  let last = 0;
  let m: RegExpExecArray | null;
  let n = 0;
  while ((m = re.exec(code))) {
    if (m.index > last) out.push(code.slice(last, m.index));
    const s = m[0];
    const k = n++;
    if (m[1]) out.push(<span key={k} className="hl-comment">{s}</span>);
    else if (m[2]) out.push(<span key={k} className="hl-string">{s}</span>);
    else if (m[3]) out.push(<span key={k} className="hl-ident">{s}</span>);
    else if (m[4]) out.push(<span key={k} className="hl-number">{s}</span>);
    else if (KEYWORDS.has(s.toLowerCase())) out.push(<span key={k} className="hl-keyword">{s}</span>);
    else out.push(s);
    last = m.index + s.length;
  }
  if (last < code.length) out.push(code.slice(last));
  return out;
}
