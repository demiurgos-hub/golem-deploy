import { createContext, createElement, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";

const THEME_KEY = "golem_deploy_theme";

function initialDark(): boolean {
  try {
    const saved = localStorage.getItem(THEME_KEY);
    if (saved) return saved === "dark";
  } catch {
    // Storage unavailable; fall back to the system preference.
  }
  return typeof window.matchMedia === "function" && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

interface Theme {
  dark: boolean;
  toggle: () => void;
}

const ThemeContext = createContext<Theme | null>(null);

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [dark, setDark] = useState(initialDark);

  useEffect(() => {
    document.documentElement.classList.toggle("dark", dark);
    document.documentElement.style.colorScheme = dark ? "dark" : "light";
    try {
      localStorage.setItem(THEME_KEY, dark ? "dark" : "light");
    } catch {
      // Not persisted; the choice still applies to this page.
    }
  }, [dark]);

  const toggle = useCallback(() => setDark((d) => !d), []);
  const value = useMemo(() => ({ dark, toggle }), [dark, toggle]);
  return createElement(ThemeContext.Provider, { value }, children);
}

export function useTheme(): Theme {
  const t = useContext(ThemeContext);
  if (!t) throw new Error("useTheme must be used inside ThemeProvider");
  return t;
}
