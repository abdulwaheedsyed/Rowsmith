package driver

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
)

// NetworkFields are the host/port/credentials inputs most servers share.
func NetworkFields(defaultPort int, dbLabel string, dbRequired bool) []Field {
	f := []Field{
		{Key: "host", Label: "Host", Type: FieldText, Required: true, Placeholder: "db.example.com", Span: 4},
		{Key: "port", Label: "Port", Type: FieldNumber, Default: defaultPort, Span: 2},
		{Key: "user", Label: "User", Type: FieldText, Section: "auth", Span: 3},
		{Key: "password", Label: "Password", Type: FieldPassword, Secret: true, Section: "auth", Span: 3},
	}
	if dbLabel != "" {
		f = append(f, Field{Key: "database", Label: dbLabel, Type: FieldText, Required: dbRequired,
			Placeholder: map[bool]string{true: "", false: "optional — all visible databases are listed"}[dbRequired]})
	}
	return f
}

var TLSModes = []Option{
	{Value: "disable", Label: "Disabled"},
	{Value: "prefer", Label: "Preferred (encrypt if the server supports it)"},
	{Value: "require", Label: "Required (encrypt, do not verify certificate)"},
	{Value: "verify-ca", Label: "Verify CA (trusted certificate, any hostname)"},
	{Value: "verify-full", Label: "Verify full (trusted certificate and hostname)"},
}

func TLSFields(defaultMode string) []Field {
	caShow := map[string][]string{"tls": {"require", "verify-ca", "verify-full", "prefer"}}
	return []Field{
		{Key: "tls", Label: "TLS / SSL", Type: FieldSelect, Options: TLSModes, Default: defaultMode, Section: "tls"},
		{Key: "tlsCA", Label: "CA certificate (PEM)", Type: FieldFile, Section: "tls", ShowIf: caShow,
			Help: "Leave empty to use the system trust store."},
		{Key: "tlsCert", Label: "Client certificate (PEM)", Type: FieldFile, Section: "tls", ShowIf: caShow, Span: 3},
		{Key: "tlsKey", Label: "Client key (PEM)", Type: FieldFile, Secret: true, Section: "tls", ShowIf: caShow, Span: 3},
		{Key: "tlsServerName", Label: "Server name override", Type: FieldText, Section: "tls",
			ShowIf: map[string][]string{"tls": {"verify-full"}}, Help: "Hostname expected in the certificate, when it differs from Host."},
	}
}

// TLSConfig builds a *tls.Config for the chosen mode. It returns nil for
// "disable" (and for "prefer", which callers handle engine-specifically).
func TLSConfig(p OpenParams, host string) (*tls.Config, error) {
	mode := p.String("tls")
	if mode == "" || mode == "disable" {
		return nil, nil
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host}
	if sn := p.String("tlsServerName"); sn != "" {
		cfg.ServerName = sn
	}
	var pool *x509.CertPool
	if ca := strings.TrimSpace(p.String("tlsCA")); ca != "" {
		pool = x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(ca)) {
			return nil, errors.New("TLS: the CA certificate is not valid PEM")
		}
		cfg.RootCAs = pool
	}
	if cert, key := strings.TrimSpace(p.String("tlsCert")), strings.TrimSpace(p.Secret("tlsKey")); cert != "" || key != "" {
		pair, err := tls.X509KeyPair([]byte(cert), []byte(key))
		if err != nil {
			return nil, fmt.Errorf("TLS: client certificate/key: %w", err)
		}
		cfg.Certificates = []tls.Certificate{pair}
	}
	switch mode {
	case "prefer", "require":
		cfg.InsecureSkipVerify = true
	case "verify-ca":
		// Verify the chain but not the hostname.
		cfg.InsecureSkipVerify = true
		cfg.VerifyPeerCertificate = func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) == 0 {
				return errors.New("TLS: server sent no certificate")
			}
			certs := make([]*x509.Certificate, len(raw))
			for i, r := range raw {
				c, err := x509.ParseCertificate(r)
				if err != nil {
					return err
				}
				certs[i] = c
			}
			opts := x509.VerifyOptions{Roots: pool, Intermediates: x509.NewCertPool()}
			for _, c := range certs[1:] {
				opts.Intermediates.AddCert(c)
			}
			_, err := certs[0].Verify(opts)
			return err
		}
	case "verify-full":
	default:
		return nil, fmt.Errorf("TLS: unknown mode %q", mode)
	}
	return cfg, nil
}

// SplitHostPort returns host and port from the form, applying the default port.
func HostPort(p OpenParams, defaultPort int) (string, int) {
	host := strings.TrimSpace(p.String("host"))
	port := p.Int("port", defaultPort)
	if port == 0 {
		port = defaultPort
	}
	return host, port
}

// FollowRenames prepares an ALTER diff for engines that carry column renames
// into indexes, keys and foreign keys by themselves (PostgreSQL, MySQL): it
// returns copies of from and to whose index, primary-key and foreign-key
// column lists use the new names, so a rename alone does not rebuild them.
func FollowRenames(from *Table, to TableDef) (*Table, TableDef) {
	ren := map[string]string{}
	newNames := map[string]bool{}
	for _, c := range to.Columns {
		newNames[c.Name] = true
		if c.OriginalName != "" && c.OriginalName != c.Name {
			ren[c.OriginalName] = c.Name
		}
	}
	if len(ren) == 0 {
		return from, to
	}
	mapCols := func(cols []string, desired bool) []string {
		out := make([]string, len(cols))
		for i, c := range cols {
			out[i] = c
			// The desired side may still use an old name, unless that name
			// now belongs to another column.
			if n, ok := ren[c]; ok && (!desired || !newNames[c]) {
				out[i] = n
			}
		}
		return out
	}
	f := *from
	f.PrimaryKey = mapCols(from.PrimaryKey, false)
	f.Indexes = make([]Index, len(from.Indexes))
	for i, ix := range from.Indexes {
		ix.Columns = mapCols(ix.Columns, false)
		f.Indexes[i] = ix
	}
	f.ForeignKeys = make([]ForeignKey, len(from.ForeignKeys))
	for i, fk := range from.ForeignKeys {
		fk.Columns = mapCols(fk.Columns, false)
		f.ForeignKeys[i] = fk
	}
	t := to
	t.PrimaryKey = mapCols(to.PrimaryKey, true)
	t.Indexes = make([]Index, len(to.Indexes))
	for i, ix := range to.Indexes {
		ix.Columns = mapCols(ix.Columns, true)
		t.Indexes[i] = ix
	}
	t.ForeignKeys = make([]ForeignKey, len(to.ForeignKeys))
	for i, fk := range to.ForeignKeys {
		fk.Columns = mapCols(fk.Columns, true)
		t.ForeignKeys[i] = fk
	}
	return &f, t
}
