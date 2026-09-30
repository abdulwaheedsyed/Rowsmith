// Package bigquery implements the Google BigQuery driver. BigQuery is not a
// database/sql engine: the driver talks to the REST API through the official
// client library, maps datasets to schemas, reads unfiltered table pages for
// free through tabledata.list and guards every query with a dry-run cost
// estimate and a maximum-bytes-billed limit.
package bigquery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"cloud.google.com/go/bigquery"
	bq "google.golang.org/api/bigquery/v2"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"

	"rowsmith/internal/driver"
)

type bqDriver struct{}

func init() { driver.Register(bqDriver{}) }

var kinds = []driver.KindInfo{
	{Kind: "table", Label: "Tables", Icon: "table", Browse: true},
	{Kind: "view", Label: "Views", Icon: "view", Browse: true},
	{Kind: "materialized_view", Label: "Materialized views", Icon: "view", Browse: true},
	{Kind: "external_table", Label: "External tables", Icon: "table", Browse: true},
	{Kind: "routine", Label: "Routines", Icon: "function"},
}

var types = []string{
	"INT64", "NUMERIC", "NUMERIC(12, 2)", "BIGNUMERIC", "FLOAT64", "BOOL", "STRING", "STRING(255)", "BYTES", "DATE",
	"DATETIME", "TIME", "TIMESTAMP", "INTERVAL", "JSON", "GEOGRAPHY", "ARRAY<STRING>", "ARRAY<INT64>",
	"STRUCT<name STRING, value INT64>", "RANGE<DATE>", "RANGE<TIMESTAMP>",
}

// defaultMaxGB is the per-query cost guard applied when the form leaves it empty.
const defaultMaxGB = 10

func (bqDriver) Info() driver.Info {
	fields := []driver.Field{
		{Key: "project", Label: "Project ID", Type: driver.FieldText, Required: true, Placeholder: "my-gcp-project", Span: 3,
			Help: "Queries run and are billed in this project; its datasets are listed as schemas."},
		{Key: "location", Label: "Location", Type: driver.FieldText, Placeholder: "optional, e.g. US, EU, me-central2", Span: 3},
		{Key: "credentials", Label: "Service account key (JSON)", Type: driver.FieldFile, Secret: true, Section: "auth",
			Placeholder: `{"type": "service_account", "project_id": …}`,
			Help:        "Leave empty to use Application Default Credentials (GOOGLE_APPLICATION_CREDENTIALS, workload identity or the metadata server)."},
		{Key: "dataset", Label: "Default dataset", Type: driver.FieldText, Placeholder: "optional — used for unqualified table names", Span: 3},
		{Key: "maxBilledGB", Label: "Maximum bytes billed per query (GB)", Type: driver.FieldNumber, Default: defaultMaxGB, Span: 3,
			Help: "Cost guard: queries estimated to process more are refused before they run, and every job carries the limit. 0 = no limit."},
		{Key: "endpoint", Label: "API endpoint", Type: driver.FieldText, Section: "advanced", Placeholder: "http://localhost:9050",
			Help: "Only for emulators and testing. Requests to this endpoint are sent without authentication."},
	}
	return driver.Info{
		ID: "bigquery", Name: "BigQuery", Order: 70,
		Description: "Google BigQuery (GoogleSQL) with dry-run cost estimates and free table previews",
		Dialect:     "bigquery", Fields: fields, SSH: false,
		Caps:  driver.Caps{Schemas: true, SQL: true, Explain: true, Processes: true, Geometry: true, CostEstimate: true},
		Kinds: kinds, Types: types, URLSchemes: []string{"bigquery"}, QuoteChar: "`",
	}
}

type conn struct {
	client   *bigquery.Client
	svc      *bq.Service // raw REST API, for listings and job details the client library does not expose
	project  string
	location string
	dataset  string // default dataset for unqualified names
	maxBytes int64  // per-query cost guard, 0 = no limit
	user     string
	auth     string
	endpoint string
	ro       bool
	labels   map[string]string

	ddl sync.Map // "project.dataset.table" -> cachedDDL
}

func (bqDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	c := &conn{
		project:  strings.TrimSpace(p.String("project")),
		location: strings.TrimSpace(p.String("location")),
		dataset:  strings.TrimSpace(p.String("dataset")),
		endpoint: strings.TrimSpace(p.String("endpoint")),
		ro:       p.ReadOnly,
	}
	if c.project == "" {
		return nil, errors.New("project is required")
	}
	gb, err := gigabytes(p, "maxBilledGB")
	if err != nil {
		return nil, err
	}
	c.maxBytes = int64(gb * (1 << 30))
	if l := labelValue(p.AppName); l != "" {
		c.labels = map[string]string{"client": l}
	}

	var opts []option.ClientOption
	key := strings.TrimSpace(p.Secret("credentials"))
	switch {
	case c.endpoint != "":
		opts = append(opts, option.WithEndpoint(c.endpoint), option.WithoutAuthentication())
		c.auth = "none (custom endpoint)"
	case key != "":
		email, err := serviceAccountEmail(key)
		if err != nil {
			return nil, err
		}
		opts = append(opts, option.WithAuthCredentialsJSON(option.ServiceAccount, []byte(key)))
		c.user, c.auth = email, "service account key"
	default:
		c.auth = "Application Default Credentials"
	}
	client, err := bigquery.NewClient(ctx, c.project, opts...)
	if err != nil {
		return nil, credentialsError(err, c.auth)
	}
	client.Location = c.location
	svc, err := bq.NewService(ctx, append([]option.ClientOption{option.WithScopes(bigquery.Scope)}, opts...)...)
	if err != nil {
		client.Close()
		return nil, credentialsError(err, c.auth)
	}
	c.client, c.svc = client, svc
	// The client retries refused connections until its context ends.
	pctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := c.Ping(pctx); err != nil {
		client.Close()
		if pctx.Err() != nil && ctx.Err() == nil {
			host := c.endpoint
			if host == "" {
				host = "bigquery.googleapis.com"
			}
			return nil, fmt.Errorf("the BigQuery API at %s did not answer within %s", host, pingTimeout)
		}
		return nil, err
	}
	return c, nil
}

const pingTimeout = 20 * time.Second

// gigabytes reads the cost guard from the form. An absent or empty value
// means the default, so clearing the field never silently removes the guard.
func gigabytes(p driver.OpenParams, key string) (float64, error) {
	gb := float64(defaultMaxGB)
	switch v := p.Params[key].(type) {
	case float64:
		gb = v
	case int:
		gb = float64(v)
	case string:
		if s := strings.TrimSpace(v); s != "" {
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return 0, errors.New("maximum bytes billed must be a number of GB")
			}
			gb = f
		}
	}
	if gb < 0 || math.IsNaN(gb) || gb >= math.MaxInt64/(1<<30) {
		return 0, errors.New("maximum bytes billed must be a number of GB between 0 and 8 billion")
	}
	return gb, nil
}

// serviceAccountEmail validates a key file. Only service account keys are
// accepted: other credential types (external accounts) can make the server
// fetch tokens from URLs chosen by whoever wrote the file.
func serviceAccountEmail(key string) (string, error) {
	var k struct {
		Type        string `json:"type"`
		ClientEmail string `json:"client_email"`
	}
	if err := json.Unmarshal([]byte(key), &k); err != nil {
		return "", errors.New("the service account key is not valid JSON")
	}
	if k.Type != "service_account" {
		return "", fmt.Errorf("expected a service account key (type \"service_account\"), got type %q", k.Type)
	}
	return k.ClientEmail, nil
}

func credentialsError(err error, auth string) error {
	if auth == "Application Default Credentials" {
		return fmt.Errorf("no service account key was provided and Application Default Credentials are not available: %w", err)
	}
	return fmt.Errorf("BigQuery client: %w", err)
}

// labelValue turns an application name into a valid job label value.
func labelValue(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	return out
}

func (c *conn) Close() error { return c.client.Close() }

// Ping lists at most one dataset: it checks credentials without running a job.
func (c *conn) Ping(ctx context.Context) error {
	it := c.client.Datasets(ctx)
	it.PageInfo().MaxSize = 1
	if _, err := it.Next(); err != nil && err != iterator.Done {
		return mapError(err)
	}
	return nil
}

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	if err := c.Ping(ctx); err != nil {
		return nil, err
	}
	info := &driver.ServerInfo{Product: "BigQuery", Version: "API v2", User: c.user, Database: c.project,
		Extras: map[string]string{"Authentication": c.auth}}
	if info.User == "" {
		info.User = c.auth
	}
	if c.location != "" {
		info.Extras["Location"] = c.location
	}
	if c.dataset != "" {
		info.Extras["Default dataset"] = c.dataset
	}
	if c.maxBytes > 0 {
		info.Extras["Cost limit"] = formatBytes(c.maxBytes) + " per query"
	} else {
		info.Extras["Cost limit"] = "none"
	}
	if c.endpoint != "" {
		info.Extras["Endpoint"] = c.endpoint
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

// Databases is empty: a connection is bound to one project.
func (c *conn) Databases(ctx context.Context) ([]driver.Database, error) { return nil, nil }

// Schemas lists the project's datasets.
func (c *conn) Schemas(ctx context.Context, database string) ([]driver.Schema, error) {
	it := c.client.Datasets(ctx)
	var out []driver.Schema
	for {
		ds, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return nil, mapError(err)
		}
		out = append(out, driver.Schema{Name: ds.DatasetID})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// datasetOf resolves a schema name to a project and dataset. "project.dataset"
// reaches datasets of other projects; an empty name means the default dataset.
func (c *conn) datasetOf(schema string) (project, dataset string, err error) {
	schema = strings.TrimSpace(schema)
	if schema == "" {
		schema = c.dataset
	}
	if schema == "" {
		return "", "", errors.New("choose a dataset")
	}
	if i := strings.LastIndexByte(schema, '.'); i > 0 {
		return schema[:i], schema[i+1:], nil
	}
	return c.project, schema, nil
}

// tableKind maps the API table type to an object kind.
func tableKind(t string) string {
	switch t {
	case "VIEW":
		return "view"
	case "MATERIALIZED_VIEW":
		return "materialized_view"
	case "EXTERNAL":
		return "external_table"
	}
	return "table" // TABLE, SNAPSHOT, CLONE
}

const (
	statsLimit   = 200 // tables whose row counts are fetched when listing a dataset
	statsWorkers = 8
)

// Objects lists tables, views and routines of a dataset. Row counts and sizes
// come from table metadata (free API calls, no query), fetched concurrently
// for the first statsLimit tables.
func (c *conn) Objects(ctx context.Context, s driver.Scope) ([]driver.Object, error) {
	project, dataset, err := c.datasetOf(s.Schema)
	if err != nil {
		return nil, err
	}
	var out []driver.Object
	err = c.svc.Tables.List(project, dataset).MaxResults(1000).Pages(ctx, func(page *bq.TableList) error {
		for _, t := range page.Tables {
			if t.TableReference == nil {
				continue
			}
			o := driver.Object{Name: t.TableReference.TableId, Kind: tableKind(t.Type)}
			if t.Type == "SNAPSHOT" || t.Type == "CLONE" {
				o.Extra = strings.ToLower(t.Type)
			}
			if t.FriendlyName != "" {
				o.Comment = t.FriendlyName
			}
			out = append(out, o)
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}
	c.tableStats(ctx, project, dataset, out)

	// Routines are best-effort: listing them needs bigquery.routines.list.
	_ = c.svc.Routines.List(project, dataset).MaxResults(1000).Pages(ctx, func(page *bq.ListRoutinesResponse) error {
		for _, r := range page.Routines {
			if r.RoutineReference == nil {
				continue
			}
			extra := strings.ToLower(strings.ReplaceAll(r.RoutineType, "_", " "))
			if r.Language != "" {
				extra = strings.TrimSpace(extra + " · " + r.Language)
			}
			out = append(out, driver.Object{Name: r.RoutineReference.RoutineId, Kind: "routine", Extra: extra})
		}
		return nil
	})
	return out, nil
}

// tableStats fills in row counts, sizes, descriptions and modification times.
func (c *conn) tableStats(ctx context.Context, project, dataset string, objs []driver.Object) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	jobs := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < statsWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				t, err := c.svc.Tables.Get(project, dataset, objs[i].Name).
					Fields("numRows", "numBytes", "description", "lastModifiedTime").Context(ctx).Do()
				if err != nil {
					continue
				}
				o := &objs[i]
				if o.Kind == "table" || o.Kind == "materialized_view" {
					n, size := int64(t.NumRows), t.NumBytes
					o.Rows, o.Size = &n, &size
				}
				if t.Description != "" {
					o.Comment = t.Description
				}
				if t.LastModifiedTime > 0 {
					o.Updated = time.UnixMilli(int64(t.LastModifiedTime)).UTC().Format("2006-01-02 15:04:05")
				}
			}
		}()
	}
	sent := 0
	for i := range objs {
		if sent == statsLimit || ctx.Err() != nil {
			break
		}
		if objs[i].Kind == "view" {
			continue // views have no storage; nothing worth a request
		}
		jobs <- i
		sent++
	}
	close(jobs)
	wg.Wait()
}

func quote(name string) string {
	return "`" + strings.NewReplacer(`\`, `\\`, "`", "\\`").Replace(name) + "`"
}

// tablePath renders `project.dataset.table` as one quoted identifier.
func tablePath(project, dataset, table string) string {
	return quote(project + "." + dataset + "." + table)
}

// formatBytes renders a byte count with binary units, as the BigQuery console does.
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	v := float64(n)
	i := -1
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	s := strconv.FormatFloat(v, 'f', 1, 64)
	s = strings.TrimSuffix(s, ".0")
	return s + " " + []string{"KB", "MB", "GB", "TB", "PB"}[i]
}
