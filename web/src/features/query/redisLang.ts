import { StreamLanguage, type StringStream } from "@codemirror/language";
import type { CompletionContext, CompletionResult } from "@codemirror/autocomplete";
import type { Extension } from "@codemirror/state";

// The Redis console language (redis-cli syntax): a command per line, then
// arguments, "double-quoted" with escapes or 'single-quoted'.

const COMMANDS = `APPEND BITCOUNT BITOP BITPOS BLMOVE BLPOP BRPOP BZPOPMAX BZPOPMIN CLIENT CONFIG COPY DBSIZE DECR DECRBY DEL DUMP ECHO
EVAL EVALSHA EVAL_RO EXEC EXISTS EXPIRE EXPIREAT EXPIRETIME FCALL FLUSHALL FLUSHDB FUNCTION GEOADD GEODIST GEOHASH GEOPOS GEOSEARCH
GET GETBIT GETDEL GETEX GETRANGE GETSET HDEL HEXISTS HEXPIRE HGET HGETALL HINCRBY HINCRBYFLOAT HKEYS HLEN HMGET HPERSIST HRANDFIELD
HSCAN HSET HSETNX HSTRLEN HTTL HVALS INCR INCRBY INCRBYFLOAT INFO KEYS LASTSAVE LINDEX LINSERT LLEN LMOVE LMPOP LPOP LPOS LPUSH
LPUSHX LRANGE LREM LSET LTRIM MEMORY MGET MOVE MSET MSETNX MULTI OBJECT PERSIST PEXPIRE PEXPIREAT PFADD PFCOUNT PFMERGE PING PSETEX
PTTL PUBLISH RANDOMKEY RENAME RENAMENX RESTORE RPOP RPUSH RPUSHX SADD SCAN SCARD SCRIPT SDIFF SDIFFSTORE SELECT SET SETBIT SETEX
SETNX SETRANGE SINTER SINTERCARD SINTERSTORE SISMEMBER SLOWLOG SMEMBERS SMISMEMBER SMOVE SORT SPOP SRANDMEMBER SREM SSCAN STRLEN
SUNION SUNIONSTORE SWAPDB TIME TOUCH TTL TYPE UNLINK UNWATCH WATCH XACK XADD XAUTOCLAIM XCLAIM XDEL XGROUP XINFO XLEN XPENDING XRANGE
XREAD XREADGROUP XREVRANGE XTRIM ZADD ZCARD ZCOUNT ZDIFF ZINCRBY ZINTER ZINTERCARD ZLEXCOUNT ZMPOP ZMSCORE ZPOPMAX ZPOPMIN
ZRANDMEMBER ZRANGE ZRANGEBYLEX ZRANGEBYSCORE ZRANGESTORE ZRANK ZREM ZREMRANGEBYLEX ZREMRANGEBYRANK ZREMRANGEBYSCORE ZREVRANGE
ZREVRANK ZSCAN ZSCORE ZUNION ZUNIONSTORE JSON.GET JSON.SET JSON.DEL JSON.MGET JSON.TYPE`.split(/\s+/);

const OPTIONS = new Set(["EX", "PX", "EXAT", "PXAT", "NX", "XX", "GT", "LT", "KEEPTTL", "GET", "MATCH", "COUNT", "TYPE", "WITHSCORES",
  "WITHVALUES", "LIMIT", "REV", "BYSCORE", "BYLEX", "ASYNC", "SYNC", "MAXLEN", "MINID", "STREAMS", "GROUP", "BLOCK", "NOACK", "CH", "INCR"]);

interface State { first: boolean }

const redis = StreamLanguage.define<State>({
  name: "redis",
  startState: () => ({ first: true }),
  token(stream: StringStream, state: State) {
    if (stream.sol()) state.first = true;
    if (stream.eatSpace()) return null;
    if (state.first && (stream.match("#") || stream.match("//"))) {
      stream.skipToEnd();
      return "comment";
    }
    const q = stream.peek();
    if (q === '"' || q === "'") {
      stream.next();
      let escaped = false;
      for (let c = stream.next(); c !== undefined; c = stream.next()) {
        if (c === q && !escaped) break;
        escaped = q === '"' && c === "\\" && !escaped;
      }
      state.first = false;
      return "string";
    }
    stream.eatWhile(/\S/);
    const word = stream.current();
    if (state.first) {
      state.first = false;
      return "keyword";
    }
    if (/^-?\d+(\.\d+)?$/.test(word) || /^[+-]inf$/i.test(word)) return "number";
    if (OPTIONS.has(word.toUpperCase())) return "atom";
    return null;
  },
  languageData: { commentTokens: { line: "#" } },
});

function complete(ctx: CompletionContext): CompletionResult | null {
  const line = ctx.state.doc.lineAt(ctx.pos);
  const before = line.text.slice(0, ctx.pos - line.from);
  const m = before.match(/^\s*([\w.]*)$/);
  if (!m || (!m[1] && !ctx.explicit)) return null;
  return { from: ctx.pos - m[1].length, options: COMMANDS.map((c) => ({ label: c, type: "keyword" })), validFor: /^[\w.]*$/ };
}

export function redisLanguage(): Extension {
  return [redis, redis.data.of({ autocomplete: complete })];
}
