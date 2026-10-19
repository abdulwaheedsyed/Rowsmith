package snowflake

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/youmark/pkcs8"

	"rowsmith/internal/driver"
)

func TestColumnTypes(t *testing.T) {
	cases := map[string]struct {
		typ  string
		kind driver.ValueKind
	}{
		`{"type":"FIXED","precision":38,"scale":0,"nullable":false}`:        {"NUMBER(38,0)", driver.KindInt},
		`{"type":"FIXED","precision":12,"scale":2,"nullable":true}`:         {"NUMBER(12,2)", driver.KindDecimal},
		`{"type":"TEXT","length":16777216,"nullable":true}`:                 {"VARCHAR", driver.KindText},
		`{"type":"TEXT","length":80,"nullable":true}`:                       {"VARCHAR(80)", driver.KindString},
		`{"type":"TIMESTAMP_TZ","precision":0,"scale":9}`:                   {"TIMESTAMP_TZ", driver.KindTimestamp},
		`{"type":"TIMESTAMP_NTZ","precision":0,"scale":3}`:                  {"TIMESTAMP_NTZ(3)", driver.KindDateTime},
		`{"type":"VARIANT","nullable":true}`:                                {"VARIANT", driver.KindJSON},
		`{"type":"GEOGRAPHY","nullable":true}`:                              {"GEOGRAPHY", driver.KindGeometry},
		`{"type":"BINARY","length":8388608,"nullable":true}`:                {"BINARY", driver.KindBinary},
		`{"type":"VECTOR","vectorElementType":"FLOAT","vectorDimension":3}`: {"VECTOR(FLOAT, 3)", driver.KindArray},
	}
	for raw, want := range cases {
		var st sfType
		if err := json.Unmarshal([]byte(raw), &st); err != nil {
			t.Fatal(err)
		}
		c := column("C", st)
		if c.Type != want.typ || c.Kind != want.kind {
			t.Errorf("%s = %s/%s, want %s/%s", raw, c.Type, c.Kind, want.typ, want.kind)
		}
	}
}

func TestParsePlan(t *testing.T) {
	raw := `{"GlobalStats":{"partitionsTotal":4,"partitionsAssigned":1,"bytesAssigned":2048},"Operations":[[
		{"id":0,"operation":"Result","expressions":["T.A"]},
		{"id":1,"operation":"Filter","expressions":["T.A = 7"],"parentOperators":[0]},
		{"id":2,"operation":"TableScan","objects":["DB.S.T"],"partitionsAssigned":1,"partitionsTotal":4,"bytesAssigned":2048,"parentOperators":[1]}]]}`
	p := parsePlan(raw)
	if p.Root == nil || p.Root.Operation != "Result" || len(p.Root.Children) != 1 {
		t.Fatalf("plan = %+v", p.Root)
	}
	scan := p.Root.Children[0].Children[0]
	if scan.Object != "DB.S.T" || scan.Detail != "1 of 4 partitions" || p.Totals["partitionsTotal"] != float64(4) {
		t.Errorf("scan = %+v, totals %v", scan, p.Totals)
	}
}

func TestFileTransfer(t *testing.T) {
	for stmt, want := range map[string]bool{
		"PUT file:///etc/passwd @~":                 true,
		"  get @~/x file:///tmp/":                   true,
		"-- note\nPUT file:///a @s":                 true,
		"/* c */ // d\nGET @s file:///tmp":          true,
		"SELECT 'PUT file:///x'":                    false,
		"INSERT INTO puts VALUES (1)":               false,
		"COPY INTO t FROM @stage FILE_FORMAT = (x)": false,
	} {
		if got := fileTransfer(stmt); got != want {
			t.Errorf("fileTransfer(%q) = %v", stmt, got)
		}
	}
}

func TestPrivateKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	plain := string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if k, err := privateKey(plain, ""); err != nil || k.N.Cmp(key.N) != 0 {
		t.Fatalf("PKCS#8: %v", err)
	}
	pkcs1 := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	if _, err := privateKey(pkcs1, ""); err != nil {
		t.Fatalf("PKCS#1: %v", err)
	}
	enc, err := pkcs8.MarshalPrivateKey(key, []byte("s3cret"), nil)
	if err != nil {
		t.Fatal(err)
	}
	encPEM := string(pem.EncodeToMemory(&pem.Block{Type: "ENCRYPTED PRIVATE KEY", Bytes: enc}))
	if _, err := privateKey(encPEM, ""); err == nil || !strings.Contains(err.Error(), "passphrase") {
		t.Fatalf("encrypted without passphrase: %v", err)
	}
	if k, err := privateKey(encPEM, "s3cret"); err != nil || k.N.Cmp(key.N) != 0 {
		t.Fatalf("encrypted: %v", err)
	}
	if _, err := privateKey("not pem", ""); err == nil {
		t.Fatal("garbage was accepted")
	}
}

func TestAlterRenamesClusteredColumn(t *testing.T) {
	from := &driver.Table{Ref: driver.ObjectRef{Database: "D", Schema: "S", Name: "T"}, Options: map[string]string{"cluster_by": "NAME, ID"},
		Columns: []driver.Column{{Name: "ID", Type: "NUMBER(38,0)"}, {Name: "NAME", Type: "VARCHAR(80)"}}}
	to := driver.TableDef{Ref: from.Ref, Options: map[string]string{"cluster_by": "NAME, ID"}, Columns: []driver.ColumnDef{
		{Column: driver.Column{Name: "ID", Type: "NUMBER(38,0)"}, OriginalName: "ID"},
		{Column: driver.Column{Name: "FULL_NAME", Type: "VARCHAR(80)"}, OriginalName: "NAME"},
	}}
	stmts, err := (&conn{}).AlterTableSQL(from, to)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(stmts, "\n")
	want := `ALTER TABLE "D"."S"."T" DROP CLUSTERING KEY
ALTER TABLE "D"."S"."T" RENAME COLUMN "NAME" TO "FULL_NAME"
ALTER TABLE "D"."S"."T" CLUSTER BY (FULL_NAME, ID)`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}
