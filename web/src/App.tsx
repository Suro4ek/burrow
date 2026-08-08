import { useCallback, useEffect, useState } from "react";
import { api, isUnauthorized, type Overview } from "./api";
import { usePoll } from "./usePoll";
import { bytes } from "./ui";
import { Tunnels } from "./views/Tunnels";
import { Tokens } from "./views/Tokens";
import { Agents } from "./views/Agents";

type Auth = "checking" | "out" | "in";
type Tab = "tunnels" | "tokens" | "agents";

export function App() {
  const [auth, setAuth] = useState<Auth>("checking");
  const [tab, setTab] = useState<Tab>(readTab);

  useEffect(() => {
    api
      .me()
      .then((r) => setAuth(r.authenticated ? "in" : "out"))
      .catch(() => setAuth("out"));
  }, []);

  // Keep the tab in the URL hash so a refresh lands where you were.
  useEffect(() => {
    window.location.hash = tab;
  }, [tab]);

  const signOut = useCallback(() => setAuth("out"), []);

  if (auth === "checking") return <div className="boot">…</div>;
  if (auth === "out") return <Login onSignedIn={() => setAuth("in")} />;

  return <Shell tab={tab} setTab={setTab} onSignOut={signOut} />;
}

function readTab(): Tab {
  const h = window.location.hash.replace("#", "");
  return h === "tokens" || h === "agents" ? h : "tunnels";
}

function Login({ onSignedIn }: { onSignedIn: () => void }) {
  const [password, setPassword] = useState("");
  const [error, setError] = useState<string | undefined>();
  const [busy, setBusy] = useState(false);

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setBusy(true);
    setError(undefined);
    try {
      await api.login(password);
      setPassword("");
      onSignedIn();
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login">
      <form className="card" onSubmit={submit}>
        <h1>burrow</h1>
        <p className="hint">Admin panel</p>
        <label>
          <span>Password</span>
          <input
            type="password"
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            autoFocus
            autoComplete="current-password"
          />
        </label>
        {error && <div className="alert">{error}</div>}
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? "signing in…" : "Sign in"}
        </button>
      </form>
    </div>
  );
}

function Shell({
  tab,
  setTab,
  onSignOut,
}: {
  tab: Tab;
  setTab: (t: Tab) => void;
  onSignOut: () => void;
}) {
  const { data: overview } = usePoll<Overview>(api.overview, 3000, onSignOut);

  const signOut = async () => {
    try {
      await api.logout();
    } catch (err) {
      // A dead session is exactly the state we want to end up in anyway.
      if (!isUnauthorized(err)) throw err;
    }
    onSignOut();
  };

  return (
    <div className="app">
      <header>
        <div className="brand">
          <strong>{overview?.base_domain ?? "burrow"}</strong>
          <span className="hint">burrowd {overview?.version ?? ""}</span>
        </div>

        <div className="stats">
          <Stat label="tunnels" value={overview?.tunnels ?? 0} />
          <Stat label="agents" value={overview?.sessions ?? 0} />
          <Stat label="tokens" value={overview?.tokens ?? 0} />
          <Stat
            label="traffic"
            value={
              overview
                ? `${bytes(overview.bytes_in)} / ${bytes(overview.bytes_out)}`
                : "—"
            }
          />
        </div>

        <button type="button" className="btn btn-ghost" onClick={() => void signOut()}>
          sign out
        </button>
      </header>

      <nav className="tabs">
        {(["tunnels", "tokens", "agents"] as const).map((t) => (
          <button
            key={t}
            type="button"
            className={t === tab ? "tab tab-active" : "tab"}
            onClick={() => setTab(t)}
          >
            {t}
          </button>
        ))}
      </nav>

      <main>
        {tab === "tunnels" && (
          <Tunnels overview={overview} onUnauthorized={onSignOut} />
        )}
        {tab === "tokens" && (
          <Tokens overview={overview} onUnauthorized={onSignOut} />
        )}
        {tab === "agents" && <Agents onUnauthorized={onSignOut} />}
      </main>

      <footer className="hint">
        {overview && (
          <>
            TCP pool {overview.tcp_min}–{overview.tcp_max} on{" "}
            <code>{overview.public_host}</code> · control port{" "}
            <code>{overview.control_port}</code>
            {!overview.control_tls && " (no TLS)"}
          </>
        )}
      </footer>
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string | number }) {
  return (
    <div className="stat">
      <span className="stat-value">{value}</span>
      <span className="stat-label">{label}</span>
    </div>
  );
}
