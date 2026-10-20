// Package dynamodb implements the Amazon DynamoDB driver on the AWS SDK for
// Go: tables with their keys, indexes, capacity, TTL and streams; a grid that
// queries by key when it can and scans otherwise; edits applied in one
// transaction with conditions, so an insert never overwrites an item and a
// change never brings a deleted one back; a console for PartiQL and the
// DynamoDB API; imports, exports and migrations.
package dynamodb

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"

	"rowsmith/internal/driver"
)

type ddbDriver struct{}

func init() { driver.Register(ddbDriver{}) }

var kinds = []driver.KindInfo{{Kind: "table", Label: "Tables", Icon: "table", Browse: true}}

// The structure editor's types: the key attribute types, and the others
// Rowsmith records for a table so imports know them.
var attributeTypes = []string{tS, tN, tB, tBOOL, tM, tL, tSS, tNS, tBS}

var design = driver.TableDesign{
	Columns: true, PrimaryKey: true, Indexes: true, IndexTypes: []string{"global", "local"},
	Options: []driver.Field{
		{Key: "billing", Label: "Capacity", Type: driver.FieldSelect, Span: 2, Options: []driver.Option{
			{Value: "PAY_PER_REQUEST", Label: "On demand"}, {Value: "PROVISIONED", Label: "Provisioned"}}},
		{Key: "read_capacity", Label: "Read units", Type: driver.FieldNumber, Span: 2, Placeholder: "5", ShowIf: map[string][]string{"billing": {"PROVISIONED"}}},
		{Key: "write_capacity", Label: "Write units", Type: driver.FieldNumber, Span: 2, Placeholder: "5", ShowIf: map[string][]string{"billing": {"PROVISIONED"}}},
		{Key: "ttl_attribute", Label: "Time-to-live attribute", Type: driver.FieldText, Span: 3, Placeholder: "e.g. expires_at (epoch seconds)"},
		{Key: "deletion_protection", Label: "Deletion protection", Type: driver.FieldSelect, Span: 3, Options: []driver.Option{
			{Value: "", Label: "Off"}, {Value: "on", Label: "On"}}},
	},
	Note: "The primary key is the partition key and an optional sort key, fixed when the table is created. Other attributes need no declaring: Rowsmith keeps their types so imports and edits write them as that type.",
}

func (ddbDriver) Info() driver.Info {
	fields := []driver.Field{
		{Key: "region", Label: "Region", Type: driver.FieldText, Required: true, Placeholder: "us-east-1", Span: 3},
		{Key: "endpoint", Label: "Endpoint", Type: driver.FieldText, Span: 3, Placeholder: "optional — e.g. http://localhost:8000",
			Help: "For DynamoDB Local, a VPC endpoint or a compatible service. Leave empty for AWS."},
		{Key: "auth", Label: "Sign in with", Type: driver.FieldSelect, Default: "keys", Section: "auth", Options: []driver.Option{
			{Value: "keys", Label: "Access key"}, {Value: "default", Label: "This server's AWS credentials (environment, profile or role)"}}},
		{Key: "accessKeyId", Label: "Access key ID", Type: driver.FieldText, Section: "auth", Span: 3, ShowIf: map[string][]string{"auth": {"keys"}}},
		{Key: "secretAccessKey", Label: "Secret access key", Type: driver.FieldPassword, Secret: true, Section: "auth", Span: 3, ShowIf: map[string][]string{"auth": {"keys"}}},
		{Key: "sessionToken", Label: "Session token", Type: driver.FieldPassword, Secret: true, Section: "auth", ShowIf: map[string][]string{"auth": {"keys"}},
			Help: "Only for temporary credentials."},
		{Key: "roleArn", Label: "Role to assume", Type: driver.FieldText, Section: "auth", Placeholder: "optional — arn:aws:iam::123456789012:role/reader"},
		{Key: "tablePrefix", Label: "Table prefix", Type: driver.FieldText, Section: "advanced", Placeholder: "optional — list only tables starting with it"},
		{Key: "timeout", Label: "Request timeout (seconds)", Type: driver.FieldNumber, Default: 60, Section: "advanced", Span: 3},
	}
	return driver.Info{
		ID: "dynamodb", Name: "DynamoDB", Order: 74,
		Description: "Amazon DynamoDB in any region, and DynamoDB Local",
		Dialect:     "partiql", Fields: fields, SSH: true, QuoteChar: `"`,
		Caps:  driver.Caps{EditRows: true, DDL: true},
		Kinds: kinds, Types: attributeTypes, URLSchemes: []string{"dynamodb"}, Design: &design,
	}
}

type conn struct {
	db       *dynamodb.Client
	ro       bool
	region   string
	endpoint string
	prefix   string
	account  string

	// The console sends API calls as written, signed with these.
	creds aws.CredentialsProvider
	hc    *http.Client
	url   string

	mu      sync.Mutex
	samples map[string]sampled // table → attributes seen, for a while
}

type sampled struct {
	at    time.Time
	attrs map[string]string // attribute → type
}

func (ddbDriver) Open(ctx context.Context, p driver.OpenParams) (driver.Conn, error) {
	region := strings.TrimSpace(p.String("region"))
	if region == "" {
		return nil, errors.New("enter the region, such as us-east-1")
	}
	hc := &http.Transport{Proxy: http.ProxyFromEnvironment, MaxIdleConnsPerHost: 16, IdleConnTimeout: 90 * time.Second, ForceAttemptHTTP2: true}
	if p.Dial != nil {
		dial := p.Dial
		hc.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) { return dial(ctx, network, addr) }
	}
	client := &http.Client{Transport: hc, Timeout: time.Duration(p.Int("timeout", 60)) * time.Second}
	// The SDK warns of response bodies it closes twice; nothing to act on.
	opts := []func(*config.LoadOptions) error{config.WithRegion(region), config.WithHTTPClient(client), config.WithLogger(logging.Nop{}),
		config.WithAppID("rowsmith")}
	if p.String("auth") != "default" {
		id, secret := strings.TrimSpace(p.String("accessKeyId")), p.Secret("secretAccessKey")
		if id == "" || secret == "" {
			return nil, errors.New("enter the access key ID and secret access key")
		}
		opts = append(opts, config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(id, secret, p.Secret("sessionToken"))))
	}
	cfg, err := config.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	if arn := strings.TrimSpace(p.String("roleArn")); arn != "" {
		cfg.Credentials = aws.NewCredentialsCache(stscreds.NewAssumeRoleProvider(sts.NewFromConfig(cfg), arn,
			func(o *stscreds.AssumeRoleOptions) { o.RoleSessionName = "rowsmith" }))
	}
	endpoint := strings.TrimSpace(p.String("endpoint"))
	c := &conn{ro: p.ReadOnly, region: region, endpoint: endpoint, prefix: strings.TrimSpace(p.String("tablePrefix")), samples: map[string]sampled{},
		creds: cfg.Credentials, hc: client, url: endpoint}
	if c.url == "" {
		ep, err := dynamodb.NewDefaultEndpointResolverV2().ResolveEndpoint(ctx, dynamodb.EndpointParameters{Region: aws.String(region)})
		if err != nil {
			return nil, err
		}
		c.url = ep.URI.String()
	}
	c.db = dynamodb.NewFromConfig(cfg, func(o *dynamodb.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
		}
	})
	if _, err := c.db.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(1)}); err != nil {
		return nil, mapError(err)
	}
	if endpoint == "" {
		if id, err := sts.NewFromConfig(cfg).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{}); err == nil {
			c.account = aws.ToString(id.Arn)
		}
	}
	return c, nil
}

func (c *conn) Close() error { return nil }

func (c *conn) Ping(ctx context.Context) error {
	_, err := c.db.ListTables(ctx, &dynamodb.ListTablesInput{Limit: aws.Int32(1)})
	return mapError(err)
}

func (c *conn) Server(ctx context.Context) (*driver.ServerInfo, error) {
	info := &driver.ServerInfo{Product: "Amazon DynamoDB", Version: c.region, User: c.account, Extras: map[string]string{"Region": c.region}}
	if c.endpoint != "" {
		info.Product, info.Extras["Endpoint"] = "DynamoDB", c.endpoint
	}
	if c.ro {
		info.Extras["Session"] = "read-only"
	}
	return info, nil
}

func (c *conn) Databases(context.Context) ([]driver.Database, error)       { return nil, nil }
func (c *conn) Schemas(context.Context, string) ([]driver.Schema, error) { return nil, nil }

func (c *conn) tableNames(ctx context.Context) ([]string, error) {
	var names []string
	in := &dynamodb.ListTablesInput{}
	if c.prefix != "" {
		in.ExclusiveStartTableName = aws.String(c.prefix)
	}
	p := dynamodb.NewListTablesPaginator(c.db, in)
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, mapError(err)
		}
		for _, n := range page.TableNames {
			if c.prefix != "" && !strings.HasPrefix(n, c.prefix) {
				if n > c.prefix {
					return names, nil // tables are listed in name order
				}
				continue
			}
			names = append(names, n)
		}
	}
	return names, nil
}

// Objects lists the tables with their (approximate) item counts and sizes,
// which DynamoDB refreshes about every six hours.
func (c *conn) Objects(ctx context.Context, _ driver.Scope) ([]driver.Object, error) {
	names, err := c.tableNames(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]driver.Object, len(names))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, n := range names {
		out[i] = driver.Object{Name: n, Kind: "table"}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			d, err := c.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(n)})
			if err != nil || d.Table == nil {
				return
			}
			out[i].Rows, out[i].Size = d.Table.ItemCount, d.Table.TableSizeBytes
			out[i].Engine = strings.ToLower(string(d.Table.TableStatus))
		}()
	}
	wg.Wait()
	return out, nil
}

// ---- describing tables ----------------------------------------------------------------

// typeTag records the types of attributes outside the key, which DynamoDB
// doesn't keep: rowsmith:types:<n> = "total=N items=L …".
const typeTag = "rowsmith:types:"

func (c *conn) declaredTypes(ctx context.Context, arn *string) map[string]string {
	out := map[string]string{}
	if arn == nil {
		return out
	}
	tags, err := c.db.ListTagsOfResource(ctx, &dynamodb.ListTagsOfResourceInput{ResourceArn: arn})
	if err != nil {
		return out
	}
	for _, t := range tags.Tags {
		if !strings.HasPrefix(aws.ToString(t.Key), typeTag) {
			continue
		}
		for _, pair := range strings.Fields(aws.ToString(t.Value)) {
			if name, typ, ok := strings.Cut(pair, "="); ok {
				out[name] = typ
			}
		}
	}
	return out
}

// sample reads up to 100 items to find the attributes outside the key,
// cached for a minute.
func (c *conn) sample(ctx context.Context, table string) map[string]string {
	c.mu.Lock()
	s, ok := c.samples[table]
	c.mu.Unlock()
	if ok && time.Since(s.at) < time.Minute {
		return s.attrs
	}
	attrs := map[string]string{}
	out, err := c.db.Scan(ctx, &dynamodb.ScanInput{TableName: aws.String(table), Limit: aws.Int32(100)})
	if err == nil {
		for _, item := range out.Items {
			for k, v := range item {
				t := typeOf(v)
				switch prev, seen := attrs[k]; {
				case !seen || prev == tNULL:
					attrs[k] = t
				case prev != t && t != tNULL:
					attrs[k] = tMixed
				}
			}
		}
	}
	c.mu.Lock()
	c.samples[table] = sampled{at: time.Now(), attrs: attrs}
	c.mu.Unlock()
	return attrs
}

func (c *conn) forget(table string) {
	c.mu.Lock()
	delete(c.samples, table)
	c.mu.Unlock()
}

func keyNames(ks []types.KeySchemaElement) (hash, rng string) {
	for _, k := range ks {
		if k.KeyType == types.KeyTypeHash {
			hash = aws.ToString(k.AttributeName)
		} else {
			rng = aws.ToString(k.AttributeName)
		}
	}
	return
}

func projection(p *types.Projection) string {
	if p == nil {
		return ""
	}
	if p.ProjectionType == types.ProjectionTypeInclude {
		return "INCLUDE " + strings.Join(p.NonKeyAttributes, ", ")
	}
	return string(p.ProjectionType)
}

// layout is what browsing and editing need beyond driver.Table.
type layout struct {
	hash, rng string
	types     map[string]string // every known attribute's type
	indexes   []index
}

type index struct {
	name, hash, rng string
	all             bool // projects every attribute
	global          bool
}

func (c *conn) describe(ctx context.Context, ref driver.ObjectRef, full bool) (*driver.Table, *layout, error) {
	d, err := c.db.DescribeTable(ctx, &dynamodb.DescribeTableInput{TableName: aws.String(ref.Name)})
	if err != nil {
		return nil, nil, mapError(err)
	}
	td := d.Table
	l := &layout{types: map[string]string{}}
	l.hash, l.rng = keyNames(td.KeySchema)
	for _, a := range td.AttributeDefinitions {
		l.types[aws.ToString(a.AttributeName)] = string(a.AttributeType)
	}
	for k, v := range c.declaredTypes(ctx, td.TableArn) {
		if _, key := l.types[k]; !key {
			l.types[k] = v
		}
	}
	for k, v := range c.sample(ctx, ref.Name) {
		if _, known := l.types[k]; !known {
			l.types[k] = v
		}
	}
	t := &driver.Table{Ref: driver.ObjectRef{Name: ref.Name, Kind: "table"}, Kind: "table", Options: map[string]string{}, Indexes: []driver.Index{},
		ForeignKeys: []driver.ForeignKey{}, Referenced: []driver.ForeignKey{}, Checks: []driver.Check{}, Triggers: []driver.Trigger{}}
	keyCols := []string{l.hash}
	if l.rng != "" {
		keyCols = append(keyCols, l.rng)
	}
	names := make([]string, 0, len(l.types))
	for n := range l.types {
		if n != l.hash && n != l.rng {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range append(keyCols, names...) {
		typ := l.types[n]
		col := driver.Column{Name: n, Type: typ, BaseType: strings.ToLower(typ), Kind: kindOf(typ), Nullable: n != l.hash && n != l.rng}
		col.PrimaryKey = !col.Nullable
		if n == l.hash {
			col.Comment = "partition key"
		} else if n == l.rng {
			col.Comment = "sort key"
		}
		t.Columns = append(t.Columns, col)
	}
	t.PrimaryKey, t.RowKey, t.RowKeyKind = keyCols, keyCols, "primary"
	t.Indexes = append(t.Indexes, driver.Index{Name: "PRIMARY KEY", Columns: keyCols, Primary: true, Unique: true})
	add := func(name *string, ks []types.KeySchemaElement, p *types.Projection, global bool) {
		h, r := keyNames(ks)
		ix := index{name: aws.ToString(name), hash: h, rng: r, all: p != nil && p.ProjectionType == types.ProjectionTypeAll, global: global}
		l.indexes = append(l.indexes, ix)
		cols := []string{h}
		if r != "" {
			cols = append(cols, r)
		}
		typ := "local"
		if global {
			typ = "global"
		}
		t.Indexes = append(t.Indexes, driver.Index{Name: ix.name, Columns: cols, Type: typ, Definition: "projects " + projection(p)})
	}
	for _, g := range td.GlobalSecondaryIndexes {
		add(g.IndexName, g.KeySchema, g.Projection, true)
	}
	for _, li := range td.LocalSecondaryIndexes {
		add(li.IndexName, li.KeySchema, li.Projection, false)
	}
	t.RowEstimate, t.Size = td.ItemCount, td.TableSizeBytes
	t.Options["partition_key"] = l.hash
	if l.rng != "" {
		t.Options["sort_key"] = l.rng
	}
	t.Options["billing"] = "PROVISIONED"
	if td.BillingModeSummary != nil && td.BillingModeSummary.BillingMode == types.BillingModePayPerRequest {
		t.Options["billing"] = "PAY_PER_REQUEST"
	}
	if t.Options["billing"] == "PROVISIONED" && td.ProvisionedThroughput != nil {
		t.Options["read_capacity"] = strconv.FormatInt(aws.ToInt64(td.ProvisionedThroughput.ReadCapacityUnits), 10)
		t.Options["write_capacity"] = strconv.FormatInt(aws.ToInt64(td.ProvisionedThroughput.WriteCapacityUnits), 10)
	}
	t.Options["status"] = strings.ToLower(string(td.TableStatus))
	t.Options["arn"] = aws.ToString(td.TableArn)
	if aws.ToBool(td.DeletionProtectionEnabled) {
		t.Options["deletion_protection"] = "on"
	}
	t.Editable = !c.ro
	if !full {
		return t, l, nil
	}
	if td.StreamSpecification != nil && aws.ToBool(td.StreamSpecification.StreamEnabled) {
		t.Options["stream"] = strings.ToLower(string(td.StreamSpecification.StreamViewType))
	}
	if td.TableClassSummary != nil {
		t.Options["table_class"] = strings.ToLower(string(td.TableClassSummary.TableClass))
	}
	if ttl, err := c.db.DescribeTimeToLive(ctx, &dynamodb.DescribeTimeToLiveInput{TableName: aws.String(ref.Name)}); err == nil && ttl.TimeToLiveDescription != nil &&
		ttl.TimeToLiveDescription.TimeToLiveStatus == types.TimeToLiveStatusEnabled {
		t.Options["ttl_attribute"] = aws.ToString(ttl.TimeToLiveDescription.AttributeName)
	}
	if pitr, err := c.db.DescribeContinuousBackups(ctx, &dynamodb.DescribeContinuousBackupsInput{TableName: aws.String(ref.Name)}); err == nil &&
		pitr.ContinuousBackupsDescription != nil && pitr.ContinuousBackupsDescription.PointInTimeRecoveryDescription != nil &&
		pitr.ContinuousBackupsDescription.PointInTimeRecoveryDescription.PointInTimeRecoveryStatus == types.PointInTimeRecoveryStatusEnabled {
		t.Options["point_in_time_recovery"] = "on"
	}
	if td.CreationDateTime != nil {
		t.Options["created"] = td.CreationDateTime.UTC().Format("2006-01-02 15:04 UTC")
	}
	t.DDL = createRequest(t, l)
	return t, l, nil
}

func (c *conn) Describe(ctx context.Context, ref driver.ObjectRef) (*driver.Table, error) {
	t, _, err := c.describe(ctx, ref, true)
	return t, err
}

// ---- errors --------------------------------------------------------------------------------

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var ae smithy.APIError
	if errors.As(err, &ae) {
		msg := ae.ErrorMessage()
		switch ae.ErrorCode() {
		case "ResourceNotFoundException":
			if msg == "" {
				msg = "the table does not exist"
			}
		case "UnrecognizedClientException", "InvalidSignatureException":
			msg = "Sign-in failed: check the access key and secret. " + msg
		case "ProvisionedThroughputExceededException", "ThrottlingException":
			msg += " — the table's capacity is used up for now; try again shortly"
		}
		return &driver.QueryError{Message: msg, Code: ae.ErrorCode()}
	}
	return err
}

func (c *conn) Classify(stmt string) driver.StatementKind {
	reqs, err := parseScript(stmt)
	if err != nil || len(reqs) == 0 {
		return driver.StmtUnknown
	}
	return reqs[0].kind()
}

func errorf(format string, a ...any) error { return &driver.QueryError{Message: fmt.Sprintf(format, a...)} }
