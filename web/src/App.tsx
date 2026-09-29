import { useEffect } from "react";
import { Router, useLocation } from "wouter";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, get, onUnauthorized, setCsrf } from "./lib/api";
import type { Bootstrap, Me } from "./lib/types";
import { AnvilMark, TipProvider, Toasts } from "./components/ui";
import { Setup, Login, MfaChallenge, EnrollScreen, ChangePasswordScreen } from "./features/auth/Auth";
import { Shell } from "./features/shell/Shell";
import { useWorkspace } from "./lib/store";
import { setNavigator } from "./lib/nav";

function NavBridge() {
  const [, navigate] = useLocation();
  useEffect(() => setNavigator(navigate), [navigate]);
  return null;
}

function Splash() {
  return (
    <div className="splash">
      <AnvilMark size={40} />
    </div>
  );
}

export function App() {
  const qc = useQueryClient();
  const boot = useQuery({ queryKey: ["bootstrap"], queryFn: () => get<Bootstrap>("bootstrap"), staleTime: Infinity });
  const me = useQuery({
    queryKey: ["me"],
    queryFn: async () => {
      try {
        const m = await get<Me>("auth/me");
        setCsrf(m.csrf);
        return m;
      } catch (e) {
        if (e instanceof ApiError && (e.status === 401 || e.status === 403)) return null;
        throw e;
      }
    },
    staleTime: Infinity,
    enabled: boot.data ? !boot.data.setupRequired : false,
  });

  useEffect(() => onUnauthorized(() => qc.setQueryData(["me"], null)), [qc]);
  useEffect(() => {
    if (me.data?.stage === "full") useWorkspace.getState().setOwner(me.data.user.id);
  }, [me.data]);

  let screen;
  if (boot.isLoading) screen = <Splash />;
  else if (boot.error) screen = <div className="splash"><p className="muted">Rowsmith is unreachable. Check that the server is running.</p></div>;
  else if (boot.data?.setupRequired && !me.data) screen = <Setup />;
  else if (me.isLoading) screen = <Splash />;
  else if (!me.data) screen = <Login />;
  else if (me.data.stage === "mfa") screen = <MfaChallenge />;
  else if (me.data.stage === "enroll") screen = <EnrollScreen />;
  else if (me.data.user.mustChangePassword) screen = <ChangePasswordScreen />;
  else screen = <Shell me={me.data} />;

  const base = (boot.data?.basePath ?? "/").replace(/\/$/, "");
  return (
    <Router base={base}>
      <NavBridge />
      <TipProvider delayDuration={350} skipDelayDuration={150}>
        {screen}
        <Toasts />
      </TipProvider>
    </Router>
  );
}
