package redis

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"rowsmith/internal/driver"
)

// The console reads redis-cli syntax: one command per line, arguments
// separated by spaces, "double quotes" with backslash escapes (\n, \t, \",
// \\, \xHH) and 'single quotes' taken literally. A quoted argument may span
// lines. Lines starting with # or // are comments.

type command struct {
	args  []string
	text  string
	start int
	end   int
	line  int
}

// name is the command, with the subcommand for container commands such as
// CONFIG GET or CLIENT KILL.
func (c command) name() string {
	n := strings.ToUpper(c.args[0])
	if containers[n] && len(c.args) > 1 {
		return n + " " + strings.ToUpper(c.args[1])
	}
	return n
}

var containers = map[string]bool{
	"ACL": true, "CLIENT": true, "CLUSTER": true, "COMMAND": true, "CONFIG": true, "DEBUG": true, "FUNCTION": true,
	"LATENCY": true, "MEMORY": true, "MODULE": true, "OBJECT": true, "PUBSUB": true, "SCRIPT": true, "SLOWLOG": true,
	"XGROUP": true, "XINFO": true,
}

// SyntaxError reports where a script stops parsing.
type SyntaxError struct {
	Line int
	Msg  string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("line %d: %s", e.Line, e.Msg) }

func parseScript(script string) ([]command, error) {
	var out []command
	i, line := 0, 1
	for i < len(script) {
		// Skip blank space between commands, counting lines.
		for i < len(script) && (script[i] == ' ' || script[i] == '\t' || script[i] == '\r' || script[i] == '\n') {
			if script[i] == '\n' {
				line++
			}
			i++
		}
		if i >= len(script) {
			break
		}
		if script[i] == '#' || strings.HasPrefix(script[i:], "//") {
			for i < len(script) && script[i] != '\n' {
				i++
			}
			continue
		}
		cmd := command{start: i, line: line}
		for i < len(script) && script[i] != '\n' {
			c := script[i]
			if c == ' ' || c == '\t' || c == '\r' {
				i++
				continue
			}
			var arg strings.Builder
			switch c {
			case '"':
				i++
				closed := false
				for i < len(script) {
					c := script[i]
					if c == '\n' {
						line++
					}
					if c == '\\' && i+1 < len(script) {
						i++
						switch e := script[i]; e {
						case 'n':
							arg.WriteByte('\n')
						case 'r':
							arg.WriteByte('\r')
						case 't':
							arg.WriteByte('\t')
						case 'b':
							arg.WriteByte('\b')
						case 'a':
							arg.WriteByte('\a')
						case 'x':
							if i+2 < len(script) && isHex(script[i+1]) && isHex(script[i+2]) {
								arg.WriteByte(unhex(script[i+1])<<4 | unhex(script[i+2]))
								i += 2
							} else {
								arg.WriteByte('x')
							}
						default:
							arg.WriteByte(e)
						}
						i++
						continue
					}
					if c == '"' {
						closed = true
						i++
						break
					}
					arg.WriteByte(c)
					i++
				}
				if !closed {
					return nil, &SyntaxError{Line: cmd.line, Msg: "unbalanced quotes: a \" is never closed"}
				}
				if i < len(script) && !isSpace(script[i]) {
					return nil, &SyntaxError{Line: line, Msg: "a closing quote must be followed by a space"}
				}
			case '\'':
				i++
				closed := false
				for i < len(script) {
					c := script[i]
					if c == '\n' {
						line++
					}
					if c == '\\' && i+1 < len(script) && script[i+1] == '\'' {
						arg.WriteByte('\'')
						i += 2
						continue
					}
					if c == '\'' {
						closed = true
						i++
						break
					}
					arg.WriteByte(c)
					i++
				}
				if !closed {
					return nil, &SyntaxError{Line: cmd.line, Msg: "unbalanced quotes: a ' is never closed"}
				}
				if i < len(script) && !isSpace(script[i]) {
					return nil, &SyntaxError{Line: line, Msg: "a closing quote must be followed by a space"}
				}
			default:
				for i < len(script) && !isSpace(script[i]) {
					arg.WriteByte(script[i])
					i++
				}
			}
			cmd.args = append(cmd.args, arg.String())
		}
		cmd.end = i
		cmd.text = strings.TrimSpace(script[cmd.start:cmd.end])
		if len(cmd.args) > 0 {
			out = append(out, cmd)
		}
	}
	return out, nil
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// quoteArg writes an argument so the console reads it back unchanged.
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	plain := true
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c == '"' || c == '\'' || c == '\\' || c == 0x7f || c >= 0x80 {
			plain = false
			break
		}
	}
	if plain && s[0] != '#' && !strings.HasPrefix(s, "//") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		switch c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if c >= 0x80 {
				if r, n := utf8.DecodeRuneInString(s[i:]); r != utf8.RuneError || n > 1 {
					b.WriteString(s[i : i+n])
					i += n
					continue
				}
			}
			if c < ' ' || c >= 0x7f {
				fmt.Fprintf(&b, `\x%02x`, c)
			} else {
				b.WriteByte(c)
			}
		}
		i++
	}
	b.WriteByte('"')
	return b.String()
}

// ---- classification ------------------------------------------------------------

var readCommands = set(`BITCOUNT BITFIELD_RO BITPOS DBSIZE DUMP ECHO EXISTS EXPIRETIME GEODIST GEOHASH GEOPOS
	GEORADIUS_RO GEORADIUSBYMEMBER_RO GEOSEARCH GET GETBIT GETRANGE HEXISTS HEXPIRETIME HGET HGETALL HKEYS HLEN HMGET
	HPEXPIRETIME HPTTL HRANDFIELD HSCAN HSTRLEN HTTL HVALS INFO KEYS LASTSAVE LCS LINDEX LLEN LOLWUT LPOS LRANGE MGET
	PEXPIRETIME PFCOUNT PING PTTL RANDOMKEY ROLE SCAN SCARD SDIFF SINTER SINTERCARD SISMEMBER SMEMBERS SMISMEMBER
	SORT_RO SRANDMEMBER SSCAN STRLEN SUBSTR SUNION TIME TOUCH TTL TYPE XLEN XPENDING XRANGE XREAD XREVRANGE ZCARD ZCOUNT
	ZDIFF ZINTER ZINTERCARD ZLEXCOUNT ZMSCORE ZRANDMEMBER ZRANGE ZRANGEBYLEX ZRANGEBYSCORE ZRANK ZREVRANGE ZREVRANGEBYLEX
	ZREVRANGEBYSCORE ZREVRANK ZSCAN ZSCORE ZUNION EVAL_RO EVALSHA_RO FCALL_RO
	ACL\ CAT ACL\ DRYRUN ACL\ GETUSER ACL\ LIST ACL\ LOG ACL\ USERS ACL\ WHOAMI ACL\ GENPASS
	CLIENT\ GETNAME CLIENT\ ID CLIENT\ INFO CLIENT\ LIST CLIENT\ TRACKINGINFO CLIENT\ GETREDIR
	CLUSTER\ INFO CLUSTER\ NODES CLUSTER\ SLOTS CLUSTER\ SHARDS CLUSTER\ KEYSLOT CLUSTER\ MYID CLUSTER\ COUNTKEYSINSLOT CLUSTER\ GETKEYSINSLOT
	COMMAND COMMAND\ COUNT COMMAND\ DOCS COMMAND\ GETKEYS COMMAND\ INFO COMMAND\ LIST
	CONFIG\ GET FUNCTION\ DUMP FUNCTION\ LIST FUNCTION\ STATS LATENCY\ DOCTOR LATENCY\ GRAPH LATENCY\ HISTOGRAM
	LATENCY\ HISTORY LATENCY\ LATEST MEMORY\ DOCTOR MEMORY\ MALLOC-STATS MEMORY\ STATS MEMORY\ USAGE MODULE\ LIST
	OBJECT\ ENCODING OBJECT\ FREQ OBJECT\ IDLETIME OBJECT\ REFCOUNT PUBSUB\ CHANNELS PUBSUB\ NUMPAT PUBSUB\ NUMSUB
	PUBSUB\ SHARDCHANNELS PUBSUB\ SHARDNUMSUB SCRIPT\ EXISTS SLOWLOG\ GET SLOWLOG\ LEN XINFO\ CONSUMERS XINFO\ GROUPS XINFO\ STREAM
	JSON.GET JSON.MGET JSON.TYPE JSON.STRLEN JSON.ARRLEN JSON.ARRINDEX JSON.OBJKEYS JSON.OBJLEN JSON.RESP JSON.DEBUG
	FT.SEARCH FT.AGGREGATE FT.INFO FT._LIST FT.EXPLAIN FT.PROFILE FT.TAGVALS FT.SPELLCHECK FT.SUGGET FT.SUGLEN FT.SYNDUMP FT.DICTDUMP
	TS.GET TS.MGET TS.RANGE TS.REVRANGE TS.MRANGE TS.MREVRANGE TS.INFO TS.QUERYINDEX
	BF.EXISTS BF.MEXISTS BF.INFO BF.CARD CF.EXISTS CF.MEXISTS CF.COUNT CF.INFO CMS.QUERY CMS.INFO TOPK.QUERY TOPK.LIST TOPK.INFO
	TDIGEST.QUANTILE TDIGEST.CDF TDIGEST.MIN TDIGEST.MAX TDIGEST.INFO TDIGEST.RANK TDIGEST.REVRANK TDIGEST.BYRANK TDIGEST.BYREVRANK TDIGEST.TRIMMED_MEAN
	VSIM VCARD VDIM VEMB VGETATTR VINFO VLINKS VRANDMEMBER`)

var sessionCommands = set(`SELECT AUTH WATCH UNWATCH READONLY READWRITE ASKING CLIENT\ SETNAME CLIENT\ SETINFO CLIENT\ NO-TOUCH`)

var txCommands = set(`MULTI EXEC DISCARD`)

var dclCommands = set(`ACL\ SETUSER ACL\ DELUSER ACL\ LOAD ACL\ SAVE`)

// Server administration: configuration, persistence, replication, scripts.
var adminCommands = set(`FLUSHDB FLUSHALL SWAPDB SHUTDOWN SAVE BGSAVE BGREWRITEAOF REPLICAOF SLAVEOF FAILOVER
	CONFIG\ SET CONFIG\ REWRITE CONFIG\ RESETSTAT SCRIPT\ FLUSH SCRIPT\ LOAD SCRIPT\ KILL FUNCTION\ LOAD FUNCTION\ DELETE
	FUNCTION\ FLUSH FUNCTION\ RESTORE FUNCTION\ KILL MODULE\ LOAD MODULE\ LOADEX MODULE\ UNLOAD CLIENT\ KILL CLIENT\ PAUSE
	CLIENT\ UNPAUSE CLIENT\ NO-EVICT SLOWLOG\ RESET LATENCY\ RESET MEMORY\ PURGE FT.CREATE FT.DROPINDEX FT.ALTER FT.ALIASADD
	FT.ALIASDEL FT.ALIASUPDATE`)

// Commands the console cannot carry: they change the protocol or stream
// replies forever.
var unsupported = map[string]string{
	"SUBSCRIBE": "subscriptions", "PSUBSCRIBE": "subscriptions", "SSUBSCRIBE": "subscriptions",
	"UNSUBSCRIBE": "subscriptions", "PUNSUBSCRIBE": "subscriptions", "SUNSUBSCRIBE": "subscriptions",
	"MONITOR": "MONITOR", "SYNC": "replication", "PSYNC": "replication", "HELLO": "protocol changes", "QUIT": "QUIT",
	"RESET": "RESET", "CLIENT REPLY": "CLIENT REPLY",
}

func set(words string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.Fields(strings.ReplaceAll(words, `\ `, "\x00")) {
		out[strings.ReplaceAll(w, "\x00", " ")] = true
	}
	return out
}

func (c command) kind() driver.StatementKind {
	n := c.name()
	switch {
	case readCommands[n]:
		return driver.StmtRead
	case sessionCommands[n]:
		return driver.StmtSession
	case txCommands[n]:
		return driver.StmtTCL
	case dclCommands[n]:
		return driver.StmtDCL
	case adminCommands[n]:
		return driver.StmtDDL
	}
	return driver.StmtWrite
}

func (c command) danger(k driver.StatementKind) driver.Danger {
	switch c.name() {
	case "FLUSHDB":
		return driver.Danger{Level: "destructive", Reason: "FLUSHDB deletes every key in the database"}
	case "FLUSHALL":
		return driver.Danger{Level: "destructive", Reason: "FLUSHALL deletes every key in every database"}
	case "SHUTDOWN":
		return driver.Danger{Level: "destructive", Reason: "SHUTDOWN stops the server"}
	case "SWAPDB":
		return driver.Danger{Level: "destructive", Reason: "SWAPDB exchanges the contents of two databases"}
	case "SCRIPT FLUSH", "FUNCTION FLUSH":
		return driver.Danger{Level: "destructive", Reason: "removes every stored script or function"}
	case "DEBUG RELOAD", "DEBUG RESTART", "DEBUG CRASH-AND-RECOVER", "DEBUG SEGFAULT":
		return driver.Danger{Level: "destructive", Reason: "DEBUG can restart or crash the server"}
	case "KEYS":
		return driver.Danger{Level: "caution", Reason: "KEYS blocks the server while it walks every key; SCAN does not"}
	case "DEL", "UNLINK":
		return driver.Danger{Level: "caution", Reason: "deletes keys"}
	}
	switch k {
	case driver.StmtRead, driver.StmtSession, driver.StmtTCL:
		return driver.Danger{}
	case driver.StmtDDL:
		return driver.Danger{Level: "caution", Reason: "changes the server"}
	case driver.StmtDCL:
		return driver.Danger{Level: "caution", Reason: "changes users and permissions"}
	}
	return driver.Danger{Level: "caution", Reason: "modifies data"}
}

// SplitScript implements driver.ScriptSplitter for the console language.
func (redisDriver) SplitScript(script string) ([]driver.ScriptStatement, error) {
	cmds, err := parseScript(script)
	if err != nil {
		return nil, err
	}
	out := make([]driver.ScriptStatement, len(cmds))
	for i, c := range cmds {
		k := c.kind()
		out[i] = driver.ScriptStatement{SQL: c.text, Start: c.start, End: c.end, Line: c.line, Kind: k, Danger: c.danger(k)}
	}
	return out, nil
}
