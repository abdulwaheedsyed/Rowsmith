package redis

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"rowsmith/internal/driver"
)

// ---- administration -----------------------------------------------------------

// Processes lists client connections (CLIENT LIST); on a cluster, every
// node's, with the node in the first column and in each id.
func (c *conn) Processes(ctx context.Context) (*driver.Result, error) {
	if c.cluster != nil {
		return c.clusterProcesses(ctx)
	}
	s, err := c.client(c.defDB).ClientList(ctx).Result()
	if err != nil {
		return nil, mapError(err)
	}
	rs := clientTable(s)
	if rs.cols == nil {
		rs.cols = []driver.ResultColumn{rc("id", driver.KindInt)}
	}
	if rs.rows == nil {
		rs.rows = [][]any{}
	}
	return &driver.Result{Columns: rs.cols, Rows: rs.rows}, nil
}

func (c *conn) clusterProcesses(ctx context.Context) (*driver.Result, error) {
	var mu sync.Mutex
	var res *driver.Result
	err := c.cluster.ForEachShard(ctx, func(ctx context.Context, node *goredis.Client) error {
		s, err := node.ClientList(ctx).Result()
		if err != nil {
			return err
		}
		rs := clientTable(s)
		addr := node.Options().Addr
		mu.Lock()
		defer mu.Unlock()
		if res == nil {
			res = &driver.Result{Columns: append([]driver.ResultColumn{rc("id", driver.KindText), rc("node", driver.KindText)}, rs.cols...), Rows: [][]any{}}
		}
		for _, r := range rs.rows {
			row := make([]any, len(res.Columns))
			row[0], row[1] = fmt.Sprintf("%s/%v", addr, r[0]), addr
			// Columns can differ between versions: place each by name.
			for i, col := range rs.cols {
				for j := 2; j < len(res.Columns); j++ {
					if res.Columns[j].Name == col.Name && i < len(r) {
						row[j] = r[i]
					}
				}
			}
			res.Rows = append(res.Rows, row)
		}
		return nil
	})
	if err != nil {
		return nil, mapError(err)
	}
	if res == nil {
		res = &driver.Result{Columns: []driver.ResultColumn{rc("id", driver.KindText)}, Rows: [][]any{}}
	}
	sort.SliceStable(res.Rows, func(i, j int) bool { return fmt.Sprint(res.Rows[i][1]) < fmt.Sprint(res.Rows[j][1]) })
	return res, nil
}

func (c *conn) KillProcess(ctx context.Context, id string) error {
	if c.cluster != nil {
		// node/id, as clusterProcesses writes it.
		i := strings.LastIndex(id, "/")
		if i < 0 {
			return fmt.Errorf("invalid client id %q", id)
		}
		addr, cid := id[:i], id[i+1:]
		if _, err := strconv.ParseInt(cid, 10, 64); err != nil {
			return fmt.Errorf("invalid client id %q", id)
		}
		var n int64
		found := false
		var mu sync.Mutex
		err := c.cluster.ForEachShard(ctx, func(ctx context.Context, node *goredis.Client) error {
			if node.Options().Addr != addr {
				return nil
			}
			k, err := node.Do(ctx, "CLIENT", "KILL", "ID", cid).Int64()
			mu.Lock()
			n, found = k, true
			mu.Unlock()
			return err
		})
		if err != nil {
			return mapError(err)
		}
		if !found {
			return fmt.Errorf("node %s is no longer part of the cluster", addr)
		}
		if n == 0 {
			return fmt.Errorf("client %s is no longer connected", id)
		}
		return nil
	}
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		return fmt.Errorf("invalid client id %q", id)
	}
	n, err := c.client(c.defDB).Do(ctx, "CLIENT", "KILL", "ID", id).Int64()
	if err != nil {
		return mapError(err)
	}
	if n == 0 {
		return fmt.Errorf("client %s is no longer connected", id)
	}
	return nil
}

// Variables shows CONFIG GET * ("variables") or INFO ("status").
func (c *conn) Variables(ctx context.Context, kind string) (*driver.Result, error) {
	cl := c.client(c.defDB)
	if kind == "status" {
		s, err := cl.Info(ctx, "everything").Result()
		if err != nil {
			return nil, mapError(err)
		}
		rs := infoTable(s)
		return &driver.Result{Columns: rs.cols, Rows: rs.rows}, nil
	}
	cfg, err := cl.ConfigGet(ctx, "*").Result()
	if err != nil {
		return nil, mapError(err)
	}
	names := make([]string, 0, len(cfg))
	for k := range cfg {
		names = append(names, k)
	}
	sort.Strings(names)
	res := &driver.Result{Columns: []driver.ResultColumn{rc("name", driver.KindText), rc("value", driver.KindText)}, Rows: make([][]any, 0, len(names))}
	for _, k := range names {
		res.Rows = append(res.Rows, []any{k, cfg[k]})
	}
	return res, nil
}

// ---- key changes, previewed and run through the console ----------------------

func (c *conn) DropObjectSQL(ref driver.ObjectRef, _ bool) ([]string, error) {
	return []string{"DEL " + quoteArg(ref.Name)}, nil
}

// RenameObjectSQL renames a key, refusing a name another key has: RENAME
// would replace that key.
func (c *conn) RenameObjectSQL(ref driver.ObjectRef, newName string) ([]string, error) {
	if newName == "" {
		return nil, fmt.Errorf("enter the new key name")
	}
	cl, err := c.clientFor(ref.Database)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if n, err := cl.Exists(ctx, newName).Result(); err != nil {
		return nil, mapError(err)
	} else if n > 0 && newName != ref.Name {
		return nil, fmt.Errorf("a key named %q already exists", newName)
	}
	return []string{"RENAME " + quoteArg(ref.Name) + " " + quoteArg(newName)}, nil
}

func (c *conn) TruncateSQL(driver.ObjectRef) ([]string, error)   { return nil, driver.ErrNotSupported }
func (c *conn) CreateTableSQL(driver.TableDef) ([]string, error) { return nil, driver.ErrNotSupported }
func (c *conn) AlterTableSQL(*driver.Table, driver.TableDef) ([]string, error) {
	return nil, driver.ErrNotSupported
}
func (c *conn) CreateDatabaseSQL(string, map[string]string) ([]string, error) {
	return nil, driver.ErrNotSupported
}
func (c *conn) DropDatabaseSQL(string, bool) ([]string, error) { return nil, driver.ErrNotSupported }
