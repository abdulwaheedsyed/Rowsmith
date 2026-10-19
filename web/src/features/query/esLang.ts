import { StreamLanguage, type StringStream } from "@codemirror/language";
import type { CompletionContext, CompletionResult } from "@codemirror/autocomplete";
import type { Extension } from "@codemirror/state";

// The Elasticsearch / OpenSearch console language (Kibana Dev Tools
// syntax): a method and a path on one line, then a JSON body, with """
// strings for scripts and # comments between requests.

const METHODS = ["GET", "POST", "PUT", "DELETE", "HEAD", "PATCH"];

const ENDPOINTS = `_search _count _doc _create _update _bulk _mget _msearch _mapping _settings _alias _aliases _analyze _explain
_field_caps _validate/query _delete_by_query _update_by_query _reindex _refresh _flush _forcemerge _stats _cat/indices _cat/nodes
_cat/shards _cat/aliases _cat/health _cat/count _cluster/health _cluster/settings _cluster/state _nodes/stats _tasks _data_stream
_index_template _component_template _ingest/pipeline _sql _query _plugins/_sql _plugins/_ppl _security/_authenticate _search/scroll`.split(/\s+/);

const QUERY_KEYS = `query bool must should filter must_not match match_phrase match_phrase_prefix multi_match term terms range exists
ids prefix wildcard regexp fuzzy query_string simple_query_string nested has_child has_parent geo_distance geo_bounding_box
knn size from sort _source aggs aggregations terms avg sum min max cardinality value_count date_histogram histogram percentiles
top_hits stats extended_stats filters composite script_fields highlight track_total_hits search_after pit fields collapse gte gt lte lt
format boost minimum_should_match field order missing interval calendar_interval fixed_interval`.split(/\s+/);

interface State { inBody: boolean; triple: boolean }

const es = StreamLanguage.define<State>({
  name: "elasticsearch",
  startState: () => ({ inBody: false, triple: false }),
  token(stream: StringStream, state: State) {
    if (state.triple) {
      if (stream.skipTo('"""')) {
        stream.match('"""');
        state.triple = false;
      } else stream.skipToEnd();
      return "string";
    }
    if (stream.sol()) {
      const m = stream.match(/^\s*(GET|POST|PUT|DELETE|HEAD|PATCH)\b/i, false);
      if (m) {
        stream.eatSpace();
        stream.eatWhile(/[A-Za-z]/);
        state.inBody = true;
        return "keyword";
      }
    }
    if (stream.eatSpace()) return null;
    const ch = stream.peek();
    if (ch === "#" || (ch === "/" && stream.match("//", false))) {
      stream.skipToEnd();
      return "comment";
    }
    if (stream.match('"""')) {
      state.triple = true;
      return "string";
    }
    if (ch === '"') {
      stream.next();
      let escaped = false;
      for (let c = stream.next(); c !== undefined; c = stream.next()) {
        if (c === '"' && !escaped) break;
        escaped = c === "\\" && !escaped;
      }
      // A key when a colon follows.
      return stream.match(/^\s*:/, false) ? "propertyName" : "string";
    }
    if (stream.match(/^-?\d+(\.\d+)?([eE][+-]?\d+)?/)) return "number";
    if (stream.match(/^(true|false|null)\b/)) return "atom";
    if (/[{}[\],:]/.test(ch ?? "")) {
      stream.next();
      return "punctuation";
    }
    // The path after the method.
    stream.eatWhile(/\S/);
    return "url";
  },
  languageData: { commentTokens: { line: "#" }, closeBrackets: { brackets: ["(", "[", "{", '"'] } },
});

function complete(ctx: CompletionContext): CompletionResult | null {
  const line = ctx.state.doc.lineAt(ctx.pos);
  const before = line.text.slice(0, ctx.pos - line.from);
  const method = before.match(/^\s*([A-Za-z]*)$/);
  if (method && (method[1] || ctx.explicit)) {
    return { from: ctx.pos - method[1].length, options: METHODS.map((m) => ({ label: m, type: "keyword", apply: m + " " })), validFor: /^[A-Za-z]*$/ };
  }
  const path = before.match(/^\s*(?:GET|POST|PUT|DELETE|HEAD|PATCH)\s+\S*?([\w/.]*)$/i);
  if (path) {
    return { from: ctx.pos - path[1].length, options: ENDPOINTS.map((e) => ({ label: e, type: "function" })), validFor: /^[\w/.]*$/ };
  }
  const key = before.match(/"([\w.]*)$/);
  if (key) {
    return { from: ctx.pos - key[1].length, options: QUERY_KEYS.map((k) => ({ label: k, type: "property" })), validFor: /^[\w.]*$/ };
  }
  return null;
}

export function esLanguage(): Extension {
  return [es, es.data.of({ autocomplete: complete })];
}
