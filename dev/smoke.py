#!/usr/bin/env python3
"""End-to-end smoke test against a running dev stack (dev/compose.yaml)."""
import json, os, re, subprocess, sys, time, urllib.request, http.cookiejar

BASE = os.environ.get("ROWSMITH_URL", "http://127.0.0.1:18080")
env = dict(l.strip().split("=", 1) for l in open(os.path.join(os.path.dirname(__file__), ".env")) if "=" in l)
PW = env["DEV_DB_PASSWORD"]
VIEWER = f"viewer-{int(time.time())}@example.com"
OWNER = {"email": "owner@example.com", "password": "Correct-Horse-Battery-9", "name": "Olivia Owner"}

class Client:
    def __init__(self):
        self.jar = http.cookiejar.CookieJar()
        self.op = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(self.jar))
        self.csrf = ""
    def req(self, method, path, body=None, expect=None, raw=False):
        out = self._req(method, path, body, expect, raw)
        if method == "POST" and path == "/api/connections" and isinstance(out[1], dict) and "id" in out[1]:
            created.add(out[1]["id"])
        return out
    def _req(self, method, path, body=None, expect=None, raw=False):
        data = json.dumps(body).encode() if body is not None else None
        r = urllib.request.Request(BASE + path, data=data, method=method)
        r.add_header("Content-Type", "application/json")
        r.add_header("Origin", BASE)
        if self.csrf: r.add_header("X-CSRF-Token", self.csrf)
        try:
            resp = self.op.open(r, timeout=120); code = resp.status; text = resp.read().decode()
        except urllib.error.HTTPError as e:
            code = e.code; text = e.read().decode()
        if expect is not None and code != expect:
            raise SystemExit(f"FAIL {method} {path}: expected {expect}, got {code}: {text[:600]}")
        if raw: return code, text
        try: return code, json.loads(text)
        except Exception: return code, text
    def login(self, email, password):
        self.req("POST", "/api/auth/login", {"email": email, "password": password}, 200)
        _, me = self.req("GET", "/api/auth/me", expect=200); self.csrf = me["csrf"]; return me

def ok(msg): print("  ✓", msg)

def stream(c, cid, sql, **kw):
    code, text = c.req("POST", f"/api/c/{cid}/query", {"sql": sql, "tab": "smoke", **kw}, raw=True)
    if code != 200: return code, text
    return code, [json.loads(l) for l in text.splitlines() if l.strip()]

created = set()

def main():
    c = Client()
    _, boot = c.req("GET", "/api/bootstrap", expect=200)
    if boot["setupRequired"]:
        logs = subprocess.run(["docker", "compose", "-f", "dev/compose.yaml", "--env-file", "dev/.env", "logs", "rowsmith"], capture_output=True, text=True).stdout
        token = re.findall(r"setup code: ([A-Z0-9-]+)", logs)[-1]
        c.req("POST", "/api/setup", {"token": "WRONG-CODE-0000", **OWNER}, 403); ok("setup rejects a wrong code")
        c.req("POST", "/api/setup", {"token": token, **OWNER}, 200); ok("first-run setup created the owner")
    c.login(OWNER["email"], OWNER["password"]); ok("owner signed in")

    # CSRF protections
    saved = c.csrf; c.csrf = "nope"
    c.req("POST", "/api/connections", {"name": "x"}, 403); c.csrf = saved; ok("CSRF token enforced")

    _, drivers = c.req("GET", "/api/drivers", expect=200)
    ok("drivers: " + ", ".join(d["id"] for d in drivers))

    conns = {}
    for name, drv, host, user, db in [("Shop (MySQL)", "mysql", "mysql", "root", ""), ("Shop (MariaDB)", "mariadb", "mariadb", "root", ""),
                                      ("City (PostGIS)", "postgres", "postgis", "postgres", "city")]:
        _, cv = c.req("POST", "/api/connections", {"name": name, "driver": drv, "environment": "development",
            "params": {"host": host, "user": user, "database": db, "tls": "disable"}, "secrets": {"password": PW}}, 201)
        assert "password" not in json.dumps(cv["params"]), "secret leaked into params"
        conns[drv] = cv["id"]
    ok("created 3 connections (secrets stored write-only)")

    for drv in ("mysql", "mariadb"):
        cid = conns[drv]
        for _ in range(60):
            code, dbs = c.req("GET", f"/api/c/{cid}/databases")
            if code == 200 and any(d["name"] == "analytics" for d in dbs): break
            time.sleep(3)
        else: raise SystemExit(f"FAIL {drv} never became ready: {dbs}")
        _, objs = c.req("GET", f"/api/c/{cid}/objects?db=shop", expect=200)
        kinds = sorted({o["kind"] for o in objs}); ok(f"{drv}: {len(objs)} objects, kinds {kinds}")
        _, t = c.req("GET", f"/api/c/{cid}/describe?db=shop&name=products", expect=200)
        assert t["primaryKey"] == ["id"] and any(col.get("generated") for col in t["columns"]), t["columns"]
        assert any(fk["refTable"]["name"] == "categories" for fk in t["foreignKeys"])
        ok(f"{drv}: describe products (PK, generated column, FK, {len(t['indexes'])} indexes)")
        _, res = c.req("POST", f"/api/c/{cid}/browse", {"ref": {"database": "shop", "name": "products"}, "limit": 5,
            "sort": [{"column": "price", "desc": True}], "filters": [{"column": "stock", "op": ">", "value": 10}]}, 200)
        assert len(res["rows"]) == 5 and res["truncated"], res
        thumb = [r for r in res["rows"]]; ok(f"{drv}: browse with filter+sort, SQL: {res['sql'][:70]}...")
        _, res = c.req("POST", f"/api/c/{cid}/browse", {"ref": {"database": "shop", "name": "stores"}, "limit": 10}, 200)
        geo = res["rows"][0][res["columns"].index(next(x for x in res["columns"] if x["name"] == "location"))]
        assert "$geo" in geo, geo; ok(f"{drv}: geometry cell -> {json.dumps(geo)[:60]}")
        _, res = c.req("POST", f"/api/c/{cid}/browse", {"ref": {"database": "shop", "name": "products"}, "limit": 1}, 200)
        thumb = res["rows"][0][[x["name"] for x in res["columns"]].index("thumbnail")]
        assert thumb and thumb.get("mime") == "image/png", thumb; ok(f"{drv}: BLOB sniffed as {thumb['mime']}")
        _, n = c.req("POST", f"/api/c/{cid}/count", {"ref": {"database": "shop", "name": "orders"}, "filters": [{"column": "status", "op": "=", "value": "paid"}]}, 200)
        ok(f"{drv}: count paid orders = {n['rows']}")
        # edits
        _, er = c.req("POST", f"/api/c/{cid}/edit", {"ref": {"database": "shop", "name": "customers"}, "edits": [
            {"op": "update", "key": {"id": 1}, "values": {"full_name": "Ava Updated", "tier": "enterprise"}},
            {"op": "insert", "values": {"email": f"new-{drv}-{time.time_ns()}@example.com", "full_name": "New Person", "preferences": '{"a":1}'}}]}, 200)
        ok(f"{drv}: grid edits applied ({er['applied']})")
        _, ae = c.req("POST", f"/api/c/{cid}/browse", {"ref": {"database": "shop", "name": "audit_events"}, "limit": 1,
            "filters": [{"column": "actor", "op": "=", "value": "admin"}]}, 200)
        names = [x["name"] for x in ae["columns"]]; row = dict(zip(names, ae["rows"][0]))
        c.req("POST", f"/api/c/{cid}/edit", {"ref": {"database": "shop", "name": "audit_events"}, "edits": [
            {"op": "delete", "key": {k: row[k] for k in ("happened_at", "actor", "action")}}]}, 200)
        ok(f"{drv}: delete on keyless table (all-columns match + LIMIT 1)")
        # streaming console
        code, ev = stream(c, cid, "SELECT COUNT(*) AS n FROM orders; SELECT id, status, total FROM orders ORDER BY id LIMIT 3;\nUPDATE products SET stock = stock WHERE id = 1;", database="shop")
        assert code == 200, ev
        ends = [e for e in ev if e["t"] == "stmtEnd"]; errs = [e.get("error") for e in ends if e.get("error")]
        assert len(ends) == 3 and not errs, ev
        ok(f"{drv}: console streamed {len(ends)} statements, {sum(len(e['rows']) for e in ev if e['t']=='rows')} rows")
        code, ev = stream(c, cid, "SELEC broken", database="shop")
        err = next(e["error"] for e in ev if e["t"] == "stmtEnd"); ok(f"{drv}: syntax error surfaced: {err['message'][:60]}")
        code, body = stream(c, cid, "DELETE FROM audit_events", database="shop")
        assert code == 409 and "confirm_required" in body, body; ok(f"{drv}: DELETE without WHERE requires confirmation")
        code, ev = stream(c, cid, "CALL restock(1, 5)", database="shop")
        assert not [e for e in ev if e["t"] == "stmtEnd" and e.get("error")], ev; ok(f"{drv}: CALL procedure returned result set")
        _, plan = c.req("POST", f"/api/c/{cid}/explain", {"database": "shop", "sql": "SELECT * FROM orders o JOIN customers c ON c.id = o.customer_id WHERE o.status = 'paid' ORDER BY o.placed_at DESC LIMIT 10"}, 200)
        ok(f"{drv}: explain ({plan['format']}) root: {plan['root']['operation'] if plan.get('root') else None}")
        _, pl = c.req("GET", f"/api/c/{cid}/processes", expect=200); ok(f"{drv}: processlist {len(pl['rows'])} rows")

    cid = conns["postgres"]
    _, schemas = c.req("GET", f"/api/c/{cid}/schemas?db=city", expect=200); ok("postgres schemas: " + ", ".join(s["name"] for s in schemas))
    _, objs = c.req("GET", f"/api/c/{cid}/objects?db=city&schema=transit", expect=200)
    ok("postgres transit objects: " + ", ".join(f"{o['name']}({o['kind']})" for o in objs))
    _, info = c.req("GET", f"/api/c/{cid}/server", expect=200); ok(f"postgres server {info['server']['version']} extras {info['server'].get('extras')}")
    _, t = c.req("GET", f"/api/c/{cid}/describe?db=city&schema=transit&name=stations", expect=200)
    loc = next(col for col in t["columns"] if col["name"] == "location"); assert loc["kind"] == "geometry" and loc["srid"] == 4326, loc
    ok("postgres describe stations: geometry(Point,4326), enum mode " + str(next(col for col in t['columns'] if col['name']=='mode').get('enum')))
    _, res = c.req("POST", f"/api/c/{cid}/browse", {"ref": {"database": "city", "schema": "transit", "name": "stations"}, "limit": 3, "search": "Central"}, 200)
    ok(f"postgres browse w/ search: {len(res['rows'])} rows; cell sample {json.dumps(res['rows'][0][:3])[:90]}")
    _, t = c.req("GET", f"/api/c/{cid}/describe?db=city&name=incident_log", expect=200)
    assert t["rowKeyKind"] == "rowid", t["rowKeyKind"]
    _, res = c.req("POST", f"/api/c/{cid}/browse", {"ref": {"database": "city", "name": "incident_log"}, "limit": 1}, 200)
    ctid = res["rows"][0][[x["name"] for x in res["columns"]].index("__rowsmith_rowid")]
    c.req("POST", f"/api/c/{cid}/edit", {"ref": {"database": "city", "name": "incident_log"}, "edits": [{"op": "update", "key": {"__rowsmith_rowid": ctid}, "values": {"detail": "edited via ctid"}}]}, 200)
    ok(f"postgres ctid edit on keyless table ({ctid})")
    code, ev = stream(c, cid, "UPDATE districts SET population = population WHERE id = 1; SELECT name, ST_AsText(ST_Centroid(boundary)) c, boundary FROM districts LIMIT 2; SELECT * FROM transit.nearest_stations(4.9, 52.35, 3)", database="city")
    notices = [e for e in ev if e["t"] == "notice"]; geos = [r for e in ev if e["t"] == "rows" for row in e["rows"] for r in row if isinstance(r, dict) and "$geo" in r]
    assert notices and geos, ev[:6]; ok(f"postgres console: notice '{notices[0]['text']}', {len(geos)} geometry cells decoded from EWKB")
    code, ev = stream(c, cid, "BEGIN; UPDATE districts SET population = 1 WHERE id = 2;", database="city")
    assert ev[-1]["inTx"] is True, ev[-1]
    code, ev = stream(c, cid, "SELECT population FROM districts WHERE id = 2; ROLLBACK;", database="city")
    ok(f"postgres transaction spans runs (inTx after BEGIN; ROLLBACK -> inTx={ev[-1]['inTx']})")
    _, plan = c.req("POST", f"/api/c/{cid}/explain", {"database": "city", "sql": "SELECT * FROM transit.stations s JOIN districts d ON d.id = s.district_id WHERE ST_DWithin(s.location, ST_SetSRID(ST_MakePoint(4.9,52.35),4326), 0.01)", "analyze": True}, 200)
    ok(f"postgres explain analyze root {plan['root']['operation']} time {plan['totals']}")

    # Access management: previews hide passwords; changes run, show both ways and are audited without them
    pg = conns["postgres"]
    _, m = c.req("GET", f"/api/c/{pg}/access/model", expect=200)
    assert m["supported"] and any(l["level"] == "table" for l in m["model"]["levels"]), m
    acct = f"smoke_{int(time.time())}"
    create = {"account": {"op": "create", "kind": "user", "name": acct, "password": "Hidden-Pass-123"}}
    _, pv = c.req("POST", f"/api/c/{pg}/access/preview", create, 200)
    assert "Hidden-Pass-123" not in json.dumps(pv) and "SCRAM" not in json.dumps(pv), pv; ok("access: preview shows the password redacted")
    c.req("POST", f"/api/c/{pg}/access/apply", create, 200)
    c.req("POST", f"/api/c/{pg}/access/apply", {"grants": [
        {"principal": acct, "level": "schema", "database": "city", "schema": "public", "privilege": "USAGE"},
        {"principal": acct, "level": "table", "database": "city", "schema": "public", "object": "districts", "privilege": "SELECT"}]}, 200)
    _, gs = c.req("GET", f"/api/c/{pg}/access/grants?principal={acct}&db=city&schema=public", expect=200)
    assert any(g["level"] == "table" and g.get("object") == "districts" and g["privilege"] == "SELECT" and not g.get("via") for g in gs), gs
    _, oa = c.req("GET", f"/api/c/{pg}/access/object?db=city&schema=public&name=districts", expect=200)
    assert any(g["principal"] == acct for g in oa), oa; ok(f"access: created {acct} and granted SELECT on districts; it shows on the user and on the table")
    c.req("POST", f"/api/c/{pg}/access/apply", {"account": {"op": "drop", "kind": "user", "name": acct, "attrs": {"reassign": "true"}}, "database": "city"}, 200)
    _, audit = c.req("GET", "/api/audit?limit=20", expect=200)
    assert [a for a in audit if a["action"] == "access.changed"] and not [a for a in audit if "Hidden-Pass-123" in json.dumps(a)], audit
    ok("access: dropped it again; the audit log records the changes without the password")

    # SSH tunnel to a database only reachable from the bastion
    ssh = {"enabled": True, "hops": [{"host": "bastion", "port": 22, "user": "tunnel", "auth": "password"}]}
    _, cv = c.req("POST", "/api/connections", {"name": "Vault via bastion", "driver": "postgres", "environment": "production",
        "params": {"host": "private-pg", "user": "postgres", "database": "vault", "tls": "disable"},
        "secrets": {"password": PW, "ssh.0.password": env["DEV_SSH_PASSWORD"]}, "ssh": ssh}, 201)
    vid = cv["id"]
    for _ in range(20):
        code, body = c.req("POST", f"/api/connections/{vid}/test")
        if code == 409 or code == 200: break
        time.sleep(2)
    if code == 409:
        assert body["error"]["code"] == "ssh_unknown_host", body
        hk = body["error"]["detail"]; ok(f"ssh: unknown host key surfaced ({hk['keyType']} {hk['fingerprint'][:20]}...)")
        c.req("POST", "/api/ssh/trust", hk, 200)
    else:
        ok("ssh: bastion host key already trusted from an earlier run")
    code, body = c.req("POST", f"/api/connections/{vid}/test", expect=200); ok(f"ssh: tunnel works after trust, latency {body['latencyMs']}ms, {body['server']['version']}")
    code, body = stream(c, vid, "CREATE TABLE secrets (id int)", database="vault")
    assert code == 409, body; ok("production connection: DDL requires confirmation")
    code, ev = stream(c, vid, "CREATE TABLE IF NOT EXISTS secrets (id int); INSERT INTO secrets VALUES (1)", database="vault", confirm=True)
    assert not [e for e in ev if e["t"] == "stmtEnd" and e.get("error")], ev; ok("production connection: confirmed DDL ran through the tunnel")
    code, body = c.req("POST", f"/api/c/{vid}/access/apply", {"grants": [{"principal": "postgres", "level": "schema", "database": "vault", "schema": "public", "privilege": "USAGE"}]})
    assert code == 409 and body["error"]["code"] == "confirm_required", body; ok("production connection: access changes require confirmation")

    # ClickHouse
    _, cv = c.req("POST", "/api/connections", {"name": f"Analytics (ClickHouse) {time.time_ns()}", "driver": "clickhouse", "environment": "development",
        "params": {"host": "clickhouse", "protocol": "native", "user": "default", "database": "analytics"}, "secrets": {"password": PW}}, 201)
    chid = cv["id"]
    _, objs = c.req("GET", f"/api/c/{chid}/objects?db=analytics", expect=200)
    kinds = {o["name"]: o["kind"] for o in objs}
    assert kinds.get("events") == "table" and kinds.get("daily_revenue_mv") == "materialized_view" and kinds.get("country_names") == "dictionary", kinds
    ok("clickhouse objects: " + ", ".join(f"{o['name']}({o['kind']})" for o in objs))
    _, t = c.req("GET", f"/api/c/{chid}/describe?db=analytics&name=events", expect=200)
    assert t["options"]["partition_by"] == "toYYYYMM(event_time)" and t["primaryKey"] == ["event_type", "event_time"], t["options"]
    ok(f"clickhouse structure: {t['options']['engine']} ORDER BY {t['options']['order_by']}, PARTITION BY {t['options']['partition_by']}")
    _, res = c.req("POST", f"/api/c/{chid}/browse", {"ref": {"database": "analytics", "name": "events"}, "limit": 2,
        "filters": [{"column": "device", "op": "=", "value": "tablet"}, {"column": "revenue", "op": ">", "value": "100"}]}, 200)
    row = dict(zip([x["name"] for x in res["columns"]], res["rows"][0]))
    assert row["device"] == "tablet" and isinstance(row["tags"], list) and isinstance(row["properties"], dict), row
    ok(f"clickhouse browse with typed filters: device={row['device']} revenue={row['revenue']} properties={row['properties']}")
    code, ev = stream(c, chid, "SET max_threads = 2; SELECT getSetting('max_threads') AS t, count() FROM events WHERE event_type = 'purchase'", database="analytics")
    ends = [e for e in ev if e["t"] == "end"]
    rows = [r for e in ev if e["t"] == "rows" for r in e["rows"]]
    assert rows and rows[0][0] == 2 and ends[-1]["summary"].get("rowsRead"), (rows, ends)
    ok(f"clickhouse console: session settings kept, read {ends[-1]['summary']['rowsRead']} rows")
    code, body = stream(c, chid, "ALTER TABLE events DELETE WHERE 1", database="analytics")
    assert code == 409, body; ok("clickhouse ALTER … DELETE requires confirmation")
    _, plan = c.req("POST", f"/api/c/{chid}/explain", {"database": "analytics", "sql": "SELECT count() FROM events WHERE event_type = 'click'"}, 200)
    ok(f"clickhouse explain: {plan['root']['operation']}, totals {plan.get('totals')}")
    _, notes = c.req("POST", f"/api/c/{chid}/browse", {"ref": {"database": "analytics", "name": "notes"}, "limit": 10}, 200)
    first = dict(zip([x["name"] for x in notes["columns"]], notes["rows"][0]))
    c.req("POST", f"/api/c/{chid}/edit", {"ref": {"database": "analytics", "name": "notes"}, "edits": [{"op": "update", "key": first, "values": {"body": "first, edited"}}]}, 200)
    edited = dict(first, body="first, edited")
    c.req("POST", f"/api/c/{chid}/edit", {"ref": {"database": "analytics", "name": "notes"}, "edits": [{"op": "update", "key": edited, "values": {"body": first["body"]}}]}, 200)
    ok("clickhouse grid edit as a mutation, matched on the row's values, and undone")
    _, m = c.req("GET", f"/api/c/{chid}/access/model", expect=200)
    assert m["supported"], m; ok("clickhouse users and grants: " + ", ".join(l["level"] for l in m["model"]["levels"]))

    # MongoDB
    _, cv = c.req("POST", "/api/connections", {"name": f"Docs (Mongo) {time.time_ns()}", "driver": "mongodb", "environment": "development",
        "params": {"mode": "fields", "host": "mongo", "user": "root", "authSource": "admin", "database": "shop", "tls": "disable"},
        "secrets": {"password": PW}}, 201)
    mid = cv["id"]
    _, dbs = c.req("GET", f"/api/c/{mid}/databases", expect=200); ok("mongo databases: " + ", ".join(d["name"] for d in dbs))
    _, objs = c.req("GET", f"/api/c/{mid}/objects?db=shop", expect=200); ok("mongo collections: " + ", ".join(f"{o['name']}({o['kind']},{o.get('rows')})" for o in objs))
    _, t = c.req("GET", f"/api/c/{mid}/describe?db=shop&name=customers", expect=200)
    ok("mongo inferred fields: " + ", ".join(f"{col['name']}:{col['type']}" for col in t["columns"]))
    _, res = c.req("POST", f"/api/c/{mid}/browse", {"ref": {"database": "shop", "name": "customers"}, "limit": 3,
        "filters": [{"column": "tier", "op": "=", "value": "pro"}], "sort": [{"column": "signupAt", "desc": True}]}, 200)
    names = [x["name"] for x in res["columns"]]; row = dict(zip(names, res["rows"][0]))
    assert "$oid" in row["_id"] and "$geo" in json.dumps(row), row
    ok(f"mongo browse: {len(res['rows'])} rows, _id={row['_id']}, {res['sql'][:60]}")
    _, n = c.req("POST", f"/api/c/{mid}/count", {"ref": {"database": "shop", "name": "orders"}, "where": "{ status: 'paid', total: { $gt: NumberDecimal('500') } }"}, 200)
    ok(f"mongo count with shell-syntax filter: {n['rows']}")
    c.req("POST", f"/api/c/{mid}/edit", {"ref": {"database": "shop", "name": "customers"}, "edits": [{"op": "update", "key": {"_id": row["_id"]}, "values": {"tier": "enterprise", "tags": "['vip']"}}]}, 200)
    _, res2 = c.req("POST", f"/api/c/{mid}/browse", {"ref": {"database": "shop", "name": "customers"}, "limit": 1, "filters": [{"column": "_id", "op": "=", "value": row["_id"]}]}, 200)
    r2 = dict(zip([x["name"] for x in res2["columns"]], res2["rows"][0]))
    assert r2["tier"] == "enterprise" and r2["tags"] == ["vip"], r2; ok("mongo edit by _id kept types (array stays array)")
    code, ev = stream(c, mid, "db.orders.aggregate([{ $group: { _id: '$status', n: { $sum: 1 }, revenue: { $sum: '$total' } } }, { $sort: { n: -1 } }])\nshow collections\ndb.customers.find({ tier: 'pro' }, { email: 1 }).sort({ email: 1 }).limit(3)\ndb.orders.countDocuments({ status: 'refunded' })", database="shop")
    ends = [e for e in ev if e["t"] == "stmtEnd"]; errs = [e["error"] for e in ends if e.get("error")]
    assert len(ends) == 4 and not errs, (ends, errs)
    ok(f"mongo console: {len(ends)} statements, {sum(len(e['rows']) for e in ev if e['t']=='rows')} rows")
    code, body = stream(c, mid, "db.orders.deleteMany({})", database="shop")
    assert code == 409, body; ok("mongo deleteMany({}) requires confirmation")
    code, ev = stream(c, mid, "db.orders.find({ status: ", database="shop")
    err = next(e for e in ev if e["t"] == "stmtEnd")["error"]; ok(f"mongo parse error surfaced: {err['message'][:60]}")
    code, ev = stream(c, mid, "db.orders.find({}).limit(2)\ndb.orders.countDocuments({})", database="shop", mode="statement", cursor=5)
    assert len([e for e in ev if e["t"] == "stmt"]) == 1, ev; ok("mongo run-statement-at-cursor")
    _, plan = c.req("POST", f"/api/c/{mid}/explain", {"database": "shop", "sql": "db.orders.find({ status: 'paid' }).sort({ placedAt: -1 })", "analyze": True}, 200)
    ok(f"mongo explain: {plan['root']['operation']} → {[ch['operation'] for ch in plan['root'].get('children', [])]} totals {plan.get('totals')}")
    _, pl = c.req("GET", f"/api/c/{mid}/processes", expect=200); ok(f"mongo currentOp {len(pl['rows'])} ops")
    _, an = c.req("POST", f"/api/c/{mid}/analyze", {"sql": "db.a.find({})\ndb.a.drop()"}, 200)
    ok("mongo analyze markers: " + str([(s["line"], s["kind"], s["danger"]["level"]) for s in an["statements"]]))

    # PostgreSQL- and MySQL-compatible servers (CockroachDB over TLS)
    for engine, product, params in (
        ("cockroachdb", "CockroachDB", {"host": "cockroach", "user": "rowsmith", "database": "shop", "tls": "require"}),
        ("yugabytedb", "YugabyteDB", {"host": "yugabyte", "user": "yugabyte", "database": "shop", "tls": "disable"}),
        ("tidb", "TiDB", {"host": "tidb", "user": "root", "database": "shop", "tls": "disable"}),
    ):
        _, cv = c.req("POST", "/api/connections", {"name": f"Shop ({product}) {time.time_ns()}", "driver": engine, "environment": "development",
            "params": params, "secrets": {"password": PW}}, 201)
        xid = cv["id"]
        pg = engine != "tidb"
        scope = "db=shop&schema=public" if pg else "db=shop"
        _, srv = c.req("GET", f"/api/c/{xid}/server", expect=200)
        assert srv["server"]["product"] == product, srv
        _, objs = c.req("GET", f"/api/c/{xid}/objects?{scope}", expect=200)
        kinds = {o["name"]: o["kind"] for o in objs}
        assert kinds.get("orders") == "table" and kinds.get("customer_revenue") == "view", kinds
        ok(f"{engine}: {product} {srv['server']['version']}, {len(objs)} objects in shop")
        ref = {"database": "shop", "schema": "public", "name": "orders"} if pg else {"database": "shop", "name": "orders"}
        _, t = c.req("GET", f"/api/c/{xid}/describe?{scope}&name=customers", expect=200)
        _, res = c.req("POST", f"/api/c/{xid}/browse", {"ref": ref, "limit": 3, "filters": [{"column": "status", "op": "=", "value": "paid"}],
            "sort": [{"column": "placed_at", "desc": True}]}, 200)
        assert len(res["rows"]) == 3, res
        ok(f"{engine} browse with filters; customers has {len(t['columns'])} columns, row key {t['rowKey']}")
        code, ev = stream(c, xid, "DROP TABLE IF EXISTS smoke_notes;\nCREATE TABLE smoke_notes (id INT PRIMARY KEY, body TEXT);\nINSERT INTO smoke_notes VALUES (1, 'one'), (2, 'two');\nSELECT count(*) FROM smoke_notes",
            database="shop", schema="public" if pg else None, confirm=True)
        errs = [e["error"]["message"] for e in ev if e["t"] == "stmtEnd" and e.get("error")]
        assert not errs, errs
        nref = dict(ref, name="smoke_notes")
        c.req("POST", f"/api/c/{xid}/edit", {"ref": nref, "edits": [{"op": "update", "key": {"id": 1}, "values": {"body": "uno"}}, {"op": "delete", "key": {"id": 2}}]}, 200)
        _, res = c.req("POST", f"/api/c/{xid}/browse", {"ref": nref, "limit": 10}, 200)
        assert res["rows"] == [[1, "uno"]], res["rows"]
        ok(f"{engine} console DDL and grid edits")
        _, plan = c.req("POST", f"/api/c/{xid}/explain", {"database": "shop", "schema": "public" if pg else None, "sql": "SELECT * FROM orders WHERE customer_id = 7"}, 200)
        _, pl = c.req("GET", f"/api/c/{xid}/processes", expect=200)
        ok(f"{engine} plan root {plan['root']['operation']}, {len(pl['rows'])} sessions")
        code, ev = stream(c, xid, "DROP TABLE smoke_notes", database="shop", schema="public" if pg else None, confirm=True)
        assert not [e for e in ev if e["t"] == "stmtEnd" and e.get("error")], ev

    # A migration into TiDB: spatial columns become GeoJSON, and every copy is checked value by value.
    _, cv = c.req("POST", "/api/connections", {"name": f"Mig (TiDB) {time.time_ns()}", "driver": "tidb", "environment": "development",
        "params": {"host": "tidb", "user": "root", "database": "shop", "tls": "disable"}, "secrets": {"password": PW}}, 201)
    tid = cv["id"]
    stream(c, tid, "DROP DATABASE IF EXISTS smoke_mig;\nCREATE DATABASE smoke_mig", confirm=True)
    src, dst = {"connectionId": conns["mysql"], "database": "shop"}, {"connectionId": tid, "database": "smoke_mig"}
    _, pr = c.req("POST", "/api/migrations/plan", {"source": src, "target": dst, "options": {"data": True, "indexes": True, "foreignKeys": True,
        "verify": "contents", "existing": "fail", "existingRows": "append"}}, 200)
    plan = pr["plan"]
    for tp in plan["tables"]:
        tp["include"] = tp["source"]["name"] in ("stores", "categories")
    loc = next(col for tp in plan["tables"] if tp["source"]["name"] == "stores" for col in tp["columns"] if col["source"] == "location")
    assert loc["type"] == "json", loc
    _, m = c.req("POST", "/api/migrations", {"source": src, "target": dst, "plan": plan, "confirm": ""})
    for _ in range(120):
        _, st = c.req("GET", f"/api/migrations/{m['id']}", expect=200)
        if st["progress"]["status"] not in ("running", "queued", "pending", "planning"):
            break
        time.sleep(1)
    done = [(t["source"], t["status"], (t.get("verify") or {}).get("contents")) for t in st["progress"]["tables"]]
    assert st["progress"]["status"] == "done" and all(x[1] == "done" and x[2] == "match" for x in done), done
    ok(f"migration MySQL -> TiDB: {done}, shapes as GeoJSON")
    stream(c, tid, "DROP DATABASE smoke_mig", confirm=True)

    # Redis Cluster: keys from every primary, edits and commands routed by key.
    _, cv = c.req("POST", "/api/connections", {"name": f"Cache (Redis Cluster) {time.time_ns()}", "driver": "redis", "environment": "development",
        "params": {"host": "redis-cluster", "port": 7001, "database": 0, "tls": "disable"}, "secrets": {"password": PW}}, 201)
    rcid = cv["id"]
    _, srv = c.req("GET", f"/api/c/{rcid}/server", expect=200)
    assert srv["server"]["extras"]["Mode"].startswith("cluster of 3 primaries"), srv
    _, objs = c.req("GET", f"/api/c/{rcid}/objects?db=db0&match=product:*", expect=200)
    assert len(objs) == 12, objs
    c.req("POST", f"/api/c/{rcid}/edit", {"ref": {"database": "db0", "name": "user:2"}, "edits": [{"op": "update", "key": {"field": "plan"}, "values": {"value": "pro"}}]}, 200)
    code, ev = stream(c, rcid, "HGET user:2 plan\nHSET user:2 plan free\nKEYS user:*\nMULTI", database="db0")
    ends = [e for e in ev if e["t"] == "stmtEnd"]
    rows = [e["rows"] for e in ev if e["t"] == "rows"]
    assert rows[0] == [["pro"]] and len(rows[2]) == 3 and ends[3].get("error"), (rows, ends)
    _, pl = c.req("GET", f"/api/c/{rcid}/processes", expect=200)
    ok(f"redis cluster: {srv['server']['extras']['Mode']}, 12 product keys across nodes, edit and console routed by key, {len(pl['rows'])} clients on all nodes")

    # Cassandra and ScyllaDB: keyspaces, partitions and clustering order, edits as lightweight transactions.
    for engine, host in (("cassandra", "cassandra"), ("scylladb", "scylla")):
        _, cv = c.req("POST", "/api/connections", {"name": f"Wide rows ({engine}) {time.time_ns()}", "driver": engine, "environment": "development",
            "params": {"host": host, "user": "rowsmith", "database": "shop", "tls": "disable"}, "secrets": {"password": PW}}, 201)
        xid = cv["id"]
        conns[engine] = xid
        _, srv = c.req("GET", f"/api/c/{xid}/server", expect=200)
        _, objs = c.req("GET", f"/api/c/{xid}/objects?db=shop", expect=200)
        kinds = {o["name"]: o["kind"] for o in objs}
        assert kinds.get("orders_by_customer") == "table" and kinds.get("orders_by_status") == "materialized_view" and kinds.get("address") == "type", kinds
        _, t = c.req("GET", f"/api/c/{xid}/describe?db=shop&name=orders_by_customer", expect=200)
        assert t["options"]["partition_key"] == "customer_id" and t["options"]["clustering_order"] == "placed_at DESC, order_id ASC", t["options"]
        _, res = c.req("POST", f"/api/c/{xid}/browse", {"ref": {"database": "shop", "name": "sensor_readings"}, "limit": 3,
            "filters": [{"column": "sensor_id", "op": "=", "value": "fridge-1"}, {"column": "day", "op": "=", "value": "2025-06-03"}],
            "sort": [{"column": "ts", "desc": True}]}, 200)
        assert len(res["rows"]) == 3 and "ORDER BY ts DESC" in res["sql"], res
        ok(f"{engine}: {srv['server']['product']} {srv['server']['version']}, {len(objs)} objects in shop, partition browse newest first")
        code, ev = stream(c, xid, "DROP TABLE IF EXISTS smoke_notes;\nCREATE TABLE smoke_notes (id int PRIMARY KEY, body text, tags set<text>);\nINSERT INTO smoke_notes (id, body) VALUES (1, 'one');\nINSERT INTO smoke_notes (id, body) VALUES (2, 'two')",
            database="shop", confirm=True)
        assert not [e for e in ev if e["t"] == "stmtEnd" and e.get("error")], ev
        nref = {"database": "shop", "name": "smoke_notes"}
        c.req("POST", f"/api/c/{xid}/edit", {"ref": nref, "edits": [{"op": "update", "key": {"id": 1}, "values": {"body": "uno", "tags": ["a", "b"]}},
            {"op": "delete", "key": {"id": 2}}]}, 200)
        _, res = c.req("POST", f"/api/c/{xid}/browse", {"ref": nref, "limit": 10}, 200)
        assert res["rows"] == [[1, "uno", ["a", "b"]]], res["rows"]
        code, body = c.req("POST", f"/api/c/{xid}/edit", {"ref": nref, "edits": [{"op": "insert", "values": {"id": 1, "body": "again"}}]})
        assert code >= 400 and "already exists" in json.dumps(body), (code, body)
        _, plan = c.req("POST", f"/api/c/{xid}/explain", {"database": "shop", "sql": "SELECT * FROM sensor_readings WHERE sensor_id = 'boiler-1' AND day = '2025-06-01'", "analyze": True}, 200)
        ok(f"{engine} grid edits as lightweight transactions (a duplicate insert is refused), traced query on {plan['totals']['Nodes']} node(s)")
        stream(c, xid, "DROP TABLE smoke_notes", database="shop", confirm=True)
    # Migrations: MySQL rows into Cassandra, and Cassandra's collections and user types out into PostgreSQL.
    stream(c, conns["cassandra"], "DROP KEYSPACE IF EXISTS smoke_mig;\nCREATE KEYSPACE smoke_mig WITH replication = {'class': 'NetworkTopologyStrategy', 'dc1': 1}", database="shop", confirm=True)
    stream(c, conns["postgres"], "DROP SCHEMA IF EXISTS smoke_wide CASCADE;\nCREATE SCHEMA smoke_wide", database="city", confirm=True)
    for src, dst, pick, label in (
        ({"connectionId": conns["mysql"], "database": "shop"}, {"connectionId": conns["cassandra"], "database": "smoke_mig"}, ("stores", "categories"), "MySQL -> Cassandra"),
        ({"connectionId": conns["cassandra"], "database": "shop"}, {"connectionId": conns["postgres"], "database": "city", "schema": "smoke_wide"}, ("customers", "orders_by_customer"), "Cassandra -> PostgreSQL"),
    ):
        _, pr = c.req("POST", "/api/migrations/plan", {"source": src, "target": dst, "options": {"data": True, "indexes": True, "foreignKeys": True,
            "verify": "contents", "existing": "fail", "existingRows": "append"}}, 200)
        plan = pr["plan"]
        for tp in plan["tables"]:
            tp["include"] = tp["source"]["name"] in pick
        _, m = c.req("POST", "/api/migrations", {"source": src, "target": dst, "plan": plan, "confirm": ""})
        for _ in range(180):
            _, st = c.req("GET", f"/api/migrations/{m['id']}", expect=200)
            if st["progress"]["status"] not in ("running", "queued", "pending", "planning"):
                break
            time.sleep(1)
        done = [(t["source"], t["status"], (t.get("verify") or {}).get("contents")) for t in st["progress"]["tables"]]
        assert st["progress"]["status"] == "done" and all(x[1] == "done" and x[2] == "match" for x in done), (done, st["progress"])
        ok(f"migration {label}: {done}")
    stream(c, conns["cassandra"], "DROP KEYSPACE smoke_mig", database="shop", confirm=True)
    stream(c, conns["postgres"], "DROP SCHEMA smoke_wide CASCADE", database="city", confirm=True)

    # Trino over the lake: Iceberg tables, plain Parquet and CSV files, and a migration into Iceberg.
    _, cv = c.req("POST", "/api/connections", {"name": f"Lake (Trino) {time.time_ns()}", "driver": "trino", "environment": "development",
        "params": {"host": "trino", "user": "rowsmith", "catalog": "iceberg", "schema": "shop", "tls": "disable"}, "secrets": {}}, 201)
    trid = cv["id"]
    _, srv = c.req("GET", f"/api/c/{trid}/server", expect=200)
    _, dbs = c.req("GET", f"/api/c/{trid}/databases", expect=200)
    cats = {d["name"] for d in dbs}
    assert srv["server"]["product"] == "Trino" and {"iceberg", "hive", "tpch"} <= cats, (srv, cats)
    ok(f"trino: Trino {srv['server']['version']}, catalogs {sorted(cats)}")
    _, objs = c.req("GET", f"/api/c/{trid}/objects?db=iceberg&schema=shop", expect=200)
    kinds = {o["name"]: o["kind"] for o in objs}
    assert kinds.get("orders") == "table" and kinds.get("revenue_by_segment") == "view", kinds
    _, t = c.req("GET", f"/api/c/{trid}/describe?db=iceberg&schema=shop&name=orders", expect=200)
    assert t["options"]["partitioning"] == "month(placed_at)" and int(t["options"]["snapshots"]) >= 4 and t["editable"], t["options"]
    ok(f"trino iceberg orders: partitioned by {t['options']['partitioning']}, {t['options']['snapshots']} snapshots, {t['options']['data_files']} data files")
    _, res = c.req("POST", f"/api/c/{trid}/browse", {"ref": {"database": "iceberg", "schema": "shop", "name": "orders"}, "limit": 3,
        "filters": [{"column": "status", "op": "=", "value": "F"}, {"column": "total", "op": ">", "value": "100000"}], "sort": [{"column": "placed_at", "desc": True}]}, 200)
    assert len(res["rows"]) == 3, res
    _, res = c.req("POST", f"/api/c/{trid}/browse", {"ref": {"database": "hive", "schema": "raw", "name": "nations"}, "limit": 50}, 200)
    assert len(res["rows"]) == 25, res["rows"]
    _, t = c.req("GET", f"/api/c/{trid}/describe?db=hive&schema=raw&name=events", expect=200)
    ok(f"trino browse: typed filters on Iceberg, {len(res['rows'])} nations from CSV, events as {t['options']['format']} at {t['options']['location']}")
    code, ev = stream(c, trid, 'SET SESSION query_max_run_time = \'5m\';\nSELECT count(*) FROM orders FOR VERSION AS OF (SELECT 1)', database="iceberg", schema="shop")
    ends = [e for e in ev if e["t"] == "stmtEnd"]
    assert len(ends) == 2 and ends[1]["error"]["line"] == 2, ends
    code, ev = stream(c, trid, "SHOW SESSION LIKE 'query_max_run_time'", database="iceberg", schema="shop")
    assert "5m" in json.dumps([e for e in ev if e["t"] == "rows"]), ev
    ok(f"trino console: session setting kept between runs, error on line 2: {ends[1]['error']['message'][:50]}")
    code, ev = stream(c, trid, "DROP TABLE IF EXISTS smoke_notes;\nCREATE TABLE smoke_notes (id bigint, body varchar);\nINSERT INTO smoke_notes VALUES (1, 'one'), (2, 'two')",
        database="iceberg", schema="shop", confirm=True)
    assert not [e for e in ev if e["t"] == "stmtEnd" and e.get("error")], ev
    nref = {"database": "iceberg", "schema": "shop", "name": "smoke_notes"}
    c.req("POST", f"/api/c/{trid}/edit", {"ref": nref, "edits": [{"op": "update", "key": {"id": 1, "body": "one"}, "values": {"body": "uno"}},
        {"op": "delete", "key": {"id": 2, "body": "two"}}]}, 200)
    _, res = c.req("POST", f"/api/c/{trid}/browse", {"ref": nref, "limit": 10}, 200)
    assert res["rows"] == [[1, "uno"]], res["rows"]
    ok("trino grid edits on Iceberg, matched on whole rows")
    _, plan = c.req("POST", f"/api/c/{trid}/explain", {"database": "iceberg", "schema": "shop", "sql": "SELECT status, count(*) FROM orders GROUP BY status", "analyze": True}, 200)
    _, pl = c.req("GET", f"/api/c/{trid}/processes", expect=200)
    ok(f"trino explain analyze: {plan['root']['operation']} → {[ch['operation'] for ch in plan['root'].get('children', [])]}, {len(pl['rows'])} running queries")
    stream(c, trid, "DROP TABLE smoke_notes", database="iceberg", schema="shop", confirm=True)
    for catalog, label in (("iceberg", "Iceberg"), ("delta", "Delta Lake")):
        stream(c, trid, f"DROP SCHEMA IF EXISTS {catalog}.smoke_mig CASCADE;\nCREATE SCHEMA {catalog}.smoke_mig", database="iceberg", schema="shop", confirm=True)
        src, dst = {"connectionId": conns["mysql"], "database": "shop"}, {"connectionId": trid, "database": catalog, "schema": "smoke_mig"}
        _, pr = c.req("POST", "/api/migrations/plan", {"source": src, "target": dst, "options": {"data": True, "indexes": True, "foreignKeys": True,
            "verify": "contents", "existing": "fail", "existingRows": "append"}}, 200)
        plan = pr["plan"]
        for tp in plan["tables"]:
            tp["include"] = tp["source"]["name"] in ("stores", "categories")
        _, m = c.req("POST", "/api/migrations", {"source": src, "target": dst, "plan": plan, "confirm": ""})
        for _ in range(180):
            _, st = c.req("GET", f"/api/migrations/{m['id']}", expect=200)
            if st["progress"]["status"] not in ("running", "queued", "pending", "planning"):
                break
            time.sleep(1)
        done = [(t["source"], t["status"], (t.get("verify") or {}).get("contents")) for t in st["progress"]["tables"]]
        assert st["progress"]["status"] == "done" and all(x[1] == "done" and x[2] == "match" for x in done), (done, st["progress"])
        ok(f"migration MySQL -> {label} via Trino: {done}")
        stream(c, trid, f"DROP SCHEMA {catalog}.smoke_mig CASCADE", database="iceberg", schema="shop", confirm=True)
    # Delta Lake: versions from the transaction log, then a migration out of the lake into PostgreSQL.
    _, t = c.req("GET", f"/api/c/{trid}/describe?db=delta&schema=sales&name=payments", expect=200)
    assert t["options"]["connector"] == "delta_lake" and t["options"]["partitioned_by"] == "region" and int(t["options"]["versions"]) >= 5 and t["editable"], t["options"]
    _, res = c.req("POST", f"/api/c/{trid}/browse", {"ref": {"database": "delta", "schema": "sales", "name": "payments"}, "limit": 3,
        "filters": [{"column": "region", "op": "=", "value": "asia"}, {"column": "amount", "op": ">", "value": "1000.50"}]}, 200)
    assert len(res["rows"]) == 3, res
    ok(f"trino delta payments: partitioned by {t['options']['partitioned_by']}, {t['options']['versions']} versions, {t['options']['data_files']} files, ~{t.get('rowEstimate')} rows")
    stream(c, conns["postgres"], "DROP SCHEMA IF EXISTS smoke_lake CASCADE;\nCREATE SCHEMA smoke_lake", database="city", confirm=True)
    src, dst = {"connectionId": trid, "database": "delta", "schema": "sales"}, {"connectionId": conns["postgres"], "database": "city", "schema": "smoke_lake"}
    _, pr = c.req("POST", "/api/migrations/plan", {"source": src, "target": dst, "options": {"data": True, "indexes": True, "foreignKeys": True,
        "verify": "contents", "existing": "fail", "existingRows": "append"}}, 200)
    plan = pr["plan"]
    types = {col["source"]: col["type"] for tp in plan["tables"] for col in tp["columns"]}
    _, m = c.req("POST", "/api/migrations", {"source": src, "target": dst, "plan": plan, "confirm": ""})
    for _ in range(180):
        _, st = c.req("GET", f"/api/migrations/{m['id']}", expect=200)
        if st["progress"]["status"] not in ("running", "queued", "pending", "planning"):
            break
        time.sleep(1)
    done = [(t["source"], t["status"], (t.get("verify") or {}).get("contents")) for t in st["progress"]["tables"]]
    assert st["progress"]["status"] == "done" and all(x[1] == "done" and x[2] == "match" for x in done), (done, st["progress"])
    ok(f"migration Delta Lake via Trino -> PostgreSQL: {done}, amount as {types['amount']}, attributes as {types['attributes']}")
    stream(c, conns["postgres"], "DROP SCHEMA smoke_lake CASCADE", database="city", confirm=True)

    # Redis and Valkey (Valkey signs in as an ACL user)
    for engine, params in (("redis", {"host": "redis"}), ("valkey", {"host": "valkey", "user": "rowsmith"})):
        _, cv = c.req("POST", "/api/connections", {"name": f"Cache ({engine}) {time.time_ns()}", "driver": engine, "environment": "development",
            "params": {**params, "database": 0, "tls": "disable"}, "secrets": {"password": PW}}, 201)
        rid = cv["id"]
        _, srv = c.req("GET", f"/api/c/{rid}/server", expect=200)
        _, dbs = c.req("GET", f"/api/c/{rid}/databases", expect=200)
        assert dbs[0]["name"] == "db0" and dbs[0]["tables"] >= 16 and dbs[1]["tables"] == 2, dbs[:2]
        ok(f"{engine}: {srv['server']['product']} {srv['server']['version']}, {len(dbs)} databases, db0 has {dbs[0]['tables']} keys")
        _, objs = c.req("GET", f"/api/c/{rid}/objects?db=db0", expect=200)
        kinds = {o["name"]: o["kind"] for o in objs}
        assert kinds["user:1"] == "hash" and kinds["leaderboard"] == "zset" and kinds["orders"] == "stream" and kinds["queue:email"] == "list", kinds
        _, users = c.req("GET", f"/api/c/{rid}/objects?db=db0&match=user:*", expect=200)
        assert [o["name"] for o in users] == ["user:1", "user:2", "user:3"], users
        ok(f"{engine} keys: {len(objs)} by type ({', '.join(sorted(set(kinds.values())))}), user:* finds {len(users)}")
        _, t = c.req("GET", f"/api/c/{rid}/describe?db=db0&name=session:abc123", expect=200)
        assert t["kind"] == "string" and t["options"]["Expires"].startswith("in "), t["options"]
        _, res = c.req("POST", f"/api/c/{rid}/browse", {"ref": {"database": "db0", "name": "big:hash"}, "offset": 100, "limit": 5}, 200)
        assert res["rows"][0] == ["field:0101", "303"] and res["truncated"], res["rows"][:1]
        _, n = c.req("POST", f"/api/c/{rid}/count", {"ref": {"database": "db0", "name": "big:hash"}, "filters": [{"column": "value", "op": ">", "value": "1700"}]}, 200)
        assert n["rows"] == 34 and n["exact"], n
        ok(f"{engine} browse: hash fields paged and filtered ({n['rows']} match), session key {t['options']['Expires']}")
        code, ev = stream(c, rid, "DEL smoke:h\nHSET smoke:h a 1 b 2 c 3\nHGETALL smoke:h\nSET smoke:s \"hello there\" EX 60\nTTL smoke:s", database="db2")
        ends = [e for e in ev if e["t"] == "stmtEnd"]
        assert len(ends) == 5 and not [e for e in ends if e.get("error")], ends
        rows = [r for e in ev if e["t"] == "rows" for r in e["rows"]]
        assert ["a", "1"] in rows, rows
        ok(f"{engine} console: {len(ends)} commands, HGETALL as field/value rows")
        _, er = c.req("POST", f"/api/c/{rid}/edit", {"ref": {"database": "db2", "name": "smoke:h"}, "edits": [
            {"op": "update", "key": {"field": "a"}, "values": {"value": "one"}}, {"op": "delete", "key": {"field": "b"}},
            {"op": "insert", "values": {"field": "d", "value": "4"}}]}, 200)
        _, res = c.req("POST", f"/api/c/{rid}/browse", {"ref": {"database": "db2", "name": "smoke:h"}, "limit": 10}, 200)
        assert res["rows"] == [["a", "one"], ["c", "3"], ["d", "4"]], res["rows"]
        ok(f"{engine} grid edits checked and applied in one transaction: {er['statements']}")
        code, body = stream(c, rid, "FLUSHDB", database="db2")
        assert code == 409, body; ok(f"{engine} FLUSHDB requires confirmation")
        code, ev = stream(c, rid, "GET smoke:s\nHLEN smoke:h", database="db2", mode="statement", cursor=14)
        stmts = [e for e in ev if e["t"] == "stmt"]
        assert len(stmts) == 1 and stmts[0]["sql"] == "HLEN smoke:h", stmts; ok(f"{engine} runs the command at the cursor")
        _, an = c.req("POST", f"/api/c/{rid}/analyze", {"sql": "GET a\nSET a b\nFLUSHALL"}, 200)
        assert [s["kind"] for s in an["statements"]] == ["read", "write", "ddl"] and an["statements"][2]["danger"]["level"] == "destructive", an
        _, ddl = c.req("POST", f"/api/c/{rid}/ddl", {"action": "rename", "ref": {"database": "db2", "name": "smoke:h"}, "newName": "smoke:h2"}, 200)
        code, ev = stream(c, rid, ddl["script"], database="db2", confirm=True)
        c.req("GET", f"/api/c/{rid}/describe?db=db2&name=smoke:h2", expect=200)
        _, ddl = c.req("POST", f"/api/c/{rid}/ddl", {"action": "drop", "ref": {"database": "db2", "name": "smoke:h2"}}, 200)
        code, ev = stream(c, rid, ddl["script"] + "\nDEL smoke:s", database="db2", confirm=True)
        assert [r for e in ev if e["t"] == "rows" for r in e["rows"]] == [[1], [1]], ev
        code, _ = c.req("GET", f"/api/c/{rid}/describe?db=db2&name=smoke:h2")
        assert code >= 400, code
        ok(f"{engine} rename and delete through the console: {ddl['script'].strip()}")
        _, pl = c.req("GET", f"/api/c/{rid}/processes", expect=200)
        _, vs = c.req("GET", f"/api/c/{rid}/variables?kind=status", expect=200)
        ok(f"{engine} clients: {len(pl['rows'])}, INFO fields: {len(vs['rows'])}")

    # viewer with read-only share
    _, u = c.req("POST", "/api/users", {"name": "Vic Viewer", "email": VIEWER, "role": "viewer", "password": "Quiet-Lantern-Harbor-31"}, 201)
    c.req("PUT", f"/api/connections/{conns['mysql']}/shares", {"shares": [{"userId": u["id"], "access": "write"}]}, 200)
    v = Client(); me = v.login(VIEWER, "Quiet-Lantern-Harbor-31")
    assert me["user"]["mustChangePassword"], me
    v.req("GET", "/api/connections", expect=403); ok("temporary password: everything but a password change is refused")
    v.req("POST", "/api/me/password", {"current": "Quiet-Lantern-Harbor-31", "next": "Amber-Signal-Orchard-58"}, 200)
    _, lst = v.req("GET", "/api/connections", expect=200)
    assert len(lst) == 1 and lst[0]["access"] == "read" and "password" not in json.dumps(lst[0]), lst
    ok("viewer sees only the shared connection, capped to read access, no secrets")
    code, ev = stream(v, conns["mysql"], "SELECT COUNT(*) FROM customers; DELETE FROM customers WHERE id = 2", database="shop")
    errs = [e["error"]["message"] for e in ev if e["t"] == "stmtEnd" and e.get("error")]
    assert errs and "read-only" in errs[0], ev; ok("viewer: write blocked -> " + errs[0][:70])
    code, ev = stream(v, conns["mysql"], "SET SESSION TRANSACTION READ WRITE", database="shop")
    assert [e for e in ev if e["t"] == "stmtEnd" and e.get("error")], ev; ok("viewer: attempt to leave read-only mode blocked")
    v.req("POST", f"/api/c/{conns['mysql']}/edit", {"ref": {"database": "shop", "name": "customers"}, "edits": [{"op": "delete", "key": {"id": 3}}]}, 403)
    ok("viewer: grid edit rejected")
    code, _ = v.req("POST", f"/api/c/{conns['mysql']}/access/apply", {"grants": [{"principal": "root@%", "level": "server", "privilege": "SELECT"}]})
    assert code in (403, 404), code; ok(f"viewer: access changes refused ({code})")
    v.req("GET", f"/api/c/{conns['postgres']}/databases", expect=404); ok("viewer: unshared connection is invisible")
    v.req("GET", "/api/audit", expect=403); ok("viewer: audit log forbidden")
    _, audit = c.req("GET", "/api/audit?limit=200", expect=200)
    ok(f"audit log has {len(audit)} entries, e.g. {sorted({a['action'] for a in audit})[:8]}")
    _, hist = c.req("GET", "/api/history?limit=5", expect=200); ok(f"history: {len(hist)} recent entries")
    # Leave the workspace as we found it.
    _, mine = c.req("GET", "/api/connections", expect=200)
    for x in mine:
        if x["id"] in created:
            c.req("DELETE", f"/api/connections/{x['id']}")
    ok(f"cleaned up {len(created)} test connections")
    print("\nALL SMOKE TESTS PASSED")

if __name__ == "__main__":
    main()
