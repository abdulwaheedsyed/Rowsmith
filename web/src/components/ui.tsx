import * as RDialog from "@radix-ui/react-dialog";
import * as RMenu from "@radix-ui/react-dropdown-menu";
import * as RTooltip from "@radix-ui/react-tooltip";
import { X, CheckCircle2, AlertTriangle, Info } from "lucide-react";
import { forwardRef, type ButtonHTMLAttributes, type CSSProperties, type ReactNode } from "react";
import { useToasts } from "../lib/store";

// ---- Brand ----------------------------------------------------------------------

// Sparks thrown from the strike point, as in the full logo: angle from
// vertical (degrees), resting distance from the strike point, and weight.
const SPARKS: [number, number, number][] = [[-16, 4.1, 1.15], [6, 6.3, 1.15], [26, 4.7, 1.1], [44, 7, 1], [62, 5.3, 0.95], [-30, 6.6, 0.85]];
const STRIKE = { x: 19.5, y: 7 };

/** The Rowsmith mark: an anvil built from table rows cut into cells. The face
 *  carries the temper gradient — the one place the brand uses it decoratively.
 *  Gaps are wider than in the full-size logo so they survive at 30px.
 *  `sparks` adds the logo's sparks, struck in bursts; they rest where the logo
 *  draws them when motion is reduced. Too fine to show below ~64px. */
export function AnvilMark({ size = 28, sparks = false }: { size?: number; sparks?: boolean }) {
  return (
    <svg width={size} height={size} viewBox="0 0 32 32" aria-hidden="true" className={sparks ? "anvil anvil--forge" : "anvil"} overflow="visible">
      <defs>
        <linearGradient id="rs-temper" x1="0" x2="1">
          <stop offset="0" style={{ stopColor: "var(--temper-straw)" }} />
          <stop offset=".45" stopColor="#C4803F" />
          <stop offset=".75" stopColor="#93508A" />
          <stop offset="1" stopColor="#3D7AD4" />
        </linearGradient>
        {sparks && (
          <linearGradient id="rs-spark" x1="0" x2="0" y1="0" y2="1">
            <stop offset="0" style={{ stopColor: "var(--spark)" }} />
            <stop offset="1" style={{ stopColor: "var(--spark)", stopOpacity: 0 }} />
          </linearGradient>
        )}
      </defs>
      <path className="anvil__face" d="M3 7h26v4.7H13.05C9.5 11.7 6.67 10.3 3 7z" fill="url(#rs-temper)" />
      <g fill="var(--text-2)">
        <rect x="11.27" y="13" width="3.34" height="3.4" rx=".6" />
        <rect x="15.51" y="13" width="3.34" height="3.4" rx=".6" />
        <rect x="19.75" y="13" width="3.34" height="3.4" rx=".6" />
      </g>
      <g fill="var(--muted)">
        <rect x="13.64" y="17.5" width="3.1" height="2.8" rx=".6" />
        <rect x="17.64" y="17.5" width="3.1" height="2.8" rx=".6" />
      </g>
      <g fill="var(--text-2)">
        <rect x="7.73" y="21.4" width="4.05" height="3.8" rx=".6" />
        <rect x="12.68" y="21.4" width="4.05" height="3.8" rx=".6" />
        <rect x="17.63" y="21.4" width="4.05" height="3.8" rx=".6" />
        <rect x="22.58" y="21.4" width="4.05" height="3.8" rx=".6" />
      </g>
      {sparks &&
        SPARKS.map(([angle, dist, weight], i) => {
          const length = (34 * weight + 10) * 0.0591; // the logo's streak length, on this grid
          const top = STRIKE.y - 0.6 - length;
          const rest = -(dist - 0.6 - length);
          const style = { "--rest": `${rest.toFixed(2)}px`, "--fly": `${(rest - 2).toFixed(2)}px`, "--o": (1.2 - (dist / 0.0591) / 230).toFixed(2), animationDelay: `${i * 35}ms` } as CSSProperties;
          return (
            <g key={i} transform={`rotate(${angle} ${STRIKE.x} ${STRIKE.y})`}>
              <g className="anvil__spark" style={style}>
                <rect x={STRIKE.x - 0.3} y={top} width=".6" height={length.toFixed(2)} rx=".3" fill="url(#rs-spark)" />
                <circle cx={STRIKE.x} cy={top} r=".4" fill="var(--spark-head)" />
              </g>
            </g>
          );
        })}
    </svg>
  );
}

export function Wordmark({ size = 18 }: { size?: number }) {
  return (
    <span className="wordmark display" style={{ fontSize: size }}>
      rowsmith
    </span>
  );
}

// ---- Buttons ------------------------------------------------------------------------

type BtnVariant = "default" | "primary" | "ghost" | "danger";
interface BtnProps extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: BtnVariant;
  size?: "sm" | "md" | "lg";
  icon?: boolean;
  block?: boolean;
  loading?: boolean;
}

export const Button = forwardRef<HTMLButtonElement, BtnProps>(function Button(
  { variant = "default", size = "md", icon, block, loading, className = "", children, disabled, ...rest },
  ref,
) {
  const cls = [
    "btn",
    variant !== "default" && `btn--${variant}`,
    size !== "md" && `btn--${size}`,
    icon && "btn--icon",
    block && "btn--block",
    className,
  ]
    .filter(Boolean)
    .join(" ");
  return (
    <button ref={ref} type="button" className={cls} disabled={disabled || loading} {...rest}>
      {loading ? <span className="spinner" /> : null}
      {children}
    </button>
  );
});

// ---- Tooltip ------------------------------------------------------------------------

export function Tip({ label, children, side = "bottom" }: { label: ReactNode; children: ReactNode; side?: "top" | "bottom" | "left" | "right" }) {
  return (
    <RTooltip.Root delayDuration={350}>
      <RTooltip.Trigger asChild>{children}</RTooltip.Trigger>
      <RTooltip.Portal>
        <RTooltip.Content className="tooltip" side={side} sideOffset={6} collisionPadding={8}>
          {label}
        </RTooltip.Content>
      </RTooltip.Portal>
    </RTooltip.Root>
  );
}

export const TipProvider = RTooltip.Provider;

// ---- Dialog ---------------------------------------------------------------------------

interface DialogProps {
  open: boolean;
  onOpenChange(open: boolean): void;
  title: ReactNode;
  description?: ReactNode;
  children?: ReactNode;
  footer?: ReactNode;
  width?: "normal" | "wide" | "xwide";
  dismissable?: boolean;
}

export function Dialog({ open, onOpenChange, title, description, children, footer, width = "normal", dismissable = true }: DialogProps) {
  return (
    <RDialog.Root open={open} onOpenChange={(o) => (dismissable || o ? onOpenChange(o) : undefined)}>
      <RDialog.Portal>
        <RDialog.Overlay className="scrim" />
        <RDialog.Content
          className={`dialog ${width !== "normal" ? `dialog--${width}` : ""}`}
          onPointerDownOutside={(e) => !dismissable && e.preventDefault()}
          onEscapeKeyDown={(e) => !dismissable && e.preventDefault()}
          aria-describedby={description ? undefined : undefined}
        >
          <div className="dialog__head">
            <div className="grow">
              <RDialog.Title className="dialog__title">{title}</RDialog.Title>
              {description ? <RDialog.Description className="dialog__desc">{description}</RDialog.Description> : <RDialog.Description className="sr-only">{String(title)}</RDialog.Description>}
            </div>
            {dismissable && (
              <RDialog.Close asChild>
                <Button variant="ghost" icon size="sm" aria-label="Close">
                  <X />
                </Button>
              </RDialog.Close>
            )}
          </div>
          {children !== undefined && <div className="dialog__body">{children}</div>}
          {footer && <div className="dialog__foot">{footer}</div>}
        </RDialog.Content>
      </RDialog.Portal>
    </RDialog.Root>
  );
}

// ---- Menus --------------------------------------------------------------------------------

export const Menu = RMenu.Root;
export const MenuTrigger = RMenu.Trigger;

export function MenuContent({ children, align = "start", side = "bottom" }: { children: ReactNode; align?: "start" | "end" | "center"; side?: "bottom" | "top" | "right" | "left" }) {
  return (
    <RMenu.Portal>
      <RMenu.Content className="menu" align={align} side={side} sideOffset={4} collisionPadding={8}>
        {children}
      </RMenu.Content>
    </RMenu.Portal>
  );
}

export function MenuItem({ icon, children, onSelect, danger, disabled, hint }: { icon?: ReactNode; children: ReactNode; onSelect?(): void; danger?: boolean; disabled?: boolean; hint?: ReactNode }) {
  return (
    <RMenu.Item className={`menu__item ${danger ? "menu__item--danger" : ""}`} onSelect={onSelect} disabled={disabled}>
      {icon}
      <span className="grow truncate">{children}</span>
      {hint && <span className="menu__hint">{hint}</span>}
    </RMenu.Item>
  );
}

export const MenuSep = () => <RMenu.Separator className="menu__sep" />;
export const MenuLabel = ({ children }: { children: ReactNode }) => <RMenu.Label className="menu__label">{children}</RMenu.Label>;

// ---- Toasts -------------------------------------------------------------------------------

export function Toasts() {
  const { toasts, dismiss } = useToasts();
  return (
    <div className="toasts" role="status" aria-live="polite">
      {toasts.map((t) => (
        <div key={t.id} className={`toast toast--${t.kind}`}>
          {t.kind === "success" ? <CheckCircle2 size={16} color="var(--success)" /> : t.kind === "error" ? <AlertTriangle size={16} color="var(--danger)" /> : <Info size={16} color="var(--info)" />}
          <div className="grow">
            <div className="toast__title">{t.title}</div>
            {t.body && <div className="toast__body">{t.body}</div>}
          </div>
          <button className="btn btn--ghost btn--icon btn--sm" aria-label="Dismiss" onClick={() => dismiss(t.id)}>
            <X />
          </button>
        </div>
      ))}
    </div>
  );
}

// ---- Small pieces --------------------------------------------------------------------------

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="kbd">{children}</kbd>;
}

export function Env({ env }: { env: string }) {
  const label = env === "development" ? "dev" : env === "production" ? "prod" : env;
  return <span className={`env env--${env}`}>{label}</span>;
}

export function Spinner({ large }: { large?: boolean }) {
  return <span className={`spinner ${large ? "spinner--lg" : ""}`} role="progressbar" aria-label="Loading" />;
}

export function Empty({ icon, title, children, action }: { icon?: ReactNode; title: ReactNode; children?: ReactNode; action?: ReactNode }) {
  return (
    <div className="empty">
      {icon && <div className="empty__icon">{icon}</div>}
      <div className="empty__title">{title}</div>
      {children && <div>{children}</div>}
      {action}
    </div>
  );
}

export function Alert({ kind = "info", title, children }: { kind?: "info" | "warn" | "danger" | "success"; title?: ReactNode; children?: ReactNode }) {
  const Icon = kind === "success" ? CheckCircle2 : kind === "info" ? Info : AlertTriangle;
  return (
    <div className={`alert alert--${kind}`} role={kind === "danger" ? "alert" : undefined}>
      <Icon />
      <div className="grow">
        {title && <div className="alert__title">{title}</div>}
        {children && <div>{children}</div>}
      </div>
    </div>
  );
}

// ---- Engine monograms ----------------------------------------------------------------------
// Brand-neutral badges instead of vendor logos.

const engines: Record<string, { mono: string; hue: string }> = {
  mysql: { mono: "My", hue: "#4f93c2" },
  mariadb: { mono: "Ma", hue: "#b98a5c" },
  postgres: { mono: "Pg", hue: "#5b7fc7" },
  mssql: { mono: "Ms", hue: "#c9564f" },
  oracle: { mono: "Or", hue: "#d0533d" },
  sqlite: { mono: "Sl", hue: "#6aa3b8" },
  mongodb: { mono: "Mg", hue: "#4ea96b" },
  bigquery: { mono: "Bq", hue: "#5f8fe6" },
};

export function EngineBadge({ driver, size = 26 }: { driver: string; size?: number }) {
  const e = engines[driver] ?? { mono: driver.slice(0, 2), hue: "#8793a6" };
  return (
    <span
      className="engine"
      style={{ width: size, height: size, fontSize: size * 0.42, ["--hue" as string]: e.hue }}
      aria-hidden="true"
    >
      {e.mono}
    </span>
  );
}

export function Field({ label, required, help, error, children, htmlFor }: { label: ReactNode; required?: boolean; help?: ReactNode; error?: ReactNode; children: ReactNode; htmlFor?: string }) {
  return (
    <div className="field">
      <label className="field__label" htmlFor={htmlFor}>
        {label}
        {required && <span className="req">*</span>}
      </label>
      {children}
      {error ? <div className="field__error">{error}</div> : help ? <div className="field__help">{help}</div> : null}
    </div>
  );
}
