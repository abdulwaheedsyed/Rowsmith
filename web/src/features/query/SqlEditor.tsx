import { useEffect, useRef } from "react";
import { EditorState, Compartment, StateEffect, StateField, RangeSet, type Extension } from "@codemirror/state";
import {
  EditorView, keymap, lineNumbers, highlightActiveLine, highlightActiveLineGutter, drawSelection, dropCursor,
  rectangularSelection, crosshairCursor, placeholder as cmPlaceholder, Decoration, gutter, GutterMarker, type DecorationSet,
} from "@codemirror/view";
import { defaultKeymap, history, historyKeymap, indentWithTab, toggleComment } from "@codemirror/commands";
import { sql, MySQL, MariaSQL, PostgreSQL, MSSQL, PLSQL, SQLite, StandardSQL, type SQLDialect } from "@codemirror/lang-sql";
import { json } from "@codemirror/lang-json";
import { autocompletion, closeBrackets, closeBracketsKeymap, completionKeymap } from "@codemirror/autocomplete";
import { bracketMatching, indentOnInput, syntaxHighlighting, HighlightStyle } from "@codemirror/language";
import { searchKeymap, highlightSelectionMatches } from "@codemirror/search";
import { tags as t } from "@lezer/highlight";
import type { CatalogTable, Danger, StatementKind } from "../../lib/types";

export function dialectFor(d?: string, driverId?: string): SQLDialect {
  if (driverId === "mariadb") return MariaSQL;
  switch (d) {
    case "mysql": return MySQL;
    case "postgresql": return PostgreSQL;
    case "mssql": return MSSQL;
    case "plsql": return PLSQL;
    case "sqlite": return SQLite;
    default: return StandardSQL;
  }
}

// Theme built entirely from CSS variables, so it follows light/dark instantly.
const theme = EditorView.theme({
  "&": { height: "100%", fontSize: "13px", backgroundColor: "var(--surface)", color: "var(--syn-variable)" },
  ".cm-scroller": { fontFamily: "var(--font-mono)", lineHeight: "1.62", overscrollBehavior: "contain" },
  ".cm-content": { padding: "10px 0 40vh", caretColor: "var(--accent)" },
  ".cm-line": { padding: "0 14px 0 8px" },
  "&.cm-focused": { outline: "none" },
  ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--accent)", borderLeftWidth: "2px" },
  ".cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection": { backgroundColor: "var(--grid-sel) !important" },
  ".cm-activeLine": { backgroundColor: "var(--grid-alt)" },
  ".cm-gutters": { backgroundColor: "var(--surface)", color: "var(--faint)", border: "none", fontSize: "11px" },
  ".cm-lineNumbers .cm-gutterElement": { padding: "0 8px 0 12px", minWidth: "36px" },
  ".cm-activeLineGutter": { backgroundColor: "transparent", color: "var(--text-2)" },
  ".cm-matchingBracket": { backgroundColor: "var(--accent-soft)", outline: "1px solid var(--accent-line)" },
  ".cm-selectionMatch": { backgroundColor: "var(--hover)" },
  ".cm-searchMatch": { backgroundColor: "color-mix(in srgb, var(--temper-2) 30%, transparent)" },
  ".cm-searchMatch-selected": { backgroundColor: "color-mix(in srgb, var(--temper-2) 55%, transparent)" },
  ".cm-placeholder": { color: "var(--faint)", fontStyle: "italic" },
  ".cm-tooltip": { backgroundColor: "var(--overlay)", border: "1px solid var(--line)", borderRadius: "7px", boxShadow: "var(--shadow-pop)", overflow: "hidden" },
  ".cm-tooltip-autocomplete > ul": { fontFamily: "var(--font-mono)", fontSize: "12px", maxHeight: "280px" },
  ".cm-tooltip-autocomplete > ul > li": { padding: "3px 10px !important", lineHeight: "1.5" },
  ".cm-tooltip-autocomplete > ul > li[aria-selected]": { backgroundColor: "var(--accent-soft)", color: "var(--text)" },
  ".cm-completionDetail": { color: "var(--faint)", fontStyle: "normal", marginLeft: "10px" },
  ".cm-completionIcon": { opacity: 0.6 },
  ".cm-panels": { backgroundColor: "var(--raised)", color: "var(--text)", borderColor: "var(--line)" },
  ".cm-panel.cm-search input, .cm-panel.cm-search button": { fontSize: "12px" },
  ".cm-textfield": { backgroundColor: "var(--sunken)", border: "1px solid var(--line)", borderRadius: "4px", color: "var(--text)" },
  ".cm-button": { backgroundImage: "none", backgroundColor: "var(--raised)", border: "1px solid var(--line)", borderRadius: "4px", color: "var(--text)" },
  ".cm-rs-error": { backgroundColor: "var(--danger-soft)", boxShadow: "inset 2px 0 0 var(--danger)" },
  ".cm-rs-running": { backgroundColor: "color-mix(in srgb, var(--accent) 7%, transparent)" },
  ".cm-rs-kind": { width: "10px", alignItems: "center" },
});

const highlight = HighlightStyle.define([
  { tag: [t.keyword, t.operatorKeyword, t.modifier, t.controlKeyword], color: "var(--syn-keyword)", fontWeight: "500" },
  { tag: [t.string, t.special(t.string), t.regexp], color: "var(--syn-string)" },
  { tag: [t.number, t.bool, t.null, t.atom], color: "var(--syn-number)" },
  { tag: [t.comment, t.lineComment, t.blockComment], color: "var(--syn-comment)", fontStyle: "italic" },
  { tag: [t.function(t.variableName), t.function(t.propertyName), t.standard(t.name)], color: "var(--syn-function)" },
  { tag: [t.typeName, t.className, t.standard(t.typeName)], color: "var(--syn-type)" },
  { tag: [t.operator, t.compareOperator, t.arithmeticOperator, t.logicOperator], color: "var(--syn-operator)" },
  { tag: [t.punctuation, t.separator, t.bracket, t.paren, t.brace, t.squareBracket], color: "var(--syn-punct)" },
  { tag: [t.special(t.name), t.quote], color: "var(--syn-variable)" },
  { tag: [t.variableName, t.propertyName, t.name], color: "var(--syn-variable)" },
  { tag: t.invalid, color: "var(--danger-text)" },
]);

// ---- statement markers & error highlighting -------------------------------------

export interface StatementMark {
  line: number; // 1-based
  kind: StatementKind;
  danger: Danger;
}

const setMarks = StateEffect.define<StatementMark[]>();
const setErrorLine = StateEffect.define<number | null>();

class KindMarker extends GutterMarker {
  constructor(readonly kind: StatementKind, readonly danger: Danger) {
    super();
  }
  eq(o: KindMarker) {
    return o.kind === this.kind && o.danger.level === this.danger.level;
  }
  toDOM() {
    const el = document.createElement("span");
    el.className = `stmt-mark stmt-mark--${this.danger.level || (this.kind === "read" ? "read" : "write")}`;
    el.title = this.danger.reason ? `${this.kind}: ${this.danger.reason}` : this.kind;
    return el;
  }
}

const marksField = StateField.define<RangeSet<GutterMarker>>({
  create: () => RangeSet.empty,
  update(value, tr) {
    value = value.map(tr.changes);
    for (const e of tr.effects) {
      if (e.is(setMarks)) {
        const ranges = e.value
          .filter((m) => m.line >= 1 && m.line <= tr.state.doc.lines && (m.kind !== "read" || m.danger.level))
          .sort((a, b) => a.line - b.line)
          .map((m) => new KindMarker(m.kind, m.danger).range(tr.state.doc.line(m.line).from));
        value = RangeSet.of(ranges, true);
      }
    }
    return value;
  },
});

const errorField = StateField.define<DecorationSet>({
  create: () => Decoration.none,
  update(value, tr) {
    value = value.map(tr.changes);
    for (const e of tr.effects) {
      if (e.is(setErrorLine)) {
        if (e.value === null || e.value < 1 || e.value > tr.state.doc.lines) value = Decoration.none;
        else value = Decoration.set([Decoration.line({ class: "cm-rs-error" }).range(tr.state.doc.line(e.value).from)]);
      }
    }
    if (tr.docChanged && value.size) value = Decoration.none;
    return value;
  },
  provide: (f) => EditorView.decorations.from(f),
});

const kindGutter = gutter({ class: "cm-rs-kind", markers: (v) => v.state.field(marksField) });

// ---- component ----------------------------------------------------------------------------

export interface EditorHandle {
  view: EditorView;
}

interface Props {
  value: string;
  onChange(v: string): void;
  dialect?: string;
  driverId?: string;
  language?: "sql" | "json";
  catalog?: CatalogTable[];
  defaultSchema?: string;
  onRun?(mode: "statement" | "all"): void;
  onExplain?(): void;
  onSave?(): void;
  onFormat?(): void;
  marks?: StatementMark[];
  errorLine?: number | null;
  readOnly?: boolean;
  placeholder?: string;
  handleRef?: React.MutableRefObject<EditorHandle | null>;
}

export function SqlEditor(props: Props) {
  const host = useRef<HTMLDivElement>(null);
  const view = useRef<EditorView | null>(null);
  const langComp = useRef(new Compartment());
  const roComp = useRef(new Compartment());
  const cbs = useRef(props);
  cbs.current = props;

  useEffect(() => {
    const keys = keymap.of([
      { key: "Mod-Enter", preventDefault: true, run: () => (cbs.current.onRun?.("statement"), true) },
      { key: "Shift-Mod-Enter", preventDefault: true, run: () => (cbs.current.onRun?.("all"), true) },
      { key: "F5", preventDefault: true, run: () => (cbs.current.onRun?.("all"), true) },
      { key: "Mod-e", preventDefault: true, run: () => (cbs.current.onExplain?.(), true) },
      { key: "Mod-s", preventDefault: true, run: () => (cbs.current.onSave?.(), true) },
      { key: "Shift-Alt-f", preventDefault: true, run: () => (cbs.current.onFormat?.(), true) },
      { key: "Mod-/", run: toggleComment },
    ]);
    const state = EditorState.create({
      doc: props.value,
      extensions: [
        keys,
        lineNumbers(),
        kindGutter,
        marksField,
        errorField,
        highlightActiveLineGutter(),
        history(),
        drawSelection(),
        dropCursor(),
        EditorState.allowMultipleSelections.of(true),
        indentOnInput(),
        syntaxHighlighting(highlight),
        bracketMatching(),
        closeBrackets(),
        autocompletion({ activateOnTyping: true, icons: true }),
        rectangularSelection(),
        crosshairCursor(),
        highlightActiveLine(),
        highlightSelectionMatches(),
        keymap.of([...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap, ...historyKeymap, ...completionKeymap, indentWithTab]),
        theme,
        EditorView.lineWrapping,
        cmPlaceholder(props.placeholder ?? "Write SQL here.  ⌘↵ runs the statement at the cursor · ⇧⌘↵ runs everything"),
        langComp.current.of(language(props)),
        roComp.current.of(EditorState.readOnly.of(!!props.readOnly)),
        EditorView.updateListener.of((u) => {
          if (u.docChanged) cbs.current.onChange(u.state.doc.toString());
        }),
      ],
    });
    const v = new EditorView({ state, parent: host.current! });
    view.current = v;
    if (props.handleRef) props.handleRef.current = { view: v };
    return () => {
      v.destroy();
      view.current = null;
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // Reconfigure the language when dialect or catalog changes.
  useEffect(() => {
    view.current?.dispatch({ effects: langComp.current.reconfigure(language(props)) });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [props.dialect, props.driverId, props.catalog, props.defaultSchema, props.language]);

  useEffect(() => {
    view.current?.dispatch({ effects: roComp.current.reconfigure(EditorState.readOnly.of(!!props.readOnly)) });
  }, [props.readOnly]);

  // External value changes (e.g. format, open saved query).
  useEffect(() => {
    const v = view.current;
    if (v && v.state.doc.toString() !== props.value) {
      v.dispatch({ changes: { from: 0, to: v.state.doc.length, insert: props.value } });
    }
  }, [props.value]);

  useEffect(() => {
    view.current?.dispatch({ effects: setMarks.of(props.marks ?? []) });
  }, [props.marks]);

  useEffect(() => {
    view.current?.dispatch({ effects: setErrorLine.of(props.errorLine ?? null) });
  }, [props.errorLine]);

  return <div className="sqleditor" ref={host} />;
}

function language(p: Props): Extension {
  if (p.language === "json") return json();
  const schema: Record<string, string[]> = {};
  for (const t of p.catalog ?? []) {
    const cols = t.columns.map((c) => c.name);
    schema[t.name] = cols;
    if (t.schema && t.schema !== p.defaultSchema) schema[`${t.schema}.${t.name}`] = cols;
  }
  return sql({ dialect: dialectFor(p.dialect, p.driverId), schema, upperCaseKeywords: true, defaultSchema: p.defaultSchema });
}

/** Byte offset (UTF-8) of a UTF-16 string index — the server splits by bytes. */
export function byteOffset(s: string, index: number) {
  return new TextEncoder().encode(s.slice(0, index)).length;
}
