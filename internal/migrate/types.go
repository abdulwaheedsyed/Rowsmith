// Package migrate copies tables from one connection to another, on the same
// engine or across engines: it maps column types, translates keys, indexes
// and defaults, streams rows with value conversion, then verifies counts and
// contents.
package migrate

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"rowsmith/internal/driver"
	"rowsmith/internal/driver/clickhouse/chtype"
)

// Engines are driver IDs; MariaDB differs from MySQL in a few type details,
// and the servers that speak MySQL's or PostgreSQL's dialect (TiDB,
// CockroachDB, YugabyteDB) map types as their family does.
const (
	MySQL      = "mysql"
	MariaDB    = "mariadb"
	Postgres   = "postgres"
	MSSQL      = "mssql"
	Oracle     = "oracle"
	SQLite     = "sqlite"
	BigQuery   = "bigquery"
	MongoDB    = "mongodb"
	ClickHouse = "clickhouse"
	Snowflake  = "snowflake"
	Trino      = "trino"
	Cassandra  = "cassandra"
	Elastic    = "elasticsearch"
	Databricks = "databricks"
)

// compatible are the servers that speak another engine's dialect without all
// of it (spatial types, extensions, index methods, routines).
var compatible = map[string]bool{"tidb": true, "cockroachdb": true, "yugabytedb": true}

// sameEngine says whether definitions copy as they are. Between servers of
// one family that holds only for the engine itself and MySQL with MariaDB;
// the compatible servers take translated types in either direction.
func sameEngine(src, dst string) bool {
	if family(src) != family(dst) {
		return false
	}
	return src == dst || (!compatible[src] && !compatible[dst])
}

func family(engine string) string {
	switch engine {
	case MariaDB, "tidb":
		return MySQL
	case "cockroachdb", "yugabytedb":
		return Postgres
	case "scylladb":
		return Cassandra
	case "opensearch":
		return Elastic
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
	case ClickHouse:
		return clickhouseCanon(k, c)
	case Snowflake:
		return snowflakeCanon(k, c, base)
	case Trino:
		return trinoCanon(k, c, base)
	case Cassandra:
		return cassandraCanon(k, c, base)
	case Elastic:
		return elasticCanon(k, base)
	case Databricks:
		return databricksCanon(k, c, base)
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
	Spatial bool // Oracle target has Oracle Spatial (SDO_GEOMETRY)
	// Connector is the connector of a Trino target's catalog (iceberg,
	// delta_lake, hive), whose table formats take different types.
	Connector string
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
	case ClickHouse:
		clickhouseType(k, &m)
	case Snowflake:
		snowflakeType(k, &m)
	case Trino:
		trinoType(t.Connector, k, &m)
	case Cassandra:
		cassandraType(k, &m)
	case Elastic:
		elasticType(k, &m)
	case Databricks:
		databricksType(k, &m)
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
		if t.Engine == "tidb" {
			m.Type = "json"
			m.note(true, "TiDB has no spatial types, so shapes are stored as GeoJSON")
			return
		}
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
		if t.Spatial {
			m.Type = "SDO_GEOMETRY"
			return
		}
		m.Type = "CLOB"
		m.note(true, "Oracle Spatial is not installed on the target, so shapes are stored as GeoJSON text")
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

// clickhouseCanon reads a ClickHouse column type.
func clickhouseCanon(k canon, c driver.Column) canon {
	t, _ := chtype.Parse(c.Type).Unwrap()
	if t.Name == "SimpleAggregateFunction" && len(t.Args) == 1 {
		t, _ = t.Args[0].Unwrap()
	}
	if bits, unsigned, ok := t.IntBits(); ok {
		if bits > 64 {
			digits := map[int]int{128: 39, 256: 78}[bits]
			k.T, k.P, k.S, k.Note = "decimal", digits, 0, "a "+strconv.Itoa(bits)+"-bit integer is copied as a decimal"
			return k
		}
		k.T, k.Bits, k.Unsigned = "int", bits, unsigned
		return k
	}
	switch {
	case t.IsDecimal():
		k.T = "decimal"
		k.P, k.S = t.DecimalPS()
		return k
	case t.GeoShape() != "":
		k.T, k.Geo = "geometry", t.GeoShape()
		return k
	case strings.HasPrefix(t.Name, "Interval"):
		k.T = "interval"
		return k
	}
	switch t.Name {
	case "Float32":
		k.T, k.Bits = "float", 32
	case "Float64", "BFloat16":
		k.T, k.Bits = "float", 64
	case "Bool":
		k.T = "bool"
	case "String":
		k.T = "text"
	case "FixedString":
		// Often codes, often hashes: as text, binary values come out as 0x… hex.
		k.T, k.Note = "text", "a fixed-length string is copied as text; bytes that aren't text become 0x… hex"
	case "UUID":
		k.T = "uuid"
	case "IPv4", "IPv6":
		k.T, k.N = "varchar", 45
	case "Date", "Date32":
		k.T = "date"
	case "DateTime", "DateTime64":
		// An instant shown in the column's time zone: copied as it shows.
		k.T, k.Frac = "datetime", 0
		if t.Name == "DateTime64" && len(t.Params) > 0 {
			k.Frac, _ = strconv.Atoi(strings.TrimSpace(t.Params[0]))
		}
		if len(t.Params) > 0 && strings.HasPrefix(strings.TrimSpace(t.Params[len(t.Params)-1]), "'") {
			k.Note = "times are copied as shown in the column's time zone, " + t.Params[len(t.Params)-1]
		}
	case "Time", "Time64":
		k.T = "time"
	case "Enum8", "Enum16":
		k.T, k.Values = "enum", t.EnumValues()
	case "Array", "Map", "Tuple", "Nested":
		k.T = "object"
	case "JSON", "Object":
		k.T = "json"
	case "AggregateFunction":
		k.T, k.Note = "blob", "aggregate function states are copied as bytes"
	default:
		k.T, k.Note = "text", "this type is copied as text"
	}
	return k
}

// clickhouseType chooses a ClickHouse type. NULL is the column's flag: the
// table definition wraps the type in Nullable(…).
func clickhouseType(k canon, m *mapped) {
	asJSON := func(note string) {
		m.Type, m.Canon = "String", canon{T: "json", P: -1, S: -1, N: -1, Frac: -1}
		if note != "" {
			m.note(false, "%s", note)
		}
	}
	switch k.T {
	case "bool":
		m.Type = "Bool"
	case "int":
		bits := k.Bits
		if bits == 24 {
			bits = 32
		}
		if bits == 0 {
			bits = 64
		}
		m.Type = "Int" + strconv.Itoa(bits)
		if k.Unsigned {
			m.Type = "U" + m.Type
		}
	case "decimal":
		switch {
		case k.P <= 0:
			m.Type = "Decimal(38, 10)"
			m.note(true, "a decimal without a set precision becomes Decimal(38, 10)")
		case k.P > 76:
			m.Type = fmt.Sprintf("Decimal(76, %d)", min(max(k.S, 0), 76))
			m.note(true, "ClickHouse decimals hold 76 digits")
		default:
			m.Type = fmt.Sprintf("Decimal(%d, %d)", k.P, max(k.S, 0))
		}
	case "float":
		m.Type = "Float64"
		if k.Bits == 32 {
			m.Type = "Float32"
		}
	case "char", "varchar", "text", "xml", "objectid":
		m.Type = "String"
	case "set":
		m.Type = "String"
		m.note(false, "the set's values are copied as text")
	case "binary", "varbinary", "blob":
		m.Type = "String"
		m.note(false, "bytes are kept as they are in a String")
	case "date":
		m.Type = "Date32"
	case "time":
		m.Type = "String"
		m.note(false, "times of day are copied as text")
	case "datetime", "timestamptz":
		f := k.Frac
		if f < 0 || f > 6 {
			f = 6
		}
		m.Type = fmt.Sprintf("DateTime64(%d, 'UTC')", f)
		if k.T == "datetime" {
			m.note(false, "kept in UTC, so daylight saving can't shift the times")
		}
	case "interval":
		m.Type = "String"
		m.note(false, "intervals are copied as text")
	case "uuid":
		m.Type = "UUID"
	case "enum":
		if len(k.Values) == 0 {
			m.Type = "String"
			return
		}
		parts := make([]string, len(k.Values))
		for i, v := range k.Values {
			parts[i] = chtype.Quote(v) + " = " + strconv.Itoa(i+1)
		}
		m.Type = "Enum8(" + strings.Join(parts, ", ") + ")"
		if len(k.Values) > 127 {
			m.Type = "Enum16(" + strings.Join(parts, ", ") + ")"
		}
	case "geometry":
		switch k.Geo {
		case "point":
			m.Type = "Point"
		case "linestring":
			m.Type = "LineString"
		case "multilinestring":
			m.Type = "MultiLineString"
		case "polygon":
			m.Type = "Polygon"
		case "multipolygon":
			m.Type = "MultiPolygon"
		default:
			asJSON("shapes of mixed kinds are copied as GeoJSON text")
			return
		}
		if k.SRID != 0 && k.SRID != 4326 {
			m.note(true, "ClickHouse shapes have no SRID; coordinates are copied as they are")
		}
	case "json":
		asJSON("")
	case "array", "object":
		asJSON("copied as JSON text")
	default:
		m.Type = "String"
	}
}

// snowflakeCanon reads the types the Snowflake driver describes (NUMBER,
// VARCHAR, TIMESTAMP_NTZ…, VARIANT, GEOGRAPHY).
func snowflakeCanon(k canon, c driver.Column, base string) canon {
	switch base {
	case "number":
		p, s := iptr(c.Precision), iptr(c.Scale)
		if p <= 0 {
			p = 38
		}
		if s <= 0 {
			if p <= 18 {
				k.T, k.Bits = "int", 64
				return k
			}
			k.T, k.P, k.S = "decimal", p, 0
			return k
		}
		k.T, k.P, k.S = "decimal", p, s
	case "float":
		k.T, k.Bits = "float", 64
	case "varchar":
		if n := iptr(c.Length); n > 0 && n < 16777216 {
			k.T, k.N = "varchar", n
		} else {
			k.T = "text"
		}
	case "boolean":
		k.T = "bool"
	case "binary":
		if n := iptr(c.Length); n > 0 && n < 8388608 {
			k.T, k.N = "varbinary", n
		} else {
			k.T = "blob"
		}
	case "date":
		k.T = "date"
	case "time":
		k.T, k.Frac = "time", 9
	case "timestamp_ntz":
		k.T, k.Frac = "datetime", 9
	case "timestamp_tz", "timestamp_ltz":
		k.T, k.Frac = "timestamptz", 9
	case "variant", "object", "map", "array":
		k.T = "json"
	case "geography":
		k.T, k.Geog, k.SRID, k.Geo = "geometry", true, 4326, ""
	case "geometry":
		k.T, k.Geo = "geometry", ""
	default:
		k.T, k.Note = "text", c.Type+" is copied as text"
	}
	if (k.T == "time" || k.T == "datetime" || k.T == "timestamptz") && c.Scale != nil {
		k.Frac = int(*c.Scale)
	}
	return k
}

func snowflakeType(k canon, m *mapped) {
	frac := func(name string) string {
		if k.Frac >= 0 && k.Frac < 9 {
			return fmt.Sprintf("%s(%d)", name, k.Frac)
		}
		return name
	}
	switch k.T {
	case "bool":
		m.Type = "BOOLEAN"
	case "int":
		m.Type = "NUMBER(38,0)"
		if k.Unsigned && k.Bits == 64 {
			m.Type = "NUMBER(20,0)"
		}
	case "decimal":
		switch {
		case k.P <= 0:
			m.Type = "NUMBER(38,9)"
			m.note(true, "a decimal without fixed precision is stored as NUMBER(38,9)")
		case k.P > 38:
			m.Type = fmt.Sprintf("NUMBER(38,%d)", min(max(k.S, 0), 37))
			m.note(true, "Snowflake numbers hold 38 digits")
		default:
			m.Type = fmt.Sprintf("NUMBER(%d,%d)", k.P, max(k.S, 0))
		}
	case "float":
		m.Type = "FLOAT"
	case "char", "varchar":
		if k.N > 0 {
			m.Type = fmt.Sprintf("VARCHAR(%d)", k.N)
		} else {
			m.Type = "VARCHAR"
		}
	case "text", "xml", "interval", "set":
		m.Type = "VARCHAR"
		if k.T == "interval" {
			m.note(false, "Snowflake has no interval columns; stored as text")
		}
	case "binary", "varbinary":
		if k.N > 0 {
			m.Type = fmt.Sprintf("BINARY(%d)", k.N)
		} else {
			m.Type = "BINARY"
		}
	case "blob":
		m.Type = "BINARY"
		m.note(false, "Snowflake binary values hold up to 8 MB")
	case "date":
		m.Type = "DATE"
	case "time":
		m.Type = frac("TIME")
	case "datetime":
		m.Type = frac("TIMESTAMP_NTZ")
	case "timestamptz":
		m.Type = frac("TIMESTAMP_TZ")
	case "json", "object", "array":
		m.Type = "VARIANT"
	case "uuid":
		m.Type = "VARCHAR(36)"
	case "objectid":
		m.Type = "VARCHAR(24)"
	case "enum":
		m.Type = fmt.Sprintf("VARCHAR(%d)", max(maxLen(k.Values), 1))
		m.note(false, "Snowflake has no enums and does not enforce checks, so any text is accepted")
	case "geometry":
		if k.Geog {
			m.Type = "GEOGRAPHY"
		} else {
			m.Type = "GEOMETRY"
			if k.SRID == 4326 {
				m.Type = "GEOGRAPHY"
				m.note(false, "shapes in SRID 4326 become GEOGRAPHY")
			}
		}
	default:
		m.Type = "VARCHAR"
	}
}

// trinoCanon reads the types Trino describes (bigint, varchar(25),
// decimal(12,2), timestamp(6) with time zone, array(varchar), row(…)).
func trinoCanon(k canon, c driver.Column, base string) canon {
	switch base {
	case "tinyint":
		k.T, k.Bits = "int", 8
	case "smallint":
		k.T, k.Bits = "int", 16
	case "integer", "int":
		k.T, k.Bits = "int", 32
	case "bigint":
		k.T, k.Bits = "int", 64
	case "real":
		k.T, k.Bits = "float", 32
	case "double":
		k.T, k.Bits = "float", 64
	case "decimal":
		k.T, k.P, k.S = "decimal", iptr(c.Precision), iptr(c.Scale)
	case "boolean":
		k.T = "bool"
	case "varchar":
		if n := iptr(c.Length); n > 0 {
			k.T, k.N = "varchar", n
		} else {
			k.T = "text"
		}
	case "char":
		k.T, k.N = "char", max(iptr(c.Length), 1)
	case "varbinary":
		k.T = "blob"
	case "date":
		k.T = "date"
	case "time":
		k.T, k.Frac = "time", iptr(c.Scale)
		if c.Kind != driver.KindTime {
			k.T, k.Note = "text", "times with a time zone are copied as text"
		}
	case "timestamp":
		k.T, k.Frac = "datetime", iptr(c.Scale)
		if c.Kind == driver.KindTimestamp {
			k.T = "timestamptz"
		}
	case "json":
		k.T = "json"
	case "uuid":
		k.T = "uuid"
	case "array", "map", "row":
		k.T = "object"
	case "interval":
		k.T = "interval"
	case "geometry", "sphericalgeography":
		k.T, k.Note = "text", "shapes are copied as WKT text"
	default:
		k.T, k.Note = "text", c.Type+" is copied as text"
	}
	return k
}

// trinoType writes types the target catalog's table format takes. Iceberg
// (and any unknown connector) has no tinyint, smallint, char or JSON
// columns; Delta Lake has no time or uuid columns and keeps milliseconds in
// timestamps with a time zone; Hive has no time zones at all.
func trinoType(connector string, k canon, m *mapped) {
	format := map[string]string{"delta_lake": "Delta Lake", "hive": "Hive"}[connector]
	if format == "" {
		format = "Iceberg"
	}
	// Each format stores one precision (Iceberg microseconds; Delta Lake
	// milliseconds with a time zone; Hive milliseconds).
	frac := func(prec int) int {
		if k.Frac > prec {
			unit := map[int]string{3: "milliseconds", 6: "microseconds"}[prec]
			m.note(true, "%s keeps %s; finer fractions are rounded", format, unit)
		}
		return prec
	}
	asText := func(note string) {
		m.Type = "varchar"
		if note != "" {
			m.note(false, "%s", note)
		}
	}
	switch k.T {
	case "bool":
		m.Type = "boolean"
	case "int":
		switch {
		case k.Bits == 64 && k.Unsigned:
			m.Type = "decimal(20,0)"
		case format != "Iceberg" && (k.Bits == 8 && !k.Unsigned || k.Bits == 16 && !k.Unsigned):
			m.Type = map[int]string{8: "tinyint", 16: "smallint"}[k.Bits]
		case k.Bits > 0 && k.Bits < 32 || k.Bits == 32 && !k.Unsigned:
			m.Type = "integer"
		default:
			m.Type = "bigint"
		}
	case "decimal":
		switch {
		case k.P <= 0:
			m.Type = "decimal(38,9)"
			m.note(true, "a decimal without a set precision becomes decimal(38,9)")
		case k.P > 38:
			m.Type = fmt.Sprintf("decimal(38,%d)", min(max(k.S, 0), 37))
			m.note(true, "Trino decimals hold 38 digits")
		default:
			m.Type = fmt.Sprintf("decimal(%d,%d)", k.P, max(k.S, 0))
		}
	case "float":
		m.Type = "double"
		if k.Bits == 32 {
			m.Type = "real"
		}
	case "char", "varchar", "text", "xml", "objectid":
		m.Type = "varchar"
		if k.T == "char" {
			m.note(false, "fixed-length text becomes varchar")
		}
	case "set":
		asText("the set's values are copied as text")
	case "enum":
		asText("Trino tables have no enums or checks, so any text is accepted")
	case "interval":
		asText("intervals are copied as text")
	case "binary", "varbinary", "blob":
		m.Type = "varbinary"
	case "date":
		m.Type = "date"
	case "time":
		if format != "Iceberg" {
			asText(format + " has no time-of-day columns; stored as text")
			return
		}
		m.Type = fmt.Sprintf("time(%d)", frac(6))
	case "datetime":
		prec := 6
		if format == "Hive" {
			prec = 3
		}
		m.Type = fmt.Sprintf("timestamp(%d)", frac(prec))
	case "timestamptz":
		switch format {
		case "Hive":
			m.Type = fmt.Sprintf("timestamp(%d)", frac(3))
			m.note(false, "Hive has no time zones; stored as UTC")
		case "Delta Lake":
			m.Type = fmt.Sprintf("timestamp(%d) with time zone", frac(3))
		default:
			m.Type = fmt.Sprintf("timestamp(%d) with time zone", frac(6))
		}
	case "uuid":
		m.Type = "uuid"
		if format != "Iceberg" {
			asText(format + " has no uuid columns; stored as text")
		}
	case "json", "object", "array":
		m.Type, m.Canon = "varchar", canon{T: "json", P: -1, S: -1, N: -1, Frac: -1}
		m.note(false, "%s has no JSON columns; stored as JSON text", format)
	case "geometry":
		m.Type, m.Canon = "varchar", canon{T: "json", P: -1, S: -1, N: -1, Frac: -1}
		m.note(false, "shapes are copied as GeoJSON text")
	default:
		m.Type = "varchar"
	}
}

// cassandraCanon reads the types Cassandra and ScyllaDB describe (text,
// bigint, decimal, timeuuid, frozen<list<int>>, a user type's name…).
func cassandraCanon(k canon, c driver.Column, base string) canon {
	switch base {
	case "ascii", "text", "varchar":
		k.T = "text"
	case "inet":
		k.T, k.N = "varchar", 45
	case "tinyint":
		k.T, k.Bits = "int", 8
	case "smallint":
		k.T, k.Bits = "int", 16
	case "int":
		k.T, k.Bits = "int", 32
	case "bigint", "counter":
		k.T, k.Bits = "int", 64
	case "varint":
		k.T, k.S = "decimal", 0
	case "decimal":
		k.T = "decimal"
	case "float":
		k.T, k.Bits = "float", 32
	case "double":
		k.T, k.Bits = "float", 64
	case "boolean":
		k.T = "bool"
	case "uuid", "timeuuid":
		k.T = "uuid"
	case "timestamp":
		k.T, k.Frac = "timestamptz", 3
	case "date":
		k.T = "date"
	case "time":
		k.T, k.Frac = "time", 9
	case "duration":
		k.T, k.Note = "text", "durations are copied as text, such as 1h30m"
	case "blob":
		k.T = "blob"
	default:
		// Collections, tuples, vectors and user types.
		k.T = "object"
	}
	return k
}

// cassandraType writes Cassandra's types, which ScyllaDB shares.
func cassandraType(k canon, m *mapped) {
	switch k.T {
	case "bool":
		m.Type = "boolean"
	case "int":
		switch {
		case k.Bits == 64 && k.Unsigned:
			m.Type = "varint"
		case k.Bits == 8 && !k.Unsigned:
			m.Type = "tinyint"
		case k.Bits == 16 && !k.Unsigned:
			m.Type = "smallint"
		case k.Bits > 0 && k.Bits < 32 || k.Bits == 32 && !k.Unsigned:
			m.Type = "int"
		default:
			m.Type = "bigint"
		}
	case "decimal":
		m.Type = "decimal"
	case "float":
		m.Type = "double"
		if k.Bits == 32 {
			m.Type = "float"
		}
	case "char", "varchar", "text", "xml", "objectid":
		m.Type = "text"
	case "set":
		m.Type = "text"
		m.note(false, "the set's values are copied as text")
	case "enum":
		m.Type = "text"
		m.note(false, "Cassandra has no enums or checks, so any text is accepted")
	case "interval":
		m.Type = "text"
		m.note(false, "intervals are copied as text")
	case "binary", "varbinary", "blob":
		m.Type = "blob"
	case "date":
		m.Type = "date"
	case "time":
		m.Type = "time"
	case "datetime", "timestamptz":
		m.Type = "timestamp"
		if k.Frac > 3 || k.Frac < 0 {
			m.note(true, "Cassandra timestamps keep milliseconds; finer fractions are dropped")
		}
		if k.T == "datetime" {
			m.note(false, "stored as UTC")
		}
	case "uuid":
		m.Type = "uuid"
	case "json", "object", "array":
		m.Type, m.Canon = "text", canon{T: "json", P: -1, S: -1, N: -1, Frac: -1}
		m.note(false, "stored as JSON text")
	case "geometry":
		m.Type, m.Canon = "text", canon{T: "json", P: -1, S: -1, N: -1, Frac: -1}
		m.note(false, "shapes are copied as GeoJSON text")
	default:
		m.Type = "text"
	}
}

// elasticCanon reads Elasticsearch and OpenSearch field types.
func elasticCanon(k canon, base string) canon {
	switch base {
	case "_id", "keyword", "constant_keyword", "wildcard", "version", "text", "match_only_text", "search_as_you_type":
		k.T = "text"
	case "ip":
		k.T, k.N = "varchar", 45
	case "long":
		k.T, k.Bits = "int", 64
	case "integer":
		k.T, k.Bits = "int", 32
	case "short":
		k.T, k.Bits = "int", 16
	case "byte":
		k.T, k.Bits = "int", 8
	case "unsigned_long":
		k.T, k.P, k.S = "decimal", 20, 0
	case "double", "scaled_float":
		k.T, k.Bits = "float", 64
	case "float", "half_float":
		k.T, k.Bits = "float", 32
	case "boolean":
		k.T = "bool"
	case "date":
		k.T, k.Frac = "timestamptz", 3
	case "date_nanos":
		k.T, k.Frac = "timestamptz", 9
	case "binary":
		k.T = "blob"
	case "geo_point", "geo_shape":
		k.T, k.SRID, k.Geog = "geometry", 4326, true
		if base == "geo_point" {
			k.Geo = "point"
		}
	case "object", "nested", "flattened", "flat_object", "dense_vector", "knn_vector":
		k.T = "object"
	default:
		k.T, k.Note = "text", base+" values are copied as text"
	}
	return k
}

// elasticType writes field types for Elasticsearch and OpenSearch. Text
// fields get a keyword sub-field when the index is created.
func elasticType(k canon, m *mapped) {
	switch k.T {
	case "bool":
		m.Type = "boolean"
	case "int":
		switch {
		case k.Bits == 64 && k.Unsigned:
			m.Type = "unsigned_long"
		case k.Bits == 8 && !k.Unsigned:
			m.Type = "byte"
		case k.Bits == 16 && !k.Unsigned:
			m.Type = "short"
		case k.Bits > 0 && k.Bits < 32 || k.Bits == 32 && !k.Unsigned:
			m.Type = "integer"
		default:
			m.Type = "long"
		}
	case "decimal":
		if k.S >= 0 && k.S <= 4 && k.P > 0 && k.P <= 15 {
			// Exact to the scale, stored as a long.
			m.Type = fmt.Sprintf(`{"type": "scaled_float", "scaling_factor": %d}`, int(math.Pow10(max(k.S, 0))))
		} else {
			m.Type = "double"
			m.note(true, "Elasticsearch has no exact decimals; stored as double")
		}
	case "float":
		m.Type = "double"
		if k.Bits == 32 {
			m.Type = "float"
		}
	case "char", "varchar", "enum", "set", "uuid", "objectid":
		m.Type = "keyword"
		if k.T == "varchar" && (k.N <= 0 || k.N > 256) {
			m.Type = "text"
		}
	case "text", "xml":
		m.Type = "text"
	case "time", "interval":
		m.Type = "keyword"
		m.note(false, "copied as text")
	case "date":
		m.Type = "date"
	case "datetime", "timestamptz":
		m.Type = "date"
		if k.Frac > 3 {
			m.Type = "date_nanos"
		}
		if k.T == "datetime" {
			m.note(false, "stored as UTC")
		}
	case "binary", "varbinary", "blob":
		m.Type = "binary"
		m.note(false, "stored as base64 and not searchable")
	case "json", "object", "array":
		// Any JSON, kept as it is and not indexed.
		m.Type, m.Canon = `{"type": "object", "enabled": false}`, canon{T: "json", P: -1, S: -1, N: -1, Frac: -1}
		m.note(false, "stored as an object that isn't searchable")
	case "geometry":
		m.Type = "geo_shape"
		if k.Geo == "point" {
			m.Type = "geo_point"
		}
		if k.SRID != 0 && k.SRID != 4326 {
			m.note(true, "Elasticsearch shapes are in WGS 84 (SRID 4326); coordinates are copied as they are")
		}
	default:
		m.Type = "keyword"
	}
}

// databricksCanon reads Databricks (Spark SQL) types as information_schema
// writes them: bigint, decimal(10,2), array<string>, struct<…>, variant.
func databricksCanon(k canon, c driver.Column, base string) canon {
	switch base {
	case "string":
		k.T = "text"
	case "varchar", "char":
		k.T, k.N = base, iptr(c.Length)
		if k.N <= 0 {
			k.T = "text"
		}
	case "tinyint", "byte":
		k.T, k.Bits = "int", 8
	case "smallint", "short":
		k.T, k.Bits = "int", 16
	case "int", "integer":
		k.T, k.Bits = "int", 32
	case "bigint", "long":
		k.T, k.Bits = "int", 64
	case "float", "real":
		k.T, k.Bits = "float", 32
	case "double":
		k.T, k.Bits = "float", 64
	case "decimal", "dec", "numeric":
		k.T, k.P, k.S = "decimal", iptr(c.Precision), iptr(c.Scale)
	case "boolean":
		k.T = "bool"
	case "date":
		k.T = "date"
	case "timestamp", "timestamp_ltz":
		k.T, k.Frac = "timestamptz", 6
	case "timestamp_ntz":
		k.T, k.Frac = "datetime", 6
	case "binary":
		k.T = "blob"
	case "variant":
		k.T = "json"
	case "array", "map", "struct":
		k.T = "object"
	default:
		k.T, k.Note = "text", c.Type+" values are copied as text"
	}
	return k
}

// databricksType writes Delta table types.
func databricksType(k canon, m *mapped) {
	switch k.T {
	case "bool":
		m.Type = "BOOLEAN"
	case "int":
		switch {
		case k.Bits == 64 && k.Unsigned:
			m.Type = "DECIMAL(20,0)"
		case k.Bits == 8 && !k.Unsigned:
			m.Type = "TINYINT"
		case k.Bits == 16 && !k.Unsigned:
			m.Type = "SMALLINT"
		case k.Bits > 0 && k.Bits < 32 || k.Bits == 32 && !k.Unsigned:
			m.Type = "INT"
		default:
			m.Type = "BIGINT"
		}
	case "decimal":
		switch {
		case k.P <= 0:
			m.Type = "DECIMAL(38,9)"
			m.note(true, "a decimal without a set precision becomes DECIMAL(38,9)")
		case k.P > 38:
			m.Type = fmt.Sprintf("DECIMAL(38,%d)", min(max(k.S, 0), 37))
			m.note(true, "Databricks decimals hold 38 digits")
		default:
			m.Type = fmt.Sprintf("DECIMAL(%d,%d)", k.P, max(k.S, 0))
		}
	case "float":
		m.Type = "DOUBLE"
		if k.Bits == 32 {
			m.Type = "FLOAT"
		}
	case "char", "varchar", "text", "xml", "enum", "set", "uuid", "objectid":
		m.Type = "STRING"
		if k.T == "enum" {
			m.note(false, "Databricks has no enums; any text is accepted unless a CHECK constraint says otherwise")
		}
	case "binary", "varbinary", "blob":
		m.Type = "BINARY"
	case "date":
		m.Type = "DATE"
	case "time", "interval":
		m.Type = "STRING"
		m.note(false, "copied as text")
	case "datetime":
		m.Type = "TIMESTAMP_NTZ"
		if k.Frac > 6 {
			m.note(true, "Databricks timestamps keep microseconds; finer fractions are dropped")
		}
	case "timestamptz":
		m.Type = "TIMESTAMP"
		if k.Frac > 6 {
			m.note(true, "Databricks timestamps keep microseconds; finer fractions are dropped")
		}
	case "json", "object", "array":
		m.Type = "VARIANT"
	case "geometry":
		m.Type, m.Canon = "STRING", canon{T: "json", P: -1, S: -1, N: -1, Frac: -1}
		m.note(false, "shapes are copied as GeoJSON text")
	default:
		m.Type = "STRING"
	}
}
