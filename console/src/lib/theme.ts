import { useSyncExternalStore } from "react";

/** Light or dark, as the system is (no manual switch, like the house's other web UIs). The prepaint script of
 * @mqtt-home/ui (vite.config.ts) sets html.dark and html[data-theme] before the first paint and follows the system
 * live (always dark in the Wall Display panel); this reads it for the code that needs it in JS. */
export type Theme = "light" | "dark";

const isDark = () => document.documentElement.classList.contains("dark");

function subscribe(onChange: () => void) {
  const o = new MutationObserver(onChange);
  o.observe(document.documentElement, { attributes: true, attributeFilter: ["class"] });
  return () => o.disconnect();
}

export function useTheme(): Theme {
  return useSyncExternalStore(subscribe, () => (isDark() ? "dark" : "light"));
}
