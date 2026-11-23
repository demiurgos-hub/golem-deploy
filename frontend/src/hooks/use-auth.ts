import {
  createContext,
  useContext,
  useState,
  useEffect,
  useCallback,
  type ReactNode,
} from "react";
import { createElement } from "react";
import { apiClient } from "@/api/client";

const TOKEN_KEY = "golem_deploy_token";

interface AuthContextValue {
  token: string | null;
  isLoading: boolean;
  isAdmin: boolean;
  login: (
    username: string,
    password: string,
    rememberMe: boolean,
  ) => Promise<void>;
  logout: () => void;
  register: (username: string, password: string) => Promise<void>;
}

const AuthContext = createContext<AuthContextValue | null>(null);

function parseAccessLevel(token: string): number {
  try {
    const payload = JSON.parse(atob(token.split(".")[1]));
    return payload.access_level ?? 1;
  } catch {
    return 1;
  }
}

function getSavedToken(): { token: string; storage: Storage } | null {
  const local = localStorage.getItem(TOKEN_KEY);
  if (local) return { token: local, storage: localStorage };
  const session = sessionStorage.getItem(TOKEN_KEY);
  if (session) return { token: session, storage: sessionStorage };
  return null;
}

export function AuthProvider({ children }: { children: ReactNode }) {
  const [token, setToken] = useState<string | null>(null);
  // Only show the loading state while a saved token is being refreshed.
  const [isLoading, setIsLoading] = useState(() => getSavedToken() !== null);
  const [isAdmin, setIsAdmin] = useState(false);

  useEffect(() => {
    const saved = getSavedToken();
    if (!saved) return;

    apiClient.setToken(saved.token);
    apiClient
      .refresh()
      .then((resp) => {
        if (resp.status === "success" && resp.token) {
          saved.storage.setItem(TOKEN_KEY, resp.token);
          setToken(resp.token);
          setIsAdmin(parseAccessLevel(resp.token) === 0);
        } else {
          localStorage.removeItem(TOKEN_KEY);
          sessionStorage.removeItem(TOKEN_KEY);
          apiClient.setToken(null);
        }
      })
      .catch(() => {
        localStorage.removeItem(TOKEN_KEY);
        sessionStorage.removeItem(TOKEN_KEY);
        apiClient.setToken(null);
      })
      .finally(() => setIsLoading(false));
  }, []);

  const login = useCallback(
    async (username: string, password: string, rememberMe: boolean) => {
      const resp = await apiClient.login(username, password);
      if (resp.status !== "success" || !resp.token) {
        throw new Error(resp.detail || resp.message || "Login failed");
      }
      const storage = rememberMe ? localStorage : sessionStorage;
      storage.setItem(TOKEN_KEY, resp.token);
      setToken(resp.token);
      setIsAdmin(parseAccessLevel(resp.token) === 0);
    },
    [],
  );

  const logout = useCallback(() => {
    apiClient.setToken(null);
    localStorage.removeItem(TOKEN_KEY);
    sessionStorage.removeItem(TOKEN_KEY);
    setToken(null);
    setIsAdmin(false);
  }, []);

  const register = useCallback(
    async (username: string, password: string) => {
      await apiClient.createAccount(username, password);
    },
    [],
  );

  return createElement(
    AuthContext.Provider,
    { value: { token, isLoading, isAdmin, login, logout, register } },
    children,
  );
}

export function useAuth() {
  const ctx = useContext(AuthContext);
  if (!ctx) throw new Error("useAuth must be used within AuthProvider");
  return ctx;
}
