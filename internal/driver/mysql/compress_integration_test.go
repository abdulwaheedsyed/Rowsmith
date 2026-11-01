package mysql

import (
	"context"
	"os"
	"testing"
	"time"

	"rowsmith/internal/driver"
)

// With compression on, sessions use the compressed protocol where the
// server offers it.
func TestIntegrationCompression(t *testing.T) {
	for flavor, env := range map[string]string{"mysql": "ROWSMITH_TEST_MYSQL_HOST", "mariadb": "ROWSMITH_TEST_MARIADB_HOST", "tidb": "ROWSMITH_TEST_TIDB_HOST"} {
		t.Run(flavor, func(t *testing.T) {
			host := os.Getenv(env)
			if host == "" {
				t.Skip(env + " not set")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			dc, err := (&mysqlDriver{id: flavor}).Open(ctx, driver.OpenParams{AppName: "rowsmith-test",
				Params:  map[string]any{"host": host, "port": testPorts[flavor], "user": "root", "tls": "disable", "compression": "zlib"},
				Secrets: map[string]string{"password": os.Getenv("ROWSMITH_TEST_MYSQL_PASSWORD")}})
			if err != nil {
				t.Fatal(err)
			}
			defer dc.Close()
			var name, value string
			if err := dc.(*conn).db.QueryRowContext(ctx, "SHOW SESSION STATUS LIKE 'Compression'").Scan(&name, &value); err != nil {
				t.Fatal(err)
			}
			if value != "ON" {
				t.Errorf("Compression = %q", value)
			}
		})
	}
}
