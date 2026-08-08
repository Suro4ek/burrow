import { useState } from "react";
import { api, type Overview, type Token, type TokenInput } from "../api";
import { usePoll } from "../usePoll";
import {
  ago,
  Badge,
  ConfirmButton,
  Copy,
  Empty,
  parseList,
  parsePorts,
  Secret,
} from "../ui";

/** loginCommand is the line a user pastes to set up their agent. */
function loginCommand(secret: string, overview: Overview | undefined): string {
  const host = overview?.public_host ?? "your-server";
  const port = overview?.control_port ?? "7000";
  const noTLS = overview && !overview.control_tls ? " -no-tls" : "";
  return `burrow login -server ${host}:${port} -token ${secret}${noTLS}`;
}

export function Tokens({
  overview,
  onUnauthorized,
}: {
  overview: Overview | undefined;
  onUnauthorized: () => void;
}) {
  const { data, error, loading, refresh } = usePoll<Token[]>(
    api.tokens,
    5000,
    onUnauthorized,
  );
  const [created, setCreated] = useState<Token | undefined>();
  const [editing, setEditing] = useState<string | undefined>();
  const [formError, setFormError] = useState<string | undefined>();

  const act = async (fn: () => Promise<unknown>) => {
    setFormError(undefined);
    try {
      await fn();
      refresh();
    } catch (err) {
      setFormError(err instanceof Error ? err.message : String(err));
    }
  };

  return (
    <div className="stack-lg">
      <NewTokenForm
        onCreate={async (input) => {
          const tok = await api.createToken(input);
          setCreated(tok);
          refresh();
        }}
      />

      {created && (
        <div className="callout">
          <div className="callout-head">
            <strong>Token created: {created.name}</strong>
            <button
              type="button"
              className="btn btn-ghost"
              onClick={() => setCreated(undefined)}
            >
              dismiss
            </button>
          </div>
          <p className="hint">
            Run this on the machine that should be tunnelled:
          </p>
          <div className="cmd">
            <code>{loginCommand(created.token, overview)}</code>
            <Copy value={loginCommand(created.token, overview)} label="copy" />
          </div>
          <p className="hint">
            Then <code>burrow ssh</code> or <code>burrow http 3000</code>.
          </p>
        </div>
      )}

      {formError && <div className="alert">{formError}</div>}
      {error && <div className="alert">{error}</div>}

      {loading && !data ? (
        <Empty>loading…</Empty>
      ) : !data?.length ? (
        <Empty>No tokens yet. Create one above.</Empty>
      ) : (
        <div className="table-wrap">
          <table>
            <thead>
              <tr>
                <th>Name</th>
                <th>Secret</th>
                <th>Reserved</th>
                <th className="num">Limit</th>
                <th className="num">Active</th>
                <th className="num">Last seen</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {data.map((t) =>
                editing === t.id ? (
                  <EditRow
                    key={t.id}
                    token={t}
                    onCancel={() => setEditing(undefined)}
                    onSave={async (input) => {
                      await act(() => api.updateToken(t.id, input));
                      setEditing(undefined);
                    }}
                  />
                ) : (
                  <tr key={t.id} className={t.disabled ? "row-disabled" : ""}>
                    <td>
                      <div className="stack">
                        <span>
                          {t.name}{" "}
                          {t.disabled && <Badge kind="warn">disabled</Badge>}
                          {t.deny_tcp && <Badge>no tcp</Badge>}
                        </span>
                        <span className="hint">created {ago(t.created_at)} ago</span>
                      </div>
                    </td>
                    <td>
                      <Secret value={t.token} />
                    </td>
                    <td>
                      <div className="stack">
                        <span className="hint">
                          subdomains: {t.subdomains?.join(", ") || "—"}
                        </span>
                        <span className="hint">
                          ports: {t.ports?.join(", ") || "—"}
                        </span>
                      </div>
                    </td>
                    <td className="num">{t.max_tunnels || "—"}</td>
                    <td className="num">
                      {t.active_sessions > 0 ? (
                        <Badge kind="ok">
                          {t.active_sessions} / {t.active_tunnels}
                        </Badge>
                      ) : (
                        "—"
                      )}
                    </td>
                    <td className="num">{ago(t.last_seen)}</td>
                    <td className="actions">
                      <button
                        type="button"
                        className="btn btn-ghost"
                        onClick={() => setEditing(t.id)}
                      >
                        edit
                      </button>
                      <button
                        type="button"
                        className="btn btn-ghost"
                        onClick={() =>
                          void act(() =>
                            api.updateToken(t.id, { disabled: !t.disabled }),
                          )
                        }
                      >
                        {t.disabled ? "enable" : "disable"}
                      </button>
                      <ConfirmButton
                        confirmLabel="rotate?"
                        onConfirm={() =>
                          void act(async () => {
                            const tok = await api.rotateToken(t.id);
                            setCreated(tok);
                          })
                        }
                      >
                        rotate
                      </ConfirmButton>
                      <ConfirmButton
                        confirmLabel="delete?"
                        onConfirm={() => void act(() => api.deleteToken(t.id))}
                      >
                        delete
                      </ConfirmButton>
                    </td>
                  </tr>
                ),
              )}
            </tbody>
          </table>
          <p className="hint footnote">
            Reserving a subdomain or port locks it to that token — nobody else
            can take it, and <code>burrow ssh</code> picks a reserved port
            automatically, so the address stays the same across restarts.
            Rotating or deleting a token disconnects its agents immediately.
          </p>
        </div>
      )}
    </div>
  );
}

function NewTokenForm({
  onCreate,
}: {
  onCreate: (input: TokenInput) => Promise<void>;
}) {
  const [open, setOpen] = useState(false);
  const [name, setName] = useState("");
  const [subdomains, setSubdomains] = useState("");
  const [ports, setPorts] = useState("");
  const [maxTunnels, setMaxTunnels] = useState("");
  const [denyTCP, setDenyTCP] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | undefined>();

  if (!open) {
    return (
      <div>
        <button
          type="button"
          className="btn btn-primary"
          onClick={() => setOpen(true)}
        >
          New token
        </button>
      </div>
    );
  }

  const submit = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(undefined);

    if (!name.trim()) {
      setError("name is required");
      return;
    }
    const { ports: portList, error: portError } = parsePorts(ports);
    if (portError) {
      setError(portError);
      return;
    }
    const max = maxTunnels.trim() === "" ? 0 : Number(maxTunnels);
    if (!Number.isInteger(max) || max < 0) {
      setError("tunnel limit must be a whole number");
      return;
    }

    setBusy(true);
    try {
      await onCreate({
        name: name.trim(),
        subdomains: parseList(subdomains),
        ports: portList,
        max_tunnels: max,
        deny_tcp: denyTCP,
      });
      setName("");
      setSubdomains("");
      setPorts("");
      setMaxTunnels("");
      setDenyTCP(false);
      setOpen(false);
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setBusy(false);
    }
  };

  return (
    <form className="card" onSubmit={submit}>
      <div className="form-grid">
        <label>
          <span>Name</span>
          <input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="laptop"
            autoFocus
          />
        </label>
        <label>
          <span>Reserved subdomains</span>
          <input
            value={subdomains}
            onChange={(e) => setSubdomains(e.target.value)}
            placeholder="dev, api"
          />
        </label>
        <label>
          <span>Reserved TCP ports</span>
          <input
            value={ports}
            onChange={(e) => setPorts(e.target.value)}
            placeholder="25343"
          />
        </label>
        <label>
          <span>Tunnel limit</span>
          <input
            value={maxTunnels}
            onChange={(e) => setMaxTunnels(e.target.value)}
            placeholder="unlimited"
            inputMode="numeric"
          />
        </label>
      </div>
      <label className="check">
        <input
          type="checkbox"
          checked={denyTCP}
          onChange={(e) => setDenyTCP(e.target.checked)}
        />
        <span>Forbid TCP tunnels (HTTP only)</span>
      </label>

      {error && <div className="alert">{error}</div>}

      <div className="row">
        <button type="submit" className="btn btn-primary" disabled={busy}>
          {busy ? "creating…" : "Create token"}
        </button>
        <button
          type="button"
          className="btn btn-ghost"
          onClick={() => setOpen(false)}
        >
          cancel
        </button>
      </div>
    </form>
  );
}

function EditRow({
  token,
  onSave,
  onCancel,
}: {
  token: Token;
  onSave: (input: TokenInput) => Promise<void>;
  onCancel: () => void;
}) {
  const [name, setName] = useState(token.name);
  const [subdomains, setSubdomains] = useState(
    (token.subdomains ?? []).join(", "),
  );
  const [ports, setPorts] = useState((token.ports ?? []).join(", "));
  const [maxTunnels, setMaxTunnels] = useState(
    token.max_tunnels ? String(token.max_tunnels) : "",
  );
  const [denyTCP, setDenyTCP] = useState(Boolean(token.deny_tcp));
  const [error, setError] = useState<string | undefined>();

  const save = () => {
    const { ports: portList, error: portError } = parsePorts(ports);
    if (portError) {
      setError(portError);
      return;
    }
    const max = maxTunnels.trim() === "" ? 0 : Number(maxTunnels);
    if (!Number.isInteger(max) || max < 0) {
      setError("tunnel limit must be a whole number");
      return;
    }
    void onSave({
      name: name.trim(),
      subdomains: parseList(subdomains),
      ports: portList,
      max_tunnels: max,
      deny_tcp: denyTCP,
    });
  };

  return (
    <tr className="row-editing">
      <td>
        <input value={name} onChange={(e) => setName(e.target.value)} />
      </td>
      <td className="hint">editing…</td>
      <td>
        <div className="stack">
          <input
            value={subdomains}
            onChange={(e) => setSubdomains(e.target.value)}
            placeholder="subdomains"
          />
          <input
            value={ports}
            onChange={(e) => setPorts(e.target.value)}
            placeholder="ports"
          />
        </div>
      </td>
      <td>
        <input
          className="narrow"
          value={maxTunnels}
          onChange={(e) => setMaxTunnels(e.target.value)}
          inputMode="numeric"
        />
      </td>
      <td colSpan={2}>
        <label className="check">
          <input
            type="checkbox"
            checked={denyTCP}
            onChange={(e) => setDenyTCP(e.target.checked)}
          />
          <span>no tcp</span>
        </label>
        {error && <div className="alert">{error}</div>}
      </td>
      <td className="actions">
        <button type="button" className="btn btn-primary" onClick={save}>
          save
        </button>
        <button type="button" className="btn btn-ghost" onClick={onCancel}>
          cancel
        </button>
      </td>
    </tr>
  );
}
