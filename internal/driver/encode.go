package driver

import (
	"encoding/base64"
	"encoding/hex"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Cell encoding shared by every engine. JSON-native values are sent as-is;
// values JSON cannot carry faithfully are wrapped:
//
//	integers beyond ±2^53     -> string            (column kind tells the UI it is numeric)
//	decimals                  -> string            (exact)
//	binary                    -> {"$bin": base64 preview, "size": n, "mime": "..."}
//	very long text            -> {"$text": prefix, "size": n}
//	geometry                  -> {"$geo": GeoJSON, "wkt": "...", "srid": n}
//	NaN / ±Inf                -> string
const (
	MaxBinaryPreview = 64 << 10  // bytes of a binary cell sent inline
	MaxTextCell      = 256 << 10 // characters of a text cell sent inline
	maxSafeInt       = 1<<53 - 1
)

func EncodeInt(i int64) any {
	if i > maxSafeInt || i < -maxSafeInt {
		return strconv.FormatInt(i, 10)
	}
	return i
}

func EncodeUint(u uint64) any {
	if u > maxSafeInt {
		return strconv.FormatUint(u, 10)
	}
	return int64(u)
}

func EncodeFloat(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	return f
}

func EncodeText(s string) any {
	if len(s) <= MaxTextCell {
		if !utf8.ValidString(s) {
			return strings.ToValidUTF8(s, "�")
		}
		return s
	}
	cut := s[:MaxTextCell]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return map[string]any{"$text": cut, "size": len(s)}
}

func EncodeBinary(b []byte) any {
	if b == nil {
		return nil
	}
	preview := b
	if len(preview) > MaxBinaryPreview {
		preview = preview[:MaxBinaryPreview]
	}
	out := map[string]any{"$bin": base64.StdEncoding.EncodeToString(preview), "size": len(b)}
	if mime := http.DetectContentType(b); mime != "application/octet-stream" && !strings.HasPrefix(mime, "text/plain") {
		out["mime"] = mime
	}
	return out
}

// EncodeUUIDBytes renders a 16-byte value as a canonical UUID string.
func EncodeUUIDBytes(b []byte) any {
	if len(b) != 16 {
		return EncodeBinary(b)
	}
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func EncodeTime(t time.Time, kind ValueKind) any {
	switch kind {
	case KindDate:
		return t.Format("2006-01-02")
	case KindTime:
		return trimFrac(t.Format("15:04:05.000000"))
	case KindTimestamp:
		return trimFrac(t.Format("2006-01-02 15:04:05.000000")) + t.Format("-07:00")
	default:
		return trimFrac(t.Format("2006-01-02 15:04:05.000000"))
	}
}

func trimFrac(s string) string {
	if i := strings.LastIndexByte(s, '.'); i >= 0 {
		frac := strings.TrimRight(s[i+1:], "0")
		if frac == "" {
			return s[:i]
		}
		return s[:i+1] + frac
	}
	return s
}

// KindFromTypeName maps a declared SQL type to a value kind. Engines refine
// this with their own knowledge; it covers the common vocabulary.
func KindFromTypeName(t string) ValueKind {
	t = strings.ToLower(strings.TrimSpace(t))
	if i := strings.IndexByte(t, '('); i >= 0 {
		t = strings.TrimSpace(t[:i])
	}
	t = strings.TrimSuffix(t, " unsigned")
	if strings.HasSuffix(t, "[]") || strings.HasPrefix(t, "_") && len(t) > 1 {
		return KindArray
	}
	switch t {
	case "int", "integer", "int2", "int4", "int8", "smallint", "bigint", "tinyint", "mediumint", "serial", "bigserial",
		"smallserial", "int64", "year", "oid", "number(38)", "pls_integer", "binary_integer":
		return KindInt
	case "float", "float4", "float8", "double", "double precision", "real", "binary_float", "binary_double", "float64":
		return KindFloat
	case "decimal", "numeric", "number", "money", "smallmoney", "bignumeric", "dec", "fixed":
		return KindDecimal
	case "bool", "boolean", "bit":
		return KindBool
	case "char", "varchar", "nchar", "nvarchar", "varchar2", "nvarchar2", "character", "character varying", "string",
		"citext", "name", "bpchar", "sysname", "set", "inet", "cidr", "macaddr", "macaddr8", "ltree", "rowid", "urowid":
		return KindString
	case "text", "tinytext", "mediumtext", "longtext", "clob", "nclob", "ntext", "long", "xml", "tsvector", "tsquery":
		return KindText
	case "blob", "tinyblob", "mediumblob", "longblob", "binary", "varbinary", "bytea", "image", "raw", "long raw",
		"bfile", "bytes", "rowversion", "timestamp without time zone_bin":
		return KindBinary
	case "date":
		return KindDate
	case "time", "time without time zone", "timetz", "time with time zone":
		return KindTime
	case "datetime", "datetime2", "smalldatetime", "timestamp", "timestamp without time zone":
		return KindDateTime
	case "timestamptz", "timestamp with time zone", "datetimeoffset", "timestamp with local time zone":
		return KindTimestamp
	case "interval", "interval day to second", "interval year to month":
		return KindInterval
	case "json", "jsonb":
		return KindJSON
	case "uuid", "uniqueidentifier":
		return KindUUID
	case "geometry", "geography", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon",
		"geometrycollection", "sdo_geometry", "box2d", "box3d":
		return KindGeometry
	case "enum":
		return KindEnum
	case "array":
		return KindArray
	case "struct", "record", "object", "document":
		return KindObject
	}
	switch {
	case strings.HasPrefix(t, "timestamp") && strings.Contains(t, "time zone"):
		return KindTimestamp
	case strings.HasPrefix(t, "timestamp"):
		return KindDateTime
	case strings.HasPrefix(t, "interval"):
		return KindInterval
	case strings.Contains(t, "char"):
		return KindString
	case strings.Contains(t, "int"):
		return KindInt
	}
	return KindOther
}
