// Package migrate copies tables from one connection to another, on the same
// engine or across engines: it maps column types, translates keys, indexes
// and defaults, streams rows with value conversion, then verifies counts and
// contents.
package migrate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
)

// Engines are driver IDs; MariaDB differs from MySQL in a few type details.
const (
	MySQL    = "mysql"
	MariaDB  = "mariadb"
	Postgres = "postgres"
	MSSQL    = "mssql"
	Oracle   = "oracle"
	SQLite   = "sqlite"
	BigQuery = "bigquery"
	MongoDB  = "mongodb"
)

func family(engine string) string {
	if engine == MariaDB {
		return MySQL
	}
	return engine
}

// canon is an engine-neutral description of a column type.
type canon struct {
	T        string // bool int decimal float char varchar text binary varbinary blob date time datetime timestamptz interval json uuid enum set geometry array xml object objectid other
	Bits     int    // int: 8 16 24 32 64; float: 32 64
	Unsigned bool
	P, S     int // decimal precision and scale; P < 0 when unbounded
	N        int // char/binary length; < 0 when unbounded
	Frac     int // fractional second digits; < 0 when unspecified
	Values   []string
	Geo      string // geometry subtype: point, polygon…
	SRID     int
	Geog     bool // geography (round earth) rather than planar geometry
	JSONB    bool
	Note     string // how the source type was read, when it was approximated
}

var paramsRe = regexp.MustCompile(`\(\s*(\d+)\s*(?:,\s*(-?\d+)\s*)?`)

func typeParams(t string) (int, int) {
	m := paramsRe.FindStringSubmatch(t)
	if m == nil {
		return -1, -1
	}
	a, _ := strconv.Atoi(m[1])
	b := -1
	if m[2] != "" {
		b, _ = strconv.Atoi(m[2])
	}
	return a, b
}

func iptr(p *int64) int {
	if p == nil {
		return -1
	}
	return int(*p)
}

// canonOf reads a source column's type.
func canonOf(engine string, c driver.Column) canon {
	base := strings.ToLower(strings.TrimSpace(c.BaseType))
	typ := strings.ToLower(strings.TrimSpace(c.Type))
	p1, p2 := typeParams(typ)
	k := canon{P: -1, S: -1, N: -1, Frac: -1, SRID: c.SRID, Geo: strings.ToLower(c.GeometryType), Values: c.Enum, Unsigned: c.Unsigned}
	length := func() int {
		if n := iptr(c.Length); n > 0 {
			return n
		}
		return p1
	}
	prec := func() (int, int) {
		if p := iptr(c.Precision); p > 0 {
			s := iptr(c.Scale)
			if s < 0 {
				s = 0
			}
			return p, s
		}
		if p1 > 0 {
			return p1, max(p2, 0)
		}
		return -1, -1
	}
	frac := func() int {
		if s := iptr(c.Scale); s >= 0 && (c.Kind == driver.KindTime || c.Kind == driver.KindDateTime || c.Kind == driver.KindTimestamp) {
			return s
		}
		return p1
	}
	integer := func(bits int) canon { k.T, k.Bits = "int", bits; return k }
	switch family(engine) {
	case MySQL:
		switch base {
		case "tinyint":
			if typ == "tinyint(1)" || c.Kind == driver.KindBool {
				k.T = "bool"
				return k
			}
			return integer(8)
		case "smallint":
			return integer(16)
		case "mediumint":
			return integer(24)
		case "int", "integer":
			return integer(32)
		case "bigint":
			return integer(64)
		case "bit":
			if p1 <= 1 {
				k.T = "bool"
				return k
			}
			k.Note = "BIT(" + strconv.Itoa(p1) + ") is copied as a number"
			return integer(64)
		case "year":
			k.Note = "YEAR is copied as a number"
			return integer(16)
		case "decimal", "numeric":
			k.T = "decimal"
			k.P, k.S = prec()
			return k
		case "float":
			k.T, k.Bits = "float", 32
			return k
		case "double", "real":
			k.T, k.Bits = "float", 64
			return k
		case "char":
			k.T, k.N = "char", length()
			return k
		case "varchar":
			k.T, k.N = "varchar", length()
			return k
		case "tinytext":
			k.T, k.N = "varchar", 255
			return k
		case "text", "mediumtext", "longtext":
			k.T = "text"
			return k
		case "binary":
			k.T, k.N = "binary", length()
			return k
		case "varbinary":
			k.T, k.N = "varbinary", length()
			return k
		case "tinyblob", "blob", "mediumblob", "longblob":
			k.T = "blob"
			return k
		case "date":
			k.T = "date"
			return k
		case "time":
			k.T, k.Frac = "time", max(p1, 0)
			return k
		case "datetime", "timestamp":
			// TIMESTAMP is read in the session's time zone, so the wall
			// clock value is what the source shows.
			k.T, k.Frac = "datetime", max(p1, 0)
			return k
		case "json":
			k.T = "json"
			return k
		case "enum":
			k.T = "enum"
			return k
		case "set":
			k.T = "set"
			return k
		case "geometry", "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon", "geometrycollection", "geomcollection":
			k.T = "geometry"
			if k.Geo == "" && base != "geometry" {
				k.Geo = base
			}
			return k
		}
	case Postgres:
		if strings.HasPrefix(base, "_") || strings.HasSuffix(typ, "[]") {
			k.T = "array"
			return k
		}
		switch base {
		case "int2", "smallint":
			return integer(16)
		case "int4", "integer", "serial":
			return integer(32)
		case "int8", "bigint", "bigserial", "oid":
			return integer(64)
		case "numeric", "decimal":
			k.T = "decimal"
			k.P, k.S = p1, max(p2, 0)
			if p1 < 0 {
				k.S = -1
			}
			return k
		case "money":
			k.T, k.P, k.S = "decimal", 19, 2
			k.Note = "money is copied as a decimal"
			return k
		case "float4", "real":
			k.T, k.Bits = "float", 32
			return k
		case "float8", "double precision":
			k.T, k.Bits = "float", 64
			return k
		case "bool", "boolean":
			k.T = "bool"
			return k
		case "bpchar", "char", "character":
			k.T, k.N = "char", p1
			return k
		case "varchar", "character varying":
			k.T, k.N = "varchar", p1
			return k
		case "text", "citext", "name":
			k.T = "text"
			return k
		case "bytea":
			k.T = "blob"
			return k
		case "date":
			k.T = "date"
			return k
		case "time", "timetz":
			k.T, k.Frac = "time", p1
			if base == "timetz" {
				k.Note = "the time zone of time-of-day values is dropped"
			}
			return k
		case "timestamp":
			k.T, k.Frac = "datetime", p1
			return k
		case "timestamptz":
			k.T, k.Frac = "timestamptz", p1
			return k
		case "interval":
			k.T = "interval"
			return k
		case "json", "jsonb":
			k.T, k.JSONB = "json", base == "jsonb"
			return k
		case "uuid":
			k.T = "uuid"
			return k
		case "xml":
			k.T = "xml"
			return k
		case "inet", "cidr":
			k.T, k.N = "varchar", 43
			return k
		case "macaddr", "macaddr8":
			k.T, k.N = "varchar", 23
			return k
		case "bit", "varbit":
			if p1 == 1 && base == "bit" {
				k.T = "bool"
				return k
			}
			k.T, k.N = "varchar", max(p1, 64)
			return k
		case "geometry", "geography":
			k.T, k.Geog = "geometry", base == "geography"
			return k
		case "tsvector", "tsquery":
			k.T, k.Note = "text", "full-text search vectors are copied as text"
			return k
		case "hstore":
			k.T, k.Note = "text", "hstore is copied as text"
			return k
		}
		if c.Kind == driver.KindEnum {
			k.T = "enum"
			return k
		}
	case MSSQL:
		switch base {
		case "tinyint":
			k.Unsigned = true // 0 to 255
			return integer(8)
		case "smallint":
			return integer(16)
		case "int":
			return integer(32)
		case "bigint":
			return integer(64)
		case "decimal", "numeric":
			k.T = "decimal"
			k.P, k.S = prec()
			return k
		case "money":
			k.T, k.P, k.S = "decimal", 19, 4
			return k
		case "smallmoney":
			k.T, k.P, k.S = "decimal", 10, 4
			return k
		case "float":
			k.T, k.Bits = "float", 64
			if p1 > 0 && p1 <= 24 {
				k.Bits = 32
			}
			return k
		case "real":
			k.T, k.Bits = "float", 32
			return k
		case "bit":
			k.T = "bool"
			return k
		case "char", "nchar":
			k.T, k.N = "char", length()
			return k
		case "varchar", "nvarchar":
			if strings.Contains(typ, "max") || c.Kind == driver.KindText {
				k.T = "text"
				return k
			}
			k.T, k.N = "varchar", length()
			return k
		case "text", "ntext":
			k.T = "text"
			return k
		case "binary":
			k.T, k.N = "binary", length()
			return k
		case "varbinary":
			if strings.Contains(typ, "max") || c.Length == nil && p1 < 0 {
				k.T = "blob"
				return k
			}
			k.T, k.N = "varbinary", length()
			return k
		case "image":
			k.T = "blob"
			return k
		case "date":
			k.T = "date"
			return k
		case "time":
			k.T, k.Frac = "time", frac()
			return k
		case "datetime":
			k.T, k.Frac = "datetime", 3
			return k
		case "smalldatetime":
			k.T, k.Frac = "datetime", 0
			return k
		case "datetime2":
			k.T, k.Frac = "datetime", frac()
			return k
		case "datetimeoffset":
			k.T, k.Frac = "timestamptz", frac()
			return k
		case "uniqueidentifier":
			k.T = "uuid"
			return k
		case "xml":
			k.T = "xml"
			return k
		case "geometry", "geography":
			k.T, k.Geog = "geometry", base == "geography"
			if k.Geo == base {
				k.Geo = ""
			}
			return k
		case "hierarchyid":
			k.T, k.N, k.Note = "varchar", 4000, "hierarchyid is copied as its text path"
			return k
		case "rowversion", "timestamp":
			k.T, k.N, k.Note = "binary", 8, "rowversion values are copied as plain binary"
			return k
		case "sql_variant":
			k.T, k.Note = "text", "sql_variant is copied as text"
			return k
		}
	case Oracle:
		switch base {
		case "number":
			p, s := prec()
			if p > 0 && s == 0 {
				switch {
				case p <= 2:
					return integer(8)
				case p <= 4:
					return integer(16)
				case p <= 9:
					return integer(32)
				case p <= 18:
					return integer(64)
				}
			}
			k.T, k.P, k.S = "decimal", p, s
			return k
		case "float":
			k.T, k.Bits = "float", 64
			return k
		case "binary_float":
			k.T, k.Bits = "float", 32
			return k
		case "binary_double":
			k.T, k.Bits = "float", 64
			return k
		case "char", "nchar":
			k.T, k.N = "char", length()
			return k
		case "varchar2", "nvarchar2", "varchar":
			k.T, k.N = "varchar", length()
			return k
		case "clob", "nclob", "long":
			k.T = "text"
			return k
		case "raw":
			k.T, k.N = "varbinary", length()
			return k
		case "blob", "long raw", "bfile":
			k.T = "blob"
			return k
		case "date":
			k.T, k.Frac = "datetime", 0
			return k
		case "boolean":
			k.T = "bool"
			return k
		case "json":
			k.T = "json"
			return k
		case "xmltype":
			k.T = "xml"
			return k
		case "rowid", "urowid":
			k.T, k.N = "varchar", 18
			return k
		case "sdo_geometry":
			k.T = "geometry"
			return k
		}
		switch {
		case strings.HasPrefix(base, "timestamp") && strings.Contains(base, "time zone"):
			k.T, k.Frac = "timestamptz", p1
			return k
		case strings.HasPrefix(base, "timestamp"):
			k.T, k.Frac = "datetime", p1
			return k
		case strings.HasPrefix(base, "interval"):
			k.T = "interval"
			return k
		}
	case SQLite:
		switch base {
		case "integer", "int", "bigint", "smallint", "tinyint", "mediumint", "int8", "int2":
			return integer(64)
		case "boolean", "bool":
			k.T = "bool"
			return k
		case "real", "double", "float", "double precision":
			k.T, k.Bits = "float", 64
			return k
		case "numeric", "decimal":
			k.T, k.P, k.S = "decimal", p1, max(p2, 0)
			if p1 < 0 {
				k.S = -1
			}
			return k
		case "text", "clob":
			k.T = "text"
			return k
		case "varchar", "char", "nvarchar", "nchar", "character", "varying character", "native character":
			k.T, k.N = "varchar", p1
			return k
		case "blob":
			k.T = "blob"
			return k
		case "date":
			k.T = "date"
			return k
		case "time":
			k.T = "time"
			return k
		case "datetime", "timestamp":
			k.T = "datetime"
			return k
		case "json":
			k.T = "json"
			return k
		}
	case BigQuery:
		switch base {
		case "int64", "integer", "int", "smallint", "bigint", "tinyint", "byteint":
			return integer(64)
		case "numeric", "decimal":
			k.T, k.P, k.S = "decimal", 38, 9
			return k
		case "bignumeric", "bigdecimal":
			k.T, k.P, k.S = "decimal", 76, 38
			return k
		case "float64", "float":
			k.T, k.Bits = "float", 64
			return k
		case "bool", "boolean":
			k.T = "bool"
			return k
		case "string":
			k.T = "text"
			return k
		case "bytes":
			k.T = "blob"
			return k
		case "date":
			k.T = "date"
			return k
		case "datetime":
			k.T, k.Frac = "datetime", 6
			return k
		case "time":
			k.T, k.Frac = "time", 6
			return k
		case "timestamp":
			k.T, k.Frac = "timestamptz", 6
			return k
		case "geography":
			k.T, k.Geog, k.SRID = "geometry", true, 4326
			k.Geo = ""
			return k
		case "json":
			k.T = "json"
			return k
		case "interval":
			k.T = "interval"
			return k
		case "array", "struct", "record", "range":
			k.T = "object"
			return k
		}
	case MongoDB:
		switch base {
		case "objectid":
			k.T = "objectid"
			return k
		case "string":
			k.T = "text"
			return k
		case "int":
			return integer(32)
		case "long":
			return integer(64)
		case "double":
			k.T, k.Bits = "float", 64
			return k
		case "decimal":
			k.T = "decimal"
			return k
		case "bool":
			k.T = "bool"
			return k
		case "date", "timestamp":
			k.T, k.Frac = "timestamptz", 3
			return k
		case "bindata":
			k.T = "blob"
			return k
		case "geojson":
			k.T, k.SRID = "geometry", 4326
			return k
		case "object", "array":
			k.T = "object"
			return k
		}
		k.T, k.Note = "object", "fields with mixed types are copied as JSON"
		return k
	}
	return canonOfKind(k, c.Kind)
}

func canonOfKind(k canon, kind driver.ValueKind) canon {
	switch kind {
	case driver.KindInt:
		k.T, k.Bits = "int", 64
	case driver.KindFloat:
		k.T, k.Bits = "float", 64
	case driver.KindDecimal:
		k.T = "decimal"
	case driver.KindBool:
		k.T = "bool"
	case driver.KindString, driver.KindText:
		k.T = "text"
	case driver.KindBinary:
		k.T = "blob"
	case driver.KindDate:
		k.T = "date"
	case driver.KindTime:
		k.T = "time"
	case driver.KindDateTime:
		k.T = "datetime"
	case driver.KindTimestamp:
		k.T = "timestamptz"
	case driver.KindInterval:
		k.T = "interval"
	case driver.KindJSON:
		k.T = "json"
	case driver.KindUUID:
		k.T = "uuid"
	case driver.KindGeometry:
		k.T = "geometry"
	case driver.KindEnum:
		k.T = "enum"
	case driver.KindArray, driver.KindObject:
		k.T = "object"
	default:
		k.T, k.Note = "text", "this type is copied as text"
	}
	return k
}

// kindOf is the value kind a target column of this type holds.
func (k canon) kind() driver.ValueKind {
	switch k.T {
	case "bool":
		return driver.KindBool
	case "int":
		return driver.KindInt
	case "decimal":
		return driver.KindDecimal
	case "float":
		return driver.KindFloat
	case "char", "varchar", "set", "objectid":
		return driver.KindString
	case "text", "xml":
		return driver.KindText
	case "binary", "varbinary", "blob":
		return driver.KindBinary
	case "date":
		return driver.KindDate
	case "time":
		return driver.KindTime
	case "datetime":
		return driver.KindDateTime
	case "timestamptz":
		return driver.KindTimestamp
	case "interval":
		return driver.KindInterval
	case "json":
		return driver.KindJSON
	case "uuid":
		return driver.KindUUID
	case "enum":
		return driver.KindEnum
	case "geometry":
		return driver.KindGeometry
	case "array", "object":
		return driver.KindJSON
	}
	return driver.KindOther
}

// target describes the destination for type choices.
type target struct {
	Engine  string
	Version int  // major version, where types depend on it (Oracle)
	PostGIS bool // PostgreSQL target has PostGIS
}

// use says how a column is used, which limits some type choices.
type use struct {
	Indexed bool // in the primary key, a unique key or an index
}

// mapped is the chosen target type and what the reader should know.
type mapped struct {
	Type  string
	Canon canon // what the target type holds
	Notes []string
	Lossy bool // values may not survive exactly
}

func (m *mapped) note(lossy bool, format string, args ...any) {
	m.Notes = append(m.Notes, fmt.Sprintf(format, args...))
	m.Lossy = m.Lossy || lossy
}

func quoteValues(vals []string, q func(string) string) string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = q(v)
	}
	return strings.Join(out, ",")
}

func sqlString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func maxLen(vals []string) int {
	n := 1
	for _, v := range vals {
		n = max(n, len([]rune(v)))
	}
	return n
}

// typeFor chooses the target type for a source column type.
func typeFor(t target, k canon, u use) mapped {
	m := mapped{Canon: k}
	if k.Note != "" {
		m.note(false, "%s", k.Note)
	}
	switch family(t.Engine) {
	case MySQL:
		mysqlType(t, k, u, &m)
	case Postgres:
		postgresType(t, k, u, &m)
	case MSSQL:
		mssqlType(k, u, &m)
	case Oracle:
		oracleType(t, k, u, &m)
	case SQLite:
		sqliteType(k, &m)
	case MongoDB:
		m.Type = mongoType(k)
		switch {
		case (k.T == "datetime" || k.T == "timestamptz") && (k.Frac < 0 || k.Frac > 3):
			m.note(true, "MongoDB dates keep milliseconds; finer fractions are rounded")
		case k.T == "datetime":
			m.note(false, "stored as a UTC date")
		case k.T == "decimal" && k.P > 34:
			m.note(true, "MongoDB decimals hold 34 digits")
		case k.T == "time" || k.T == "interval":
			m.note(false, "stored as text")
		}
	default:
		m.Type = "TEXT"
	}
	return m
}

func geoName(k canon) string {
	switch k.Geo {
	case "point", "linestring", "polygon", "multipoint", "multilinestring", "multipolygon":
		return k.Geo
	case "geometrycollection", "geomcollection":
		return "geometrycollection"
	}
	return "geometry"
}

func mysqlType(t target, k canon, u use, m *mapped) {
	uns := ""
	if k.Unsigned {
		uns = " unsigned"
	}
	switch k.T {
	case "bool":
		m.Type = "tinyint(1)"
	case "int":
		m.Type = map[int]string{8: "tinyint", 16: "smallint", 24: "mediumint", 32: "int", 64: "bigint"}[k.Bits] + uns
		if k.Bits == 0 {
			m.Type = "bigint" + uns
		}
	case "decimal":
		switch {
		case k.P < 0:
			m.Type = "decimal(65,30)"
			m.note(false, "the source has no fixed precision; stored with 65 digits, 30 after the point")
		case k.P > 65 || k.S > 30:
			m.Type = fmt.Sprintf("decimal(%d,%d)", min(k.P, 65), min(max(k.S, 0), 30))
			m.note(true, "MySQL decimals hold at most 65 digits, 30 after the point")
		default:
			m.Type = fmt.Sprintf("decimal(%d,%d)", k.P, max(k.S, 0))
		}
	case "float":
		m.Type = map[bool]string{true: "float", false: "double"}[k.Bits == 32]
	case "char":
		switch {
		case k.N <= 0:
			m.Type = "char(1)"
		case k.N > 255:
			m.Type = fmt.Sprintf("varchar(%d)", k.N)
		default:
			m.Type = fmt.Sprintf("char(%d)", k.N)
		}
	case "varchar", "text", "xml", "set", "interval":
		n := k.N
		if k.T != "varchar" {
			n = -1
		}
		switch {
		case u.Indexed && (n < 0 || n > 768):
			m.Type = "varchar(768)"
			m.note(true, "shortened to 768 characters so it can be indexed")
		case n < 0 && k.T == "interval":
			m.Type = "varchar(64)"
			m.note(false, "intervals are copied as text")
		case n < 0:
			m.Type = "longtext"
		case n > 16383:
			m.Type = "mediumtext"
		default:
			m.Type = fmt.Sprintf("varchar(%d)", n)
		}
		if k.T == "set" && len(k.Values) > 0 {
			m.Type = "set(" + quoteValues(k.Values, sqlString) + ")"
		}
	case "binary":
		if k.N > 0 && k.N <= 255 {
			m.Type = fmt.Sprintf("binary(%d)", k.N)
		} else {
			m.Type = "longblob"
		}
	case "varbinary", "blob":
		switch {
		case k.T == "varbinary" && k.N > 0 && k.N <= 65535:
			m.Type = fmt.Sprintf("varbinary(%d)", k.N)
		case u.Indexed:
			m.Type = "varbinary(3072)"
			m.note(true, "limited to 3,072 bytes so it can be indexed")
		default:
			m.Type = "longblob"
		}
	case "date":
		m.Type = "date"
	case "time":
		m.Type = fracType("time", k.Frac, 6)
	case "datetime":
		m.Type = fracType("datetime", k.Frac, 6)
	case "timestamptz":
		m.Type = fracType("datetime", k.Frac, 6)
		m.note(false, "MySQL has no time zone type; values are stored in UTC")
	case "json", "object", "array":
		m.Type = "json"
		if t.Engine == MariaDB {
			m.Type = "longtext"
		}
	case "uuid":
		m.Type = "char(36)"
	case "objectid":
		m.Type = "char(24)"
	case "enum":
		if len(k.Values) > 0 {
			m.Type = "enum(" + quoteValues(k.Values, sqlString) + ")"
		} else {
			m.Type = "varchar(255)"
		}
	case "geometry":
		m.Type = geoName(k)
		if k.SRID > 0 {
			if t.Engine == MariaDB {
				m.Type += fmt.Sprintf(" REF_SYSTEM_ID=%d", k.SRID)
			} else {
				m.Type += fmt.Sprintf(" SRID %d", k.SRID)
			}
		}
		if k.Geog {
			m.note(false, "geography is stored as geometry in SRID %d", max(k.SRID, 4326))
		}
	default:
		m.Type = "longtext"
	}
}

func fracType(name string, frac, maxFrac int) string {
	if frac < 0 {
		frac = maxFrac
	}
	if frac == 0 {
		return name
	}
	return fmt.Sprintf("%s(%d)", name, min(frac, maxFrac))
}

var postgisNames = map[string]string{"point": "Point", "linestring": "LineString", "polygon": "Polygon", "multipoint": "MultiPoint",
	"multilinestring": "MultiLineString", "multipolygon": "MultiPolygon", "geometrycollection": "GeometryCollection", "geometry": "Geometry"}

func postgresType(t target, k canon, u use, m *mapped) {
	switch k.T {
	case "bool":
		m.Type = "boolean"
	case "int":
		switch {
		case k.Bits <= 16 && !(k.Unsigned && k.Bits == 16):
			m.Type = "smallint"
		case k.Bits <= 32 && !(k.Unsigned && k.Bits == 32):
			m.Type = "integer"
		case k.Bits <= 64 && !(k.Unsigned && k.Bits == 64):
			m.Type = "bigint"
		default:
			m.Type = "numeric(20,0)"
		}
	case "decimal":
		if k.P < 0 {
			m.Type = "numeric"
		} else {
			m.Type = fmt.Sprintf("numeric(%d,%d)", k.P, max(k.S, 0))
		}
	case "float":
		m.Type = map[bool]string{true: "real", false: "double precision"}[k.Bits == 32]
	case "char":
		m.Type = fmt.Sprintf("character(%d)", max(k.N, 1))
	case "varchar":
		if k.N <= 0 || k.N > 10485760 {
			m.Type = "text"
		} else {
			m.Type = fmt.Sprintf("character varying(%d)", k.N)
		}
	case "text", "set":
		m.Type = "text"
	case "binary", "varbinary", "blob":
		m.Type = "bytea"
	case "date":
		m.Type = "date"
	case "time":
		m.Type = fracType("time", k.Frac, 6)
	case "datetime":
		m.Type = fracType("timestamp", k.Frac, 6)
	case "timestamptz":
		m.Type = fracType("timestamptz", k.Frac, 6)
	case "interval":
		m.Type = "interval"
	case "json":
		m.Type = map[bool]string{true: "jsonb", false: "json"}[k.JSONB]
	case "object", "array":
		m.Type = "jsonb"
	case "uuid":
		m.Type = "uuid"
	case "objectid":
		m.Type = "character varying(24)"
	case "xml":
		m.Type = "xml"
	case "enum":
		m.Type = fmt.Sprintf("character varying(%d)", maxLen(k.Values))
		m.note(false, "copied as text, with a check that allows only the listed values")
	case "geometry":
		if !t.PostGIS {
			m.Type = "jsonb"
			m.note(true, "PostGIS is not installed on the target, so shapes are stored as GeoJSON")
			return
		}
		sub := postgisNames[geoName(k)]
		name := "geometry"
		if k.Geog {
			name = "geography"
		}
		switch {
		case k.SRID > 0:
			m.Type = fmt.Sprintf("%s(%s,%d)", name, sub, k.SRID)
		case sub != "Geometry":
			m.Type = fmt.Sprintf("%s(%s)", name, sub)
		default:
			m.Type = name
		}
	default:
		m.Type = "text"
	}
}

func mssqlType(k canon, u use, m *mapped) {
	switch k.T {
	case "bool":
		m.Type = "bit"
	case "int":
		switch {
		case k.Bits <= 8 && k.Unsigned:
			m.Type = "tinyint"
		case k.Bits <= 16 && !(k.Unsigned && k.Bits == 16):
			m.Type = "smallint"
		case k.Bits <= 32 && !(k.Unsigned && k.Bits == 32):
			m.Type = "int"
		case k.Bits <= 64 && !(k.Unsigned && k.Bits == 64):
			m.Type = "bigint"
		default:
			m.Type = "decimal(20,0)"
		}
	case "decimal":
		switch {
		case k.P < 0:
			m.Type = "decimal(38,10)"
			m.note(true, "the source has no fixed precision; stored with 38 digits, 10 after the point")
		case k.P > 38:
			m.Type = fmt.Sprintf("decimal(38,%d)", min(max(k.S, 0), 38))
			m.note(true, "SQL Server decimals hold at most 38 digits")
		default:
			m.Type = fmt.Sprintf("decimal(%d,%d)", k.P, max(k.S, 0))
		}
	case "float":
		m.Type = map[bool]string{true: "real", false: "float"}[k.Bits == 32]
	case "char":
		if k.N > 0 && k.N <= 4000 {
			m.Type = fmt.Sprintf("nchar(%d)", k.N)
		} else {
			m.Type = "nvarchar(max)"
		}
	case "varchar", "text", "xml", "set", "interval", "enum":
		n := k.N
		if k.T != "varchar" {
			n = -1
		}
		switch {
		case k.T == "xml":
			m.Type = "xml"
		case k.T == "enum":
			m.Type = fmt.Sprintf("nvarchar(%d)", maxLen(k.Values))
			m.note(false, "copied as text, with a check that allows only the listed values")
		case k.T == "interval":
			m.Type = "nvarchar(64)"
			m.note(false, "intervals are copied as text")
		case u.Indexed && (n < 0 || n > 450):
			m.Type = "nvarchar(450)"
			m.note(true, "shortened to 450 characters so it can be indexed")
		case n < 0 || n > 4000:
			m.Type = "nvarchar(max)"
		default:
			m.Type = fmt.Sprintf("nvarchar(%d)", n)
		}
	case "binary":
		if k.N > 0 && k.N <= 8000 {
			m.Type = fmt.Sprintf("binary(%d)", k.N)
		} else {
			m.Type = "varbinary(max)"
		}
	case "varbinary", "blob":
		switch {
		case k.T == "varbinary" && k.N > 0 && k.N <= 8000:
			m.Type = fmt.Sprintf("varbinary(%d)", k.N)
		case u.Indexed:
			m.Type = "varbinary(900)"
			m.note(true, "limited to 900 bytes so it can be indexed")
		default:
			m.Type = "varbinary(max)"
		}
	case "date":
		m.Type = "date"
	case "time":
		m.Type = fracType("time", k.Frac, 7)
	case "datetime":
		m.Type = fracType("datetime2", k.Frac, 7)
	case "timestamptz":
		m.Type = fracType("datetimeoffset", k.Frac, 7)
	case "json", "object", "array":
		m.Type = "nvarchar(max)"
		m.note(false, "JSON is stored as text")
	case "uuid":
		m.Type = "uniqueidentifier"
	case "objectid":
		m.Type = "nchar(24)"
	case "geometry":
		m.Type = map[bool]string{true: "geography", false: "geometry"}[k.Geog]
	default:
		m.Type = "nvarchar(max)"
	}
}

func oracleType(t target, k canon, u use, m *mapped) {
	switch k.T {
	case "bool":
		if t.Version >= 23 {
			m.Type = "BOOLEAN"
		} else {
			m.Type = "NUMBER(1)"
		}
	case "int":
		digits := map[int]int{8: 3, 16: 5, 24: 7, 32: 10, 64: 19}[k.Bits]
		if digits == 0 {
			digits = 19
		}
		if k.Unsigned && k.Bits == 64 {
			digits = 20
		}
		m.Type = fmt.Sprintf("NUMBER(%d)", digits)
	case "decimal":
		switch {
		case k.P < 0:
			m.Type = "NUMBER"
		case k.P > 38:
			m.Type = "NUMBER"
			m.note(true, "Oracle numbers hold at most 38 significant digits")
		default:
			m.Type = fmt.Sprintf("NUMBER(%d,%d)", k.P, max(k.S, 0))
		}
	case "float":
		m.Type = map[bool]string{true: "BINARY_FLOAT", false: "BINARY_DOUBLE"}[k.Bits == 32]
	case "char":
		if k.N > 0 && k.N <= 2000 {
			m.Type = fmt.Sprintf("CHAR(%d CHAR)", k.N)
		} else {
			m.Type = "CLOB"
		}
	case "varchar", "text", "xml", "set", "interval", "enum", "uuid", "objectid":
		n := k.N
		switch k.T {
		case "uuid":
			n = 36
		case "objectid":
			n = 24
		case "enum":
			n = maxLen(k.Values)
			m.note(false, "copied as text, with a check that allows only the listed values")
		case "interval":
			n = 64
			m.note(false, "intervals are copied as text")
		case "varchar":
		default:
			n = -1
		}
		switch {
		case u.Indexed && (n < 0 || n > 1000):
			m.Type = "VARCHAR2(1000 CHAR)"
			m.note(true, "shortened to 1,000 characters so it can be indexed")
		case n < 0 || n > 4000:
			m.Type = "CLOB"
		default:
			m.Type = fmt.Sprintf("VARCHAR2(%d CHAR)", n)
		}
	case "binary", "varbinary", "blob":
		if k.T != "blob" && k.N > 0 && k.N <= 2000 {
			m.Type = fmt.Sprintf("RAW(%d)", k.N)
		} else {
			m.Type = "BLOB"
		}
	case "date":
		m.Type = "DATE"
	case "time":
		m.Type = "VARCHAR2(18)"
		m.note(false, "Oracle has no time-of-day type; stored as text")
	case "datetime":
		if k.Frac == 0 {
			m.Type = "DATE"
		} else {
			m.Type = fracType("TIMESTAMP", k.Frac, 9)
		}
	case "timestamptz":
		m.Type = fracType("TIMESTAMP", k.Frac, 9) + " WITH TIME ZONE"
		if k.Frac == 0 {
			m.Type = "TIMESTAMP(0) WITH TIME ZONE"
		}
	case "json", "object", "array":
		if t.Version >= 21 {
			m.Type = "JSON"
		} else {
			m.Type = "CLOB"
			m.note(false, "JSON is stored as text")
		}
	case "geometry":
		m.Type = "CLOB"
		m.note(true, "shapes are stored as GeoJSON text")
	default:
		m.Type = "CLOB"
	}
}

func sqliteType(k canon, m *mapped) {
	switch k.T {
	case "bool":
		m.Type = "BOOLEAN"
	case "int":
		m.Type = "INTEGER"
		if k.Unsigned && k.Bits == 64 {
			m.Type = "NUMERIC"
			m.note(true, "SQLite integers are signed 64-bit; larger values are stored as numbers that may lose precision")
		}
	case "decimal":
		if k.P > 0 {
			m.Type = fmt.Sprintf("DECIMAL(%d,%d)", k.P, max(k.S, 0))
		} else {
			m.Type = "NUMERIC"
		}
		m.note(true, "SQLite stores decimals as floating point when they are not whole numbers")
	case "float":
		m.Type = "REAL"
	case "char", "varchar":
		if k.N > 0 {
			m.Type = fmt.Sprintf("VARCHAR(%d)", k.N)
		} else {
			m.Type = "TEXT"
		}
	case "binary", "varbinary", "blob":
		m.Type = "BLOB"
	case "date":
		m.Type = "DATE"
	case "time":
		m.Type = "TIME"
	case "datetime":
		m.Type = "DATETIME"
	case "timestamptz":
		m.Type = "DATETIME"
		m.note(false, "SQLite has no time zone type; values are stored in UTC")
	case "geometry":
		m.Type = "TEXT"
		m.note(false, "shapes are stored as GeoJSON text")
	case "enum":
		m.Type = "TEXT"
		m.note(false, "with a check that allows only the listed values")
	default:
		m.Type = "TEXT"
	}
}

// mongoType is informational: collections have no column types.
func mongoType(k canon) string {
	switch k.T {
	case "bool":
		return "bool"
	case "int":
		if k.Bits <= 32 && !k.Unsigned {
			return "int"
		}
		return "long"
	case "decimal":
		return "decimal"
	case "float":
		return "double"
	case "binary", "varbinary", "blob":
		return "binData"
	case "date", "datetime", "timestamptz":
		return "date"
	case "json", "object", "array":
		return "object"
	case "objectid":
		return "objectId"
	case "geometry":
		return "geojson"
	}
	return "string"
}
