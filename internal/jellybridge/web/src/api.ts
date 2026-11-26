// The bridge's API under /sso/, as sso.go serves it.

/** A session as GET /sso/sessions lists it. */
export type Session = {
  ID: string;
  Kind: "browser" | "app";
  Client: string;
  Device: string;
  Created: string;
  LastUsed: string;
};

/** A one-time code for an app without Quick Connect, from POST /sso/code. */
export type AppCode = {
  Username: string;
  Code: string;
  ExpiresIn: number;
};

async function send<T>(path: string, init?: RequestInit): Promise<T> {
  const resp = await fetch(path, init);
  if (!resp.ok) {
    throw new Error((await resp.text()).trim() || resp.statusText);
  }
  return resp.status === 204 ? (undefined as T) : resp.json();
}

/** POSTs body as JSON, which the bridge requires of every POST. */
export function post<T = void>(path: string, body: object = {}): Promise<T> {
  return send(path, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

export const sessions = () => send<Session[]>("/sso/sessions");
export const quickConnect = (code: string) => post("/sso/quickconnect", { Code: code });
export const appCode = () => post<AppCode>("/sso/code");
export const revoke = (id: string) => post("/sso/revoke", { ID: id });
