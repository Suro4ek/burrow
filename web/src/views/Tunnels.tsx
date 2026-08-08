import { api, type Overview, type Tunnel } from "../api";
import { usePoll } from "../usePoll";
import { ago, Badge, bytes, ConfirmButton, Copy, Empty } from "../ui";

/** sshHint renders the command a user would actually run for a TCP tunnel. */
function sshHint(t: Tunnel, overview: Overview | undefined): string {
  const host = overview?.public_host ?? "";
  return `ssh -p ${t.port} user@${host}`;
}

export function Tunnels({
  overview,
  onUnauthorized,
}: {
  overview: Overview | undefined;
  onUnauthorized: () => void;
}) {
  const { data, error, loading, refresh } = usePoll<Tunnel[]>(
    api.tunnels,
    2000,
    onUnauthorized,
  );

  const close = async (id: string) => {
    await api.closeTunnel(id);
    refresh();
  };

  if (error) return <div className="alert">{error}</div>;
  if (loading && !data) return <Empty>loading…</Empty>;
  if (!data?.length) {
    return (
      <Empty>
        No tunnels are open. Start one with <code>burrow http 3000</code> or{" "}
        <code>burrow ssh</code>.
      </Empty>
    );
  }

  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Public address</th>
            <th>Local</th>
            <th>Token</th>
            <th>Agent</th>
            <th className="num">Conns</th>
            <th className="num">In / Out</th>
            <th className="num">Age</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {data.map((t) => (
            <tr key={t.id}>
              <td>
                <div className="stack">
                  <span>
                    <Badge kind={t.proto}>{t.proto}</Badge>{" "}
                    {t.proto === "http" ? (
                      <a href={t.public} target="_blank" rel="noreferrer">
                        {t.public}
                      </a>
                    ) : (
                      <code>{t.public}</code>
                    )}
                    <Copy value={t.public} />
                  </span>
                  {t.proto === "tcp" && (
                    <span className="hint">
                      <code>{sshHint(t, overview)}</code>
                    </span>
                  )}
                </div>
              </td>
              <td>
                <code>{t.local}</code>
              </td>
              <td>{t.token_name}</td>
              <td>
                <div className="stack">
                  <span>{t.hostname || "—"}</span>
                  <span className="hint">{t.agent_addr}</span>
                </div>
              </td>
              <td className="num">{t.conns}</td>
              <td className="num">
                {bytes(t.bytes_in)} / {bytes(t.bytes_out)}
              </td>
              <td className="num">{ago(t.created_at)}</td>
              <td className="actions">
                <ConfirmButton onConfirm={() => void close(t.id)}>
                  close
                </ConfirmButton>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="hint footnote">
        Closing a tunnel drops the agent's connection. A running agent
        reconnects on its own within a few seconds — disable or delete its
        token to keep it out.
      </p>
    </div>
  );
}
