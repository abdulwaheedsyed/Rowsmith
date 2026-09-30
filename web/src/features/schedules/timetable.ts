// Timetables people pick, and the cron expressions they become. Anything
// else is kept as a custom cron expression.

export type Timetable =
  | { kind: "minutes"; every: number }
  | { kind: "hourly"; every: number; minute: number }
  | { kind: "days"; days: number[]; time: string }
  | { kind: "monthly"; day: number | "L"; time: string }
  | { kind: "cron"; expr: string };

export const MINUTE_STEPS = [5, 10, 15, 20, 30];
export const HOUR_STEPS = [1, 2, 3, 4, 6, 8, 12];
export const DAY_NAMES = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];
export const DAY_SHORT = ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"];
export const WEEK_ORDER = [1, 2, 3, 4, 5, 6, 0]; // Monday first

export const defaultTimetable = (): Timetable => ({ kind: "days", days: [1, 2, 3, 4, 5], time: "08:00" });

const pad = (n: number) => String(n).padStart(2, "0");

function hm(time: string): [number, number] {
  const m = /^(\d{1,2}):(\d{2})$/.exec(time.trim());
  if (!m) return [8, 0];
  return [Math.min(23, Number(m[1])), Math.min(59, Number(m[2]))];
}

export function toCron(t: Timetable): string {
  switch (t.kind) {
    case "minutes":
      return `*/${t.every} * * * *`;
    case "hourly":
      return t.every === 1 ? `${t.minute} * * * *` : `${t.minute} */${t.every} * * *`;
    case "days": {
      const [h, m] = hm(t.time);
      const days = [...new Set(t.days)].sort((a, b) => a - b);
      const dow = days.length === 7 || days.length === 0 ? "*" : days.join(",") === "1,2,3,4,5" ? "1-5" : days.join(",");
      return `${m} ${h} * * ${dow}`;
    }
    case "monthly": {
      const [h, m] = hm(t.time);
      return `${m} ${h} ${t.day} * *`;
    }
    case "cron":
      return t.expr.trim();
  }
}

const num = (s: string, lo: number, hi: number) => (/^\d+$/.test(s) && Number(s) >= lo && Number(s) <= hi ? Number(s) : null);

function parseDays(s: string): number[] | null {
  if (s === "*") return [0, 1, 2, 3, 4, 5, 6];
  const out = new Set<number>();
  for (const part of s.split(",")) {
    const r = /^(\d)(?:-(\d))?$/.exec(part);
    if (!r) return null;
    const a = Number(r[1]), b = r[2] ? Number(r[2]) : a;
    if (a > 7 || b > 7 || a > b) return null;
    for (let d = a; d <= b; d++) out.add(d % 7);
  }
  return [...out].sort((a, b) => a - b);
}

export function fromCron(expr: string): Timetable {
  const f = expr.trim().split(/\s+/);
  if (f.length === 5) {
    const [mi, ho, dom, mon, dow] = f;
    let m: RegExpExecArray | null;
    if ((m = /^\*\/(\d+)$/.exec(mi)) && ho === "*" && dom === "*" && mon === "*" && dow === "*" && MINUTE_STEPS.includes(Number(m[1]))) {
      return { kind: "minutes", every: Number(m[1]) };
    }
    const minute = num(mi, 0, 59);
    if (minute !== null && dom === "*" && mon === "*" && dow === "*") {
      if (ho === "*") return { kind: "hourly", every: 1, minute };
      if ((m = /^\*\/(\d+)$/.exec(ho)) && HOUR_STEPS.includes(Number(m[1]))) return { kind: "hourly", every: Number(m[1]), minute };
    }
    const hour = num(ho, 0, 23);
    if (minute !== null && hour !== null && mon === "*") {
      const time = `${pad(hour)}:${pad(minute)}`;
      if (dom === "*") {
        const days = parseDays(dow);
        if (days) return { kind: "days", days, time };
      } else if (dow === "*") {
        if (dom.toUpperCase() === "L") return { kind: "monthly", day: "L", time };
        const d = num(dom, 1, 31);
        if (d !== null) return { kind: "monthly", day: d, time };
      }
    }
  }
  return { kind: "cron", expr };
}

function ordinal(n: number) {
  const s = n % 100 >= 11 && n % 100 <= 13 ? "th" : ["th", "st", "nd", "rd"][n % 10] ?? "th";
  return `${n}${s}`;
}

function listDays(days: number[]) {
  const d = WEEK_ORDER.filter((x) => days.includes(x));
  if (d.length === 7) return "Every day";
  if (d.join() === "1,2,3,4,5") return "Weekdays";
  if (d.join() === "6,0") return "Weekends";
  const names = d.map((x) => DAY_NAMES[x] + "s");
  return names.length === 1 ? names[0] : names.slice(0, -1).join(", ") + " and " + names[names.length - 1];
}

export function describeTimetable(t: Timetable): string {
  switch (t.kind) {
    case "minutes":
      return `Every ${t.every} minutes`;
    case "hourly":
      return `${t.every === 1 ? "Every hour" : `Every ${t.every} hours`} at :${pad(t.minute)}`;
    case "days":
      return `${listDays(t.days)} at ${t.time}`;
    case "monthly":
      return `${t.day === "L" ? "The last day" : `The ${ordinal(t.day)}`} of every month at ${t.time}`;
    case "cron":
      return `Custom: ${t.expr}`;
  }
}

export const describeCron = (expr: string) => describeTimetable(fromCron(expr));

export function localZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC";
  } catch {
    return "UTC";
  }
}

export function zones(): string[] {
  try {
    const list = (Intl as unknown as { supportedValuesOf?(k: string): string[] }).supportedValuesOf?.("timeZone");
    if (list?.length) return list.includes("UTC") ? list : ["UTC", ...list];
  } catch {
    /* older browsers */
  }
  return ["UTC", localZone()];
}

/** Formats a time in the schedule's zone, e.g. "Thu 1 Oct, 08:00". */
export function atZone(ms: number, tz: string, withZone = false): string {
  try {
    return new Intl.DateTimeFormat(undefined, { weekday: "short", day: "numeric", month: "short", hour: "2-digit", minute: "2-digit", hourCycle: "h23", timeZone: tz, ...(withZone ? { timeZoneName: "short" } : {}) }).format(ms);
  } catch {
    return new Date(ms).toLocaleString();
  }
}

/** "in 3 h", "in 12 min", "tomorrow"… */
export function until(ms: number): string {
  const d = ms - Date.now();
  if (d <= 0) return "now";
  const min = Math.round(d / 60000);
  if (min < 60) return `in ${Math.max(1, min)} min`;
  const h = Math.round(min / 60);
  if (h < 36) return `in ${h} h`;
  return `in ${Math.round(h / 24)} days`;
}
