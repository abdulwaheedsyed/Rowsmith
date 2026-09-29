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

    # viewer with read-only share
    _, u = c.req("POST", "/api/users", {"name": "Vic Viewer", "email": VIEWER, "role": "viewer", "password": "Quiet-Lantern-Harbor-31"}, 201)
    c.req("PUT", f"/api/connections/{conns['mysql']}/shares", {"shares": [{"userId": u["id"], "access": "write"}]}, 200)
    v = Client(); v.login(VIEWER, "Quiet-Lantern-Harbor-31")
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
