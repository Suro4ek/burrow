import { api, type Session } from "../api";
import { usePoll } from "../usePoll";
import { ago, ConfirmButton, Empty } from "../ui";

export function Agents({ onUnauthorized }: { onUnauthorized: () => void }) {
  const { data, error, loading, refresh } = usePoll<Session[]>(
    api.sessions,
    2000,
    onUnauthorized,
  );

  const disconnect = async (id: string) => {
    await api.closeSession(id);
    refresh();
  };

  if (error) return <div className="alert">{error}</div>;
  if (loading && !data) return <Empty>loading…</Empty>;
  if (!data?.length) return <Empty>No agents are connected.</Empty>;

  return (
    <div className="table-wrap">
      <table>
        <thead>
          <tr>
            <th>Host</th>
            <th>Token</th>
            <th>Address</th>
            <th className="num">Tunnels</th>
            <th className="num">Connected</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {data.map((s) => (
            <tr key={s.id}>
              <td>
                <div className="stack">
                  <span>{s.hostname || "—"}</span>
                  <span className="hint">session {s.id}</span>
                </div>
              </td>
              <td>{s.token_name}</td>
              <td>
                <code>{s.agent_addr}</code>
              </td>
              <td className="num">{s.tunnels}</td>
              <td className="num">{ago(s.connected_at)}</td>
              <td className="actions">
                <ConfirmButton onConfirm={() => void disconnect(s.id)}>
                  disconnect
                </ConfirmButton>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
