import { useMemo } from "react";

// A week at a glance: one column per day in the schedule's time zone, with a
// tick at each run's time of day. Dense schedules fill their columns.

interface Parts {
  key: string;
  minute: number;
}

function partsIn(tz: string) {
  let fmt: Intl.DateTimeFormat;
  try {
    fmt = new Intl.DateTimeFormat("en-CA", { timeZone: tz, year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
  } catch {
    fmt = new Intl.DateTimeFormat("en-CA", { timeZone: "UTC", year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hourCycle: "h23" });
  }
  return (ms: number): Parts => {
    const p: Record<string, string> = {};
    for (const x of fmt.formatToParts(ms)) p[x.type] = x.value;
    return { key: `${p.year}-${p.month}-${p.day}`, minute: Number(p.hour) * 60 + Number(p.minute) };
  };
}

export function WeekStrip({ runs, timezone }: { runs: number[]; timezone: string }) {
  const { days, now } = useMemo(() => {
    const at = partsIn(timezone);
    const today = at(Date.now());
    const [y, m, d] = today.key.split("-").map(Number);
    const days = Array.from({ length: 7 }, (_, i) => {
      const date = new Date(Date.UTC(y, m - 1, d + i));
      return {
        key: date.toISOString().slice(0, 10),
        name: date.toLocaleDateString(undefined, { weekday: "short", timeZone: "UTC" }),
        num: date.getUTCDate(),
        ticks: [] as number[],
      };
    });
    const byKey = new Map(days.map((x) => [x.key, x]));
    for (const r of runs) byKey.get(at(r).key)?.ticks.push(at(r).minute);
    return { days, now: today.minute };
  }, [runs, timezone]);

  return (
    <div className="weekstrip" role="img" aria-label={`${runs.length} runs in the next 7 days`}>
      <div className="weekstrip__hours" aria-hidden>
        <span>0</span><span>6</span><span>12</span><span>18</span><span>24</span>
      </div>
      {days.map((day, i) => (
        <div key={day.key} className={`weekstrip__day ${day.ticks.length ? "has-runs" : ""}`}>
          <div className="weekstrip__label"><span>{day.name}</span><b>{day.num}</b></div>
          <svg className="weekstrip__track" viewBox="0 0 10 1440" preserveAspectRatio="none" aria-hidden>
            {[360, 720, 1080].map((y) => <line key={y} x1="0" x2="10" y1={y} y2={y} className="weekstrip__grid" vectorEffect="non-scaling-stroke" />)}
            {day.ticks.map((t, j) => <line key={j} x1="1.5" x2="8.5" y1={t} y2={t} className="weekstrip__tick" vectorEffect="non-scaling-stroke" />)}
            {i === 0 && <line x1="0" x2="10" y1={now} y2={now} className="weekstrip__now" vectorEffect="non-scaling-stroke" />}
          </svg>
          <div className="weekstrip__count">{day.ticks.length || ""}</div>
        </div>
      ))}
    </div>
  );
}
