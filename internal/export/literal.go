package export

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/twpayne/go-geom/encoding/geojson"
	"github.com/twpayne/go-geom/encoding/wkt"

	"rowsmith/internal/driver"
)

// Dialects are driver dialect names (driver.Info.Dialect).
const (
	MySQL    = "mysql"
	Postgres = "postgresql"
	MSSQL    = "mssql"
	Oracle   = "plsql"
	SQLite   = "sqlite"
	BigQuery = "bigquery"
)

var (
	numeric = regexp.MustCompile(`^[-+]?(\d+\.?\d*|\.\d+)([eE][-+]?\d+)?$`)
	isoDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	isoTS   = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?$`)
	isoTSTZ = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}(\.\d+)?([+-]\d{2}:\d{2}|Z)$`)
)

// oracleMaxLiteral is the longest string or RAW Oracle accepts in one literal.
const (
	oracleMaxLiteral = 4000
	oracleMaxRaw     = 2000
)

// ErrTooLarge is reported (as a count) for values a dialect cannot express
// as a literal; they are written as NULL.
type LiteralStats struct{ TooLarge int }

// Literal renders a cell as a SQL literal for dialect, using the result
// column to pick quoting and type constructors.
func Literal(dialect string, col driver.ResultColumn, v any, st *LiteralStats) string {
	typ := strings.ToLower(col.Type)
	switch x := v.(type) {
	case nil:
		return "NULL"
	case bool:
		return boolLit(dialect, x)
	case int64:
		return strconv.FormatInt(x, 10)
	case int:
		return strconv.Itoa(x)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return stringLit(dialect, strconv.FormatFloat(x, 'g', -1, 64), st)
		}
		return strconv.FormatFloat(x, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(x), 'g', -1, 32)
	case json.Number:
		return x.String()
	case string:
		return textLit(dialect, col, x, st)
	case driver.LongText:
		return textLit(dialect, col, x.Text, st)
	case driver.LargeBinary:
		return binLit(dialect, x.Data, st)
	case map[string]any:
		if b, ok := binBytes(x); ok {
			return binLit(dialect, b, st)
		}
		if t, ok := x["$text"].(string); ok {
			return textLit(dialect, col, t, st)
		}
		if g, ok := x["$geo"]; ok {
			srid, _ := x["srid"].(float64)
			if n, ok := x["srid"].(int); ok {
				srid = float64(n)
			}
			w, _ := x["wkt"].(string)
			return geoLit(dialect, typ, g, w, int(srid), st)
		}
		return stringLit(dialect, compactJSON(plainJSON(x)), st)
	case []any:
		return stringLit(dialect, compactJSON(plainJSON(x)), st)
	}
	s, _ := Text(v)
	return stringLit(dialect, s, st)
}

func boolLit(dialect string, b bool) string {
	switch dialect {
	case MSSQL, Oracle, SQLite:
		if b {
			return "1"
		}
		return "0"
	}
	if b {
		return "TRUE"
	}
	return "FALSE"
}

func textLit(dialect string, col driver.ResultColumn, s string, st *LiteralStats) string {
	switch col.Kind {
	case driver.KindInt, driver.KindFloat, driver.KindDecimal:
		if numeric.MatchString(s) {
			return s
		}
	case driver.KindBool:
		switch strings.ToLower(s) {
		case "true", "t", "1":
			return boolLit(dialect, true)
		case "false", "f", "0":
			return boolLit(dialect, false)
		}
	case driver.KindDate, driver.KindDateTime, driver.KindTimestamp, driver.KindTime:
		if dialect == Oracle {
			return oracleTime(s, st)
		}
	}
	return stringLit(dialect, s, st)
}

func oracleTime(s string, st *LiteralStats) string {
	q := oracleQuote(s)
	switch {
	case isoDate.MatchString(s):
		return "TO_DATE(" + q + ", 'YYYY-MM-DD')"
	case isoTS.MatchString(s):
		f := "'YYYY-MM-DD HH24:MI:SS'"
		if strings.Contains(s, ".") {
			f = "'YYYY-MM-DD HH24:MI:SS.FF'"
		}
		return "TO_TIMESTAMP(" + strings.Replace(q, "T", " ", 1) + ", " + f + ")"
	case isoTSTZ.MatchString(s):
		f := "'YYYY-MM-DD HH24:MI:SSTZH:TZM'"
		if strings.Contains(s, ".") {
			f = "'YYYY-MM-DD HH24:MI:SS.FFTZH:TZM'"
		}
		v := strings.Replace(strings.TrimSuffix(s, "Z"), "T", " ", 1)
		if strings.HasSuffix(s, "Z") {
			v += "+00:00"
		}
		return "TO_TIMESTAMP_TZ(" + oracleQuote(v) + ", " + f + ")"
	}
	return stringLit(Oracle, s, st)
}

func oracleQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func stringLit(dialect, s string, st *LiteralStats) string {
	switch dialect {
	case MySQL:
		r := strings.NewReplacer(`\`, `\\`, "'", `\'`, "\x00", `\0`, "\n", `\n`, "\r", `\r`, "\x1a", `\Z`)
		return "'" + r.Replace(s) + "'"
	case BigQuery:
		r := strings.NewReplacer(`\`, `\\`, "'", `\'`, "\n", `\n`, "\r", `\r`)
		return "'" + r.Replace(s) + "'"
	case MSSQL:
		return "N'" + strings.ReplaceAll(s, "'", "''") + "'"
	case Oracle:
		if len(s) <= oracleMaxLiteral {
			return oracleQuote(s)
		}
		// Longer text only fits a CLOB, built from literal pieces.
		var parts []string
		for len(s) > 0 {
			n := min(len(s), oracleMaxLiteral/4) // room for multi-byte characters
			for n < len(s) && !utf8Start(s[n]) {
				n--
			}
			parts = append(parts, "TO_CLOB("+oracleQuote(s[:n])+")")
			s = s[n:]
		}
		return strings.Join(parts, " || ")
	case Postgres:
		if strings.ContainsRune(s, 0) {
			s = strings.ReplaceAll(s, "\x00", "") // PostgreSQL text cannot hold NUL
		}
	}
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

func binLit(dialect string, b []byte, st *LiteralStats) string {
	h := strings.ToUpper(hex.EncodeToString(b))
	switch dialect {
	case Postgres:
		return "'\\x" + h + "'::bytea"
	case MSSQL:
		return "0x" + h
	case Oracle:
		if len(b) > oracleMaxRaw {
			if st != nil {
				st.TooLarge++
			}
			return "NULL"
		}
		return "HEXTORAW('" + h + "')"
	case BigQuery:
		return "FROM_BASE64('" + base64.StdEncoding.EncodeToString(b) + "')"
	}
	return "X'" + h + "'"
}

// geoLit rebuilds a geometry from its GeoJSON (and WKT when the engine only
// reads WKT), keeping the SRID.
func geoLit(dialect, typ string, g any, w string, srid int, st *LiteralStats) string {
	gj := compactJSON(g)
	if w == "" && (dialect == MSSQL || dialect == Oracle) {
		var gt geojson.Geometry
		if err := json.Unmarshal([]byte(gj), &gt); err == nil {
			if t, err := gt.Decode(); err == nil {
				w, _ = wkt.Marshal(t)
			}
		}
	}
	s := strconv.Itoa(srid)
	switch dialect {
	case MySQL:
		return "ST_GeomFromGeoJSON(" + stringLit(MySQL, gj, st) + ", 1, " + s + ")"
	case Postgres:
		e := "ST_SetSRID(ST_GeomFromGeoJSON(" + stringLit(Postgres, gj, st) + "), " + s + ")"
		if strings.Contains(typ, "geography") {
			e += "::geography"
		}
		return e
	case MSSQL:
		fn := "geometry"
		if strings.Contains(typ, "geography") {
			fn = "geography"
		}
		return fn + "::STGeomFromText(" + stringLit(MSSQL, w, st) + ", " + s + ")"
	case Oracle:
		if srid == 0 {
			return "SDO_GEOMETRY(" + stringLit(Oracle, w, st) + ")"
		}
		return "SDO_GEOMETRY(" + stringLit(Oracle, w, st) + ", " + s + ")"
	case BigQuery:
		return "ST_GEOGFROMGEOJSON(" + stringLit(BigQuery, gj, st) + ")"
	}
	return stringLit(dialect, gj, st)
}
