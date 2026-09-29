import { useEffect, useState } from "react";

export function useMediaQuery(query: string) {
  const [match, setMatch] = useState(() => typeof window !== "undefined" && window.matchMedia(query).matches);
  useEffect(() => {
    const mq = window.matchMedia(query);
    const on = () => setMatch(mq.matches);
    on();
    mq.addEventListener("change", on);
    return () => mq.removeEventListener("change", on);
  }, [query]);
  return match;
}

export function useDebounced<T>(value: T, ms = 300) {
  const [v, setV] = useState(value);
  useEffect(() => {
    const t = setTimeout(() => setV(value), ms);
    return () => clearTimeout(t);
  }, [value, ms]);
  return v;
}

export function useTheme(): "dark" | "light" {
  const [t, setT] = useState<"dark" | "light">(() => resolve());
  useEffect(() => {
    const mq = window.matchMedia("(prefers-color-scheme: light)");
    const on = () => setT(resolve());
    const obs = new MutationObserver(on);
    obs.observe(document.documentElement, { attributes: true, attributeFilter: ["data-theme"] });
    mq.addEventListener("change", on);
    return () => {
      obs.disconnect();
      mq.removeEventListener("change", on);
    };
  }, []);
  return t;
}

function resolve(): "dark" | "light" {
  const d = document.documentElement.dataset.theme;
  if (d === "light" || d === "dark") return d;
  return window.matchMedia("(prefers-color-scheme: light)").matches ? "light" : "dark";
}
