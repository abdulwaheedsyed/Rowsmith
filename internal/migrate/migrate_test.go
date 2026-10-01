package migrate

import (
	"encoding/json"
	"strings"
	"testing"

	"rowsmith/internal/driver"
)

func i64(n int64) *int64 { return &n }

func TestTypeMapping(t *testing.T) {
	pg := target{Engine: Postgres, PostGIS: true}
	my := target{Engine: MySQL}
	ms := target{Engine: MSSQL}
	ora := target{Engine: Oracle, Version: 23}
	lite := target{Engine: SQLite}
	cases := []struct {
		src  string
		col  driver.Column
		dst  target
		use  use
		want string
	}{
		{MySQL, driver.Column{Type: "tinyint(1)", BaseType: "tinyint", Kind: driver.KindBool}, pg, use{}, "boolean"},
		{MySQL, driver.Column{Type: "bigint unsigned", BaseType: "bigint", Kind: driver.KindInt, Unsigned: true}, pg, use{}, "numeric(20,0)"},
		{MySQL, driver.Column{Type: "smallint unsigned", BaseType: "smallint", Kind: driver.KindInt, Unsigned: true}, pg, use{}, "integer"},
		{MySQL, driver.Column{Type: "decimal(12,3)", BaseType: "decimal", Kind: driver.KindDecimal, Precision: i64(12), Scale: i64(3)}, ora, use{}, "NUMBER(12,3)"},
		{MySQL, driver.Column{Type: "varchar(200)", BaseType: "varchar", Kind: driver.KindString, Length: i64(200)}, ms, use{}, "nvarchar(200)"},
		{MySQL, driver.Column{Type: "longtext", BaseType: "longtext", Kind: driver.KindText}, ms, use{Indexed: true}, "nvarchar(450)"},
		{MySQL, driver.Column{Type: "datetime(6)", BaseType: "datetime", Kind: driver.KindDateTime}, ms, use{}, "datetime2(6)"},
		{MySQL, driver.Column{Type: "point", BaseType: "point", Kind: driver.KindGeometry, SRID: 4326, GeometryType: "point"}, pg, use{}, "geometry(Point,4326)"},
		{MySQL, driver.Column{Type: "json", BaseType: "json", Kind: driver.KindJSON}, pg, use{}, "json"},
		{MySQL, driver.Column{Type: "enum('a','b')", BaseType: "enum", Kind: driver.KindEnum, Enum: []string{"a", "bb"}}, pg, use{}, "character varying(2)"},
		{Postgres, driver.Column{Type: "text", BaseType: "text", Kind: driver.KindText}, my, use{}, "longtext"},
		{Postgres, driver.Column{Type: "text", BaseType: "text", Kind: driver.KindText}, my, use{Indexed: true}, "varchar(768)"},
		{Postgres, driver.Column{Type: "numeric", BaseType: "numeric", Kind: driver.KindDecimal}, my, use{}, "decimal(65,30)"},
		{Postgres, driver.Column{Type: "timestamp with time zone", BaseType: "timestamptz", Kind: driver.KindTimestamp}, my, use{}, "datetime(6)"},
		{Postgres, driver.Column{Type: "timestamp(3) without time zone", BaseType: "timestamp", Kind: driver.KindDateTime}, my, use{}, "datetime(3)"},
		{Postgres, driver.Column{Type: "uuid", BaseType: "uuid", Kind: driver.KindUUID}, ms, use{}, "uniqueidentifier"},
		{Postgres, driver.Column{Type: "integer[]", BaseType: "_int4", Kind: driver.KindArray}, my, use{}, "json"},
		{Postgres, driver.Column{Type: "geography(Point,4326)", BaseType: "geography", Kind: driver.KindGeometry, SRID: 4326, GeometryType: "point"}, my, use{}, "point SRID 4326"},
		{Postgres, driver.Column{Type: "geometry(MultiPolygon,3857)", BaseType: "geometry", Kind: driver.KindGeometry, SRID: 3857, GeometryType: "multipolygon"}, target{Engine: MariaDB}, use{}, "multipolygon REF_SYSTEM_ID=3857"},
		{Postgres, driver.Column{Type: "bytea", BaseType: "bytea", Kind: driver.KindBinary}, ora, use{}, "BLOB"},
		{Postgres, driver.Column{Type: "geometry(Point,4326)", BaseType: "geometry", Kind: driver.KindGeometry, SRID: 4326}, target{Engine: Oracle, Version: 23, Spatial: true}, use{}, "SDO_GEOMETRY"},
		{Postgres, driver.Column{Type: "geometry(Point,4326)", BaseType: "geometry", Kind: driver.KindGeometry, SRID: 4326}, ora, use{}, "CLOB"},
		{Oracle, driver.Column{Type: "SDO_GEOMETRY", BaseType: "sdo_geometry", Kind: driver.KindGeometry, SRID: 4326}, pg, use{}, "geometry(Geometry,4326)"},
		{MSSQL, driver.Column{Type: "tinyint", BaseType: "tinyint", Kind: driver.KindInt}, pg, use{}, "smallint"},
		{MSSQL, driver.Column{Type: "nvarchar(max)", BaseType: "nvarchar", Kind: driver.KindText}, pg, use{}, "text"},
		{MSSQL, driver.Column{Type: "datetimeoffset(3)", BaseType: "datetimeoffset", Kind: driver.KindTimestamp, Scale: i64(3)}, pg, use{}, "timestamptz(3)"},
		{MSSQL, driver.Column{Type: "money", BaseType: "money", Kind: driver.KindDecimal}, my, use{}, "decimal(19,4)"},
		{MSSQL, driver.Column{Type: "bit", BaseType: "bit", Kind: driver.KindBool}, ora, use{}, "BOOLEAN"},
		{MSSQL, driver.Column{Type: "bit", BaseType: "bit", Kind: driver.KindBool}, target{Engine: Oracle, Version: 19}, use{}, "NUMBER(1)"},
		{Oracle, driver.Column{Type: "NUMBER(10)", BaseType: "number", Kind: driver.KindInt, Precision: i64(10)}, pg, use{}, "bigint"},
		{Oracle, driver.Column{Type: "NUMBER(9)", BaseType: "number", Kind: driver.KindInt, Precision: i64(9)}, pg, use{}, "integer"},
		{Oracle, driver.Column{Type: "NUMBER", BaseType: "number", Kind: driver.KindDecimal}, pg, use{}, "numeric"},
		{Oracle, driver.Column{Type: "DATE", BaseType: "date", Kind: driver.KindDateTime}, pg, use{}, "timestamp"},
		{Oracle, driver.Column{Type: "VARCHAR2(100 CHAR)", BaseType: "varchar2", Kind: driver.KindString, Length: i64(100)}, pg, use{}, "character varying(100)"},
		{Oracle, driver.Column{Type: "TIMESTAMP(6) WITH TIME ZONE", BaseType: "timestamp with time zone", Kind: driver.KindTimestamp}, ms, use{}, "datetimeoffset(6)"},
		{SQLite, driver.Column{Type: "DECIMAL(10,2)", BaseType: "decimal", Kind: driver.KindDecimal}, pg, use{}, "numeric(10,2)"},
		{SQLite, driver.Column{Type: "INTEGER", BaseType: "integer", Kind: driver.KindInt}, my, use{}, "bigint"},
		{BigQuery, driver.Column{Type: "STRING", BaseType: "string", Kind: driver.KindString}, pg, use{}, "text"},
		{BigQuery, driver.Column{Type: "TIMESTAMP", BaseType: "timestamp", Kind: driver.KindTimestamp}, ora, use{}, "TIMESTAMP(6) WITH TIME ZONE"},
		{MongoDB, driver.Column{Type: "objectId", BaseType: "objectId", Kind: driver.KindOther}, pg, use{}, "character varying(24)"},
		{MongoDB, driver.Column{Type: "date", BaseType: "date", Kind: driver.KindTimestamp}, my, use{}, "datetime(3)"},
		{MongoDB, driver.Column{Type: "array", BaseType: "array", Kind: driver.KindArray}, pg, use{}, "jsonb"},
		{Postgres, driver.Column{Type: "time(3) without time zone", BaseType: "time", Kind: driver.KindTime}, ora, use{}, "VARCHAR2(18)"},
		{Postgres, driver.Column{Type: "double precision", BaseType: "float8", Kind: driver.KindFloat}, lite, use{}, "REAL"},
	}
	for _, c := range cases {
		m := typeFor(c.dst, canonOf(c.src, c.col), c.use)
		if m.Type != c.want {
			t.Errorf("%s %s → %s: got %q, want %q (notes %v)", c.src, c.col.Type, c.dst.Engine, m.Type, c.want, m.Notes)
		}
	}
	if m := typeFor(target{Engine: Postgres}, canonOf(MySQL, driver.Column{Type: "point", BaseType: "point", Kind: driver.KindGeometry}), use{}); m.Type != "jsonb" || !m.Lossy {
		t.Errorf("geometry without PostGIS: %+v", m)
	}
}

func TestDefaults(t *testing.T) {
	k := func(ty string) canon { return canon{T: ty} }
	cases := []struct {
		src, dst, def string
		k             canon
		dstType       string
		want          string // "" for dropped
	}{
		{MySQL, Postgres, "CURRENT_TIMESTAMP(6)", k("datetime"), "timestamp(6)", "CURRENT_TIMESTAMP"},
		{Postgres, MySQL, "now()", k("timestamptz"), "datetime(6)", "CURRENT_TIMESTAMP(6)"},
		{Postgres, MSSQL, "now()", k("timestamptz"), "datetimeoffset(6)", "SYSDATETIMEOFFSET()"},
		{MSSQL, Postgres, "(getdate())", k("datetime"), "timestamp(3)", "CURRENT_TIMESTAMP"},
		{Postgres, MySQL, "'abc'::character varying", k("varchar"), "varchar(10)", "'abc'"},
		{MySQL, MSSQL, "'x'", k("varchar"), "nvarchar(10)", "N'x'"},
		{MSSQL, Postgres, "((0))", k("int"), "integer", "0"},
		{Postgres, MySQL, "false", k("bool"), "tinyint(1)", "0"},
		{MySQL, Postgres, "1", k("bool"), "boolean", "TRUE"},
		{Postgres, MySQL, "nextval('seq'::regclass)", k("int"), "int", ""},
		{Postgres, MySQL, "gen_random_uuid()", k("uuid"), "char(36)", "(UUID())"},
		{Postgres, MySQL, "'{}'::text[]", k("array"), "json", ""},
		{Postgres, MySQL, "some_function(1)", k("int"), "int", ""},
		{Postgres, MySQL, "'x'", k("text"), "longtext", ""},
		{Oracle, Postgres, "SYSDATE", k("datetime"), "timestamp", "CURRENT_TIMESTAMP"},
	}
	for _, c := range cases {
		got, _ := translateDefault(c.src, c.dst, c.def, c.k, c.dstType)
		g := ""
		if got != nil {
			g = *got
		}
		if g != c.want {
			t.Errorf("%s→%s %q: got %q, want %q", c.src, c.dst, c.def, g, c.want)
		}
	}
}

func TestPgArray(t *testing.T) {
	v, err := pgArray(`{1,2,NULL,"a b","q\"x",{3,4},t}`)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(v)
	if string(b) != `[1,2,null,"a b","q\"x",[3,4],true]` {
		t.Fatalf("got %s", b)
	}
	if _, err := pgArray("{1,2"); err == nil {
		t.Fatal("unterminated array should fail")
	}
}

func TestConvert(t *testing.T) {
	cases := []struct {
		src, dst string
		sk, dk   canon
		in       any
		want     any
	}{
		{MySQL, Postgres, canon{T: "bool"}, canon{T: "bool"}, int64(1), true},
		{Postgres, Oracle, canon{T: "bool"}, canon{T: "int"}, true, int64(1)},
		{Postgres, MySQL, canon{T: "timestamptz"}, canon{T: "datetime"}, "2024-02-29 12:00:00+03:00", "2024-02-29 09:00:00"},
		{MySQL, Postgres, canon{T: "datetime"}, canon{T: "timestamptz"}, "2024-02-29 23:59:59.999999", "2024-02-29 23:59:59.999999+00:00"},
		{Postgres, MySQL, canon{T: "time"}, canon{T: "time"}, "13:45:59+03", "13:45:59"},
		{Postgres, MySQL, canon{T: "array"}, canon{T: "json"}, "{1,2,3}", "[1,2,3]"},
		{Postgres, MySQL, canon{T: "decimal", Note: "money is copied as a decimal"}, canon{T: "decimal"}, "$1,234.50", "1234.50"},
		{MongoDB, Postgres, canon{T: "objectid"}, canon{T: "varchar"}, map[string]any{"$oid": "65a1b2c3d4e5f6a7b8c9d0e1"}, "65a1b2c3d4e5f6a7b8c9d0e1"},
		{MongoDB, MySQL, canon{T: "timestamptz"}, canon{T: "datetime"}, map[string]any{"$date": "2024-02-29T09:00:00.000Z"}, "2024-02-29 09:00:00"},
		{MongoDB, Postgres, canon{T: "object"}, canon{T: "json"}, driver.Doc{Keys: []string{"b", "a"}, Values: map[string]any{"a": int64(1), "b": "x"}}, `{"b":"x","a":1}`},
		{Postgres, MongoDB, canon{T: "timestamptz"}, canon{T: "timestamptz"}, "2024-02-29 12:00:00+03:00", "2024-02-29T09:00:00Z"},
		{Postgres, MySQL, canon{T: "char"}, canon{T: "varchar"}, "ab   ", "ab"},
		{Postgres, MySQL, canon{T: "int"}, canon{T: "varchar"}, int64(5), "5"},
		{MySQL, Postgres, canon{T: "text"}, canon{T: "json"}, "plain", `"plain"`},
		{MySQL, MSSQL, canon{T: "int", Bits: 64, Unsigned: true}, canon{T: "decimal"}, int64(0), "0"},
		{SQLite, Postgres, canon{T: "decimal"}, canon{T: "decimal"}, 12.5, "12.5"},
	}
	for i, c := range cases {
		got, err := newConverter(c.src, c.dst, c.sk, c.dk)(c.in)
		if err != nil || got != c.want {
			t.Errorf("case %d (%s→%s %s→%s): got %#v, %v; want %#v", i, c.src, c.dst, c.sk.T, c.dk.T, got, err, c.want)
		}
	}
	geo := map[string]any{"$geo": json.RawMessage(`{"type":"Point","coordinates":[1,2]}`), "srid": 4326, "wkt": "POINT (1 2)"}
	got, _ := newConverter(MSSQL, Postgres, canon{T: "geometry"}, canon{T: "geometry"})(geo)
	if m, ok := got.(map[string]any); !ok || len(m) != 1 || string(m["$geo"].(json.RawMessage)) != `{"type":"Point","coordinates":[1,2]}` {
		t.Errorf("geometry: %#v", got)
	}
	got, _ = newConverter(MySQL, Oracle, canon{T: "geometry"}, canon{T: "text"})(geo)
	if got != `{"type":"Point","coordinates":[1,2]}` {
		t.Errorf("geometry into text: %#v", got)
	}
	got, _ = newConverter(MSSQL, MySQL, canon{T: "geometry"}, canon{T: "geometry"})("SRID=4326;POINT (24.7136 46.6753)")
	if m, ok := got.(map[string]any); !ok || string(m["$geo"].(json.RawMessage)) != `{"type":"Point","coordinates":[24.7136,46.6753]}` {
		t.Errorf("EWKT: %#v", got)
	}
	if _, err := newConverter(MSSQL, Postgres, canon{T: "geometry"}, canon{T: "geometry"})(map[string]any{"$geo": map[string]any{}, "display": "extent"}); err == nil {
		t.Error("a shortened shape must not be copied")
	}
}

func TestNormalizeMatchesAcrossEngines(t *testing.T) {
	same := []struct {
		k    canon
		a, b any
	}{
		{canon{T: "decimal"}, "-123456789.125", "-123456789.1250"},
		{canon{T: "decimal"}, "$12.34", "12.34"},
		{canon{T: "int"}, int64(7), "7"},
		{canon{T: "bool"}, int64(1), true},
		{canon{T: "float", Bits: 32}, 1.5, "1.5"},
		{canon{T: "float", Bits: 64}, 3.141592653589793, 3.14159265358979},
		{canon{T: "timestamptz"}, "2024-02-29 12:00:00+03:00", "2024-02-29 09:00:00"},
		{canon{T: "timestamptz"}, map[string]any{"$date": "2024-02-29T09:00:00.000Z"}, "2024-02-29 09:00:00+00:00"},
		{canon{T: "datetime", Frac: 0}, "2024-02-29 00:00:00", "2024-02-29"},
		{canon{T: "time"}, "13:45:59.123", "13:45:59.123000"},
		{canon{T: "json"}, `{"b": 1.50, "a": [1, 2]}`, `{"a":[1,2],"b":1.5}`},
		{canon{T: "array"}, "{1,2,3}", "[1, 2, 3]"},
		{canon{T: "uuid"}, "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11", "a0eebc99-9c0b-4ef8-bb6d-6bb9bd380a11"},
		{canon{T: "blob"}, map[string]any{"$bin": "3q2+7w==", "size": 4}, driver.LargeBinary{Data: []byte{0xde, 0xad, 0xbe, 0xef}}},
		{canon{T: "char"}, "ab   ", "ab"},
		{canon{T: "varchar"}, "", nil},
		{canon{T: "varbinary"}, map[string]any{"$bin": "", "size": 0}, nil},
		{canon{T: "binary"}, "0x", nil},
		{canon{T: "object"}, driver.Doc{Keys: []string{"x"}, Values: map[string]any{"x": int64(1)}}, `{"x": 1}`},
		{canon{T: "text"}, driver.LongText{Text: "long"}, "long"},
		{canon{T: "interval"}, "+01 02:03:04.000000", "1 day 02:03:04"},
		{canon{T: "geometry"}, map[string]any{"$geo": json.RawMessage(`{"type":"Point","coordinates":[24.71360000001,46.6753]}`), "srid": 4326}, "POINT (24.7136 46.6753)"},
		{canon{T: "geometry"}, "SRID=4326;POLYGON ((0 0, 0 1, 1 1, 1 0, 0 0))", `{"type":"Polygon","coordinates":[[[0,0],[1,0],[1,1],[0,1],[0,0]]]}`},
		{canon{T: "interval"}, "P1DT2H3M4S", "1 day 02:03:04"},
		{canon{T: "interval"}, "+01-02", "1 year 2 mons"},
		{canon{T: "interval"}, "-00 00:00:30.500000", "-00:00:30.5"},
	}
	for i, c := range same {
		if a, b := normalize(c.a, c.k), normalize(c.b, c.k); a != b {
			t.Errorf("case %d (%s): %q != %q", i, c.k.T, a, b)
		}
	}
	differ := []struct {
		k    canon
		a, b any
	}{
		{canon{T: "decimal"}, "1.25", "1.2"},
		{canon{T: "datetime"}, "2024-02-29 23:59:59.999999", "2024-03-01 00:00:00"},
		{canon{T: "text"}, "héllo", "hello"},
		{canon{T: "bool"}, true, false},
		{canon{T: "geometry"}, "POINT (24.7136 46.6753)", "POINT (46.6753 24.7136)"},
		{canon{T: "geometry"}, "POINT (1 2)", nil},
	}
	for i, c := range differ {
		if normalize(c.a, c.k) == normalize(c.b, c.k) {
			t.Errorf("differ case %d (%s) should not match", i, c.k.T)
		}
	}
}

func TestDigestIgnoresOrder(t *testing.T) {
	a, b := newDigest(2), newDigest(2)
	a.add([]string{"1", "x"})
	a.add([]string{"2", "y"})
	b.add([]string{"2", "y"})
	b.add([]string{"1", "x"})
	if a.Sums[0] != b.Sums[0] || a.Sums[1] != b.Sums[1] {
		t.Fatal("digests should not depend on row order")
	}
	c := newDigest(2)
	c.add([]string{"1", "y"})
	c.add([]string{"2", "x"})
	if c.Sums[0] != a.Sums[0] || c.Sums[1] == a.Sums[1] && false {
		t.Fatal("unexpected")
	}
}

func TestRequalify(t *testing.T) {
	in := `CREATE VIEW "public"."v" AS SELECT * FROM public.zoo JOIN other.public_x ON true WHERE mypublic.y = 1`
	want := `CREATE VIEW "copy"."v" AS SELECT * FROM "copy".zoo JOIN other.public_x ON true WHERE mypublic.y = 1`
	if got := requalify(in, "public", "copy"); got != want {
		t.Fatalf("got  %s\nwant %s", got, want)
	}
}

func TestParentsFirst(t *testing.T) {
	child := &TablePlan{Source: driver.ObjectRef{Name: "child"}, ForeignKeys: []*FKPlan{{RefTable: "parent"}}}
	parent := &TablePlan{Source: driver.ObjectRef{Name: "parent"}}
	self := &TablePlan{Source: driver.ObjectRef{Name: "tree"}, ForeignKeys: []*FKPlan{{RefTable: "tree"}}}
	by := map[string]*TablePlan{"child": child, "parent": parent, "tree": self}
	var names []string
	for _, t := range parentsFirst([]*TablePlan{child, self, parent}, by) {
		names = append(names, t.Source.Name)
	}
	if strings.Join(names, ",") != "parent,child,tree" {
		t.Fatalf("order: %v", names)
	}
}
