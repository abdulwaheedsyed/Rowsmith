import { useEffect, useRef, useState, type ReactNode } from "react";
import { Link, Route, Switch, useLocation, useRoute } from "wouter";
import { useQueryClient } from "@tanstack/react-query";
import {
  Plus, Search, Settings, LogOut, Moon, Sun, Monitor, UserRound, BookMarked, Shield, Menu as MenuIcon, X, Home as HomeIcon,
  TerminalSquare, Table2, Lock, PanelLeftClose, PanelLeftOpen,
} from "lucide-react";
import { post } from "../../lib/api";
import { useConnections, useServer } from "../../lib/queries";
import { useUI, useWorkspace } from "../../lib/store";
import type { Connection, Me } from "../../lib/types";
import { AnvilMark, Button, Env, Kbd, Menu, MenuContent, MenuItem, MenuLabel, MenuSep, MenuTrigger, Tip } from "../../components/ui";
import { modKey } from "../../lib/format";
import { Home } from "../home/Home";
import { Workspace } from "../workspace/Workspace";
import { Navigator } from "../navigator/Navigator";
import { CommandPalette } from "./CommandPalette";
import { ConnectionDialog } from "../connections/ConnectionForm";
import { Library } from "../library/Library";
import { Admin } from "../admin/Admin";
import { Account } from "../account/Account";
import { StatusBar } from "./StatusBar";
import { newQueryTab } from "../workspace/actions";

export function Shell({ me }: { me: Me }) {
  const [, params] = useRoute<{ id: string }>("/c/:id/*?");
  const connId = params?.id;
  const { navOpen, setNavOpen, setPaletteOpen, navWidth } = useUI();
  const [drawer, setDrawer] = useState(false);
  const [location] = useLocation();
  const [editing, setEditing] = useState<{ open: boolean; conn?: Connection; driver?: string }>({ open: false });
  const conns = useConnections();
  const conn = conns.data?.find((c) => c.id === connId);
  const touchRecent = useWorkspace((s) => s.touchRecent);

  useEffect(() => {
    if (connId) touchRecent(connId);
  }, [connId, touchRecent]);
  const activeTab = useWorkspace((s) => (connId ? s.active[connId] : undefined));
  // Close the mobile drawer after navigating or opening a tab from it.
  useEffect(() => setDrawer(false), [location, activeTab]);

  // Global shortcuts.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      const mod = e.metaKey || e.ctrlKey;
      if (mod && (e.key === "k" || e.key === "K") && !e.shiftKey) {
        e.preventDefault();
        setPaletteOpen(true);
      } else if (mod && e.key === "p" && !e.shiftKey && !e.altKey) {
        e.preventDefault();
        setPaletteOpen(true);
      } else if (mod && e.key === "b" && !e.shiftKey && !(e.target as HTMLElement)?.closest?.(".cm-editor")) {
        e.preventDefault();
        setNavOpen(!useUI.getState().navOpen);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [setPaletteOpen, setNavOpen]);

  const inWorkspace = !!connId;
  const envClass = conn ? `shell--env-${conn.environment}` : "";

  return (
    <div className={`shell ${navOpen ? "" : "shell--nav-collapsed"} ${inWorkspace ? "" : "shell--no-nav"} ${drawer ? "shell--drawer" : ""} ${envClass}`} style={{ ["--nav-w" as string]: `${navWidth}px` }}>
      <Rail me={me} activeId={connId} onAdd={() => setEditing({ open: true })} />
      <aside className="shell__nav" aria-label="Navigator">
        {inWorkspace && conn ? (
          <Navigator conn={conn} onEdit={() => setEditing({ open: true, conn })} />
        ) : (
          <HomeNav me={me} onAdd={() => setEditing({ open: true })} />
        )}
        <NavResizer />
      </aside>
      <div className="shell__scrim" onClick={() => setDrawer(false)} />
      <div className="shell__main">
        <TopBar me={me} conn={conn} onMenu={() => setDrawer(true)} />
        <main className="shell__content">
          <Switch>
            <Route path="/c/:id/*?">{conn ? <Workspace conn={conn} me={me} /> : conns.isLoading ? null : <MissingConnection />}</Route>
            <Route path="/library">
              <Library />
            </Route>
            <Route path="/admin/*?">
              <Admin me={me} />
            </Route>
            <Route path="/account">
              <Account me={me} />
            </Route>
            <Route>
              <Home me={me} onAdd={(driver) => setEditing({ open: true, driver })} onEdit={(c) => setEditing({ open: true, conn: c })} />
            </Route>
          </Switch>
        </main>
        <StatusBar conn={conn} />
      </div>
      <MobileBar conn={conn} onMenu={() => setDrawer(true)} />
      <CommandPalette me={me} conn={conn} onNewConnection={() => setEditing({ open: true })} />
      {editing.open && <ConnectionDialog conn={editing.conn} initialDriver={editing.driver} onClose={() => setEditing({ open: false })} />}
    </div>
  );
}

function MissingConnection() {
  return (
    <div className="empty" style={{ marginTop: "12vh" }}>
      <div className="empty__title">Connection not found</div>
      <div>It may have been deleted, or it is no longer shared with you.</div>
      <Link href="/" className="btn">Back to connections</Link>
    </div>
  );
}

// ---- Rail --------------------------------------------------------------------------

function initials(name: string) {
  const words = name.replace(/\(.*?\)/g, "").trim().split(/[^\p{L}\p{N}]+/u).filter(Boolean);
  if (words.length === 0) return "?";
  if (words.length === 1) return words[0].slice(0, 2);
  return (words[0][0] + words[1][0]);
}

function Rail({ me, activeId, onAdd }: { me: Me; activeId?: string; onAdd(): void }) {
  const conns = useConnections();
  const recent = useWorkspace((s) => s.recent);
  const list = [...(conns.data ?? [])].sort((a, b) => {
    const ra = recent.indexOf(a.id), rb = recent.indexOf(b.id);
    if (ra !== rb) return (ra < 0 ? 999 : ra) - (rb < 0 ? 999 : rb);
    return a.name.localeCompare(b.name);
  });
  const [location] = useLocation();
  return (
    <nav className="rail" aria-label="Connections">
      <Tip label="All connections" side="right">
        <Link href="/" className={`rail__brand ${location === "/" ? "is-active" : ""}`} aria-label="Home">
          <AnvilMark size={30} />
        </Link>
      </Tip>
      <div className="rail__list">
        {list.map((c) => (
          <Tip key={c.id} side="right" label={<span className="row gap-3">{c.name} <Env env={c.environment} /></span>}>
            <Link href={`/c/${c.id}`} className={`ingot ingot--${c.environment} ${c.id === activeId ? "is-active" : ""}`} aria-label={c.name} style={c.color ? { ["--ingot" as string]: c.color } : undefined}>
              <span className="ingot__text">{initials(c.name)}</span>
              {c.readOnly || c.access === "read" ? <Lock className="ingot__lock" /> : null}
            </Link>
          </Tip>
        ))}
        {me.user.role !== "viewer" && (
          <Tip label="New connection" side="right">
            <button className="ingot ingot--add" onClick={onAdd} aria-label="New connection">
              <Plus />
            </button>
          </Tip>
        )}
      </div>
      <div className="rail__foot">
        <Tip label="Saved queries & history" side="right">
          <Link href="/library" className={`rail__icon ${location.startsWith("/library") ? "is-active" : ""}`} aria-label="Library">
            <BookMarked />
          </Link>
        </Tip>
        {(me.user.role === "owner" || me.user.role === "admin") && (
          <Tip label="Administration" side="right">
            <Link href="/admin" className={`rail__icon ${location.startsWith("/admin") ? "is-active" : ""}`} aria-label="Administration">
              <Shield />
            </Link>
          </Tip>
        )}
        <AccountMenu me={me} side="right" />
      </div>
    </nav>
  );
}

function AccountMenu({ me, side = "bottom" }: { me: Me; side?: "right" | "bottom" }) {
  const qc = useQueryClient();
  const [, navigate] = useLocation();
  const { theme, setTheme } = useUI();
  const initialsText = me.user.name.split(/\s+/).map((w) => w[0]).slice(0, 2).join("").toUpperCase();
  return (
    <Menu>
      <MenuTrigger asChild>
        <button className="avatar" aria-label="Account menu">{initialsText}</button>
      </MenuTrigger>
      <MenuContent side={side} align="end">
        <div className="menu__who">
          <div className="menu__who-name">{me.user.name}</div>
          <div className="menu__who-mail">{me.user.email} · {me.user.role}</div>
        </div>
        <MenuSep />
        <MenuItem icon={<UserRound />} onSelect={() => navigate("/account")}>Account & security</MenuItem>
        <MenuSep />
        <MenuLabel>Theme</MenuLabel>
        <MenuItem icon={<Monitor />} onSelect={() => setTheme("system")} hint={theme === "system" ? "✓" : undefined}>System</MenuItem>
        <MenuItem icon={<Moon />} onSelect={() => setTheme("dark")} hint={theme === "dark" ? "✓" : undefined}>Dark</MenuItem>
        <MenuItem icon={<Sun />} onSelect={() => setTheme("light")} hint={theme === "light" ? "✓" : undefined}>Light</MenuItem>
        <MenuSep />
        <MenuItem
          icon={<LogOut />}
          onSelect={async () => {
            await post("auth/logout").catch(() => {});
            qc.clear();
            qc.setQueryData(["me"], null);
            navigate("/");
          }}
        >
          Sign out
        </MenuItem>
      </MenuContent>
    </Menu>
  );
}

// ---- Navigator column when not in a connection ----------------------------------------

function HomeNav({ me, onAdd }: { me: Me; onAdd(): void }) {
  const conns = useConnections();
  const [location] = useLocation();
  const folders = new Map<string, Connection[]>();
  for (const c of conns.data ?? []) {
    const f = c.folder || "";
    if (!folders.has(f)) folders.set(f, []);
    folders.get(f)!.push(c);
  }
  const sorted = [...folders.entries()].sort(([a], [b]) => (a === "" ? -1 : b === "" ? 1 : a.localeCompare(b)));
  return (
    <div className="homenav">
      <div className="homenav__head">
        <span className="eyebrow">Connections</span>
        <span className="spacer" />
        {me.user.role !== "viewer" && (
          <Tip label="New connection">
            <Button variant="ghost" size="sm" icon onClick={onAdd} aria-label="New connection"><Plus /></Button>
          </Tip>
        )}
      </div>
      <div className="homenav__list">
        {sorted.map(([folder, list]) => (
          <div key={folder} className="homenav__group">
            {folder && <div className="homenav__folder">{folder}</div>}
            {list.map((c) => (
              <Link key={c.id} href={`/c/${c.id}`} className={`homenav__item ${location === `/c/${c.id}` ? "is-active" : ""}`}>
                <span className={`dot dot--${c.environment}`} />
                <span className="truncate grow">{c.name}</span>
                {(c.readOnly || c.access === "read") && <Lock size={12} className="faint" />}
              </Link>
            ))}
          </div>
        ))}
        {conns.data && conns.data.length === 0 && <p className="homenav__empty muted">No connections yet.</p>}
      </div>
    </div>
  );
}

function NavResizer() {
  const setNavWidth = useUI((s) => s.setNavWidth);
  const drag = useRef<{ x: number; w: number } | null>(null);
  return (
    <div
      className="nav-resizer"
      role="separator"
      aria-orientation="vertical"
      aria-label="Resize navigator"
      tabIndex={0}
      onKeyDown={(e) => {
        const w = useUI.getState().navWidth;
        if (e.key === "ArrowLeft") setNavWidth(w - 16);
        if (e.key === "ArrowRight") setNavWidth(w + 16);
      }}
      onPointerDown={(e) => {
        drag.current = { x: e.clientX, w: useUI.getState().navWidth };
        (e.target as HTMLElement).setPointerCapture(e.pointerId);
        document.body.classList.add("is-resizing");
      }}
      onPointerMove={(e) => {
        if (drag.current) setNavWidth(drag.current.w + e.clientX - drag.current.x);
      }}
      onPointerUp={() => {
        drag.current = null;
        document.body.classList.remove("is-resizing");
      }}
    />
  );
}

// ---- Top bar -------------------------------------------------------------------------------

function TopBar({ me, conn, onMenu }: { me: Me; conn?: Connection; onMenu(): void }) {
  const { setPaletteOpen, navOpen, setNavOpen } = useUI();
  const server = useServer(conn?.id);
  const [location] = useLocation();
  let title: ReactNode = null;
  if (conn) {
    title = (
      <div className="topbar__title">
        <span className="topbar__name truncate">{conn.name}</span>
        <Env env={conn.environment} />
        {server.data && <span className="topbar__server mono truncate">{server.data.server.product} {server.data.server.version}</span>}
        {(server.data?.readOnly || conn.readOnly) && (
          <Tip label="Read-only: statements that could modify data are blocked">
            <span className="badge"><Lock /> read-only</span>
          </Tip>
        )}
      </div>
    );
  } else {
    const label = location.startsWith("/admin") ? "Administration" : location.startsWith("/account") ? "Account & security" : location.startsWith("/library") ? "Library" : "Connections";
    title = <div className="topbar__title"><span className="topbar__name">{label}</span></div>;
  }
  return (
    <header className="topbar">
      <button className="btn btn--ghost btn--icon topbar__menu" onClick={onMenu} aria-label="Open navigation">
        <MenuIcon />
      </button>
      <Tip label={<>{navOpen ? "Hide" : "Show"} navigator <Kbd>{modKey()}B</Kbd></>}>
        <button className="btn btn--ghost btn--icon btn--sm topbar__collapse" onClick={() => setNavOpen(!navOpen)} aria-label="Toggle navigator">
          {navOpen ? <PanelLeftClose /> : <PanelLeftOpen />}
        </button>
      </Tip>
      {title}
      <span className="spacer" />
      <button className="palette-trigger" onClick={() => setPaletteOpen(true)} aria-label="Search and commands">
        <Search />
        <span className="palette-trigger__text">{conn ? "Jump to a table, run a command…" : "Search connections and commands…"}</span>
        <span className="palette-trigger__keys"><Kbd>{modKey()}</Kbd><Kbd>K</Kbd></span>
      </button>
      {conn && (
        <Tip label="New query tab">
          <Button size="sm" onClick={() => newQueryTab(conn)} className="topbar__newq">
            <TerminalSquare /> New query
          </Button>
        </Tip>
      )}
      <div className="topbar__account">
        <AccountMenu me={me} />
      </div>
    </header>
  );
}

// ---- Mobile bottom bar --------------------------------------------------------------------

function MobileBar({ conn, onMenu }: { conn?: Connection; onMenu(): void }) {
  const setPaletteOpen = useUI((s) => s.setPaletteOpen);
  const [location, navigate] = useLocation();
  return (
    <nav className="mobilebar" aria-label="Primary">
      <button className={location === "/" ? "is-active" : ""} onClick={() => navigate("/")}>
        <HomeIcon /> <span>Home</span>
      </button>
      <button onClick={onMenu} disabled={!conn}>
        <Table2 /> <span>Objects</span>
      </button>
      <button onClick={() => conn && newQueryTab(conn)} disabled={!conn}>
        <TerminalSquare /> <span>Query</span>
      </button>
      <button onClick={() => setPaletteOpen(true)}>
        <Search /> <span>Search</span>
      </button>
      <button className={location.startsWith("/account") || location.startsWith("/admin") ? "is-active" : ""} onClick={() => navigate("/account")}>
        <Settings /> <span>Settings</span>
      </button>
    </nav>
  );
}

export function DrawerClose({ onClose }: { onClose(): void }) {
  return (
    <button className="btn btn--ghost btn--icon drawer-close" onClick={onClose} aria-label="Close navigation">
      <X />
    </button>
  );
}
