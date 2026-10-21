import { createContext, useCallback, useContext, useEffect, useState, type ReactNode } from 'react';
import { ApiError, get, onAuthProblem, post } from './api';
import type { Role, User } from './types';

const rank: Record<Role, number> = { viewer: 1, operator: 2, admin: 3 };

interface SessionValue {
  user: User | null;
  loading: boolean;
  login: (username: string, password: string) => Promise<User>;
  logout: () => Promise<void>;
  refresh: () => Promise<void>;
  setUser: (u: User) => void;
  /** At least this role. */
  can: (role: Role) => boolean;
}

const SessionContext = createContext<SessionValue | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<User | null>(null);
  const [loading, setLoading] = useState(true);

  const refresh = useCallback(async () => {
    try {
      const me = await get<{ user: User }>('/auth/me');
      setUser(me.user);
    } catch (e) {
      if (e instanceof ApiError && e.status === 401) setUser(null);
    } finally {
      setLoading(false);
    }
  }, []);

  useEffect(() => {
    refresh();
    return onAuthProblem((status) => {
      if (status === 401) setUser(null);
      else refresh();
    });
  }, [refresh]);

  const login = async (username: string, password: string) => {
    const res = await post<{ user: User }>('/auth/login', { username, password });
    setUser(res.user);
    return res.user;
  };

  const logout = async () => {
    try {
      await post('/auth/logout');
    } finally {
      setUser(null);
    }
  };

  const can = (role: Role) => !!user && rank[user.role] >= rank[role];

  return (
    <SessionContext.Provider value={{ user, loading, login, logout, refresh, setUser, can }}>
      {children}
    </SessionContext.Provider>
  );
}

export function useSession(): SessionValue {
  const v = useContext(SessionContext);
  if (!v) throw new Error('useSession outside SessionProvider');
  return v;
}
