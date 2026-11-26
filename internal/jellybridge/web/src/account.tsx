// The account page, at /sso/: signing in apps, and the user's sessions.
import { createMemo, createSignal, Errored, For, Loading, refresh, Show } from "solid-js";
import { render } from "@solidjs/web";
import * as api from "./api";
import "./style.css";

/** ago says how long ago when was, roughly. */
function ago(when: string): string {
  const minutes = Math.round((Date.now() - Date.parse(when)) / 60000);
  if (minutes < 2) return "just now";
  if (minutes < 120) return `${minutes} minutes ago`;
  const hours = Math.round(minutes / 60);
  if (hours < 48) return `${hours} hours ago`;
  return `${Math.round(hours / 24)} days ago`;
}

function QuickConnect(props: { onSignedIn: () => void }) {
  const [result, setResult] = createSignal("");
  const submit = async (e: SubmitEvent) => {
    e.preventDefault();
    const form = e.currentTarget as HTMLFormElement;
    const code = (form.elements.namedItem("code") as HTMLInputElement).value.trim();
    try {
      await api.quickConnect(code);
      setResult("Signed in. The app continues on its own.");
      form.reset();
      props.onSignedIn();
    } catch (err) {
      setResult(`Not signed in: ${api.message(err)}`);
    }
  };
  return (
    <section>
      <h2>Sign in an app with Quick Connect</h2>
      <p>Choose Quick Connect in the app, then enter the code it shows.</p>
      <form onSubmit={submit}>
        <input name="code" inputmode="numeric" autocomplete="off" placeholder="Code" required />
        <button>Sign in</button>
      </form>
      <div class="result">{result()}</div>
    </section>
  );
}

function AppCode() {
  const [code, setCode] = createSignal<api.AppCode>();
  const [error, setError] = createSignal("");
  const get = async (e: SubmitEvent) => {
    e.preventDefault();
    try {
      setCode(await api.appCode());
      setError("");
    } catch (err) {
      setCode(undefined);
      setError(`No code: ${api.message(err)}`);
    }
  };
  return (
    <section>
      <h2>Sign in an app without Quick Connect</h2>
      <p>
        For Infuse and other apps asking for a password: get a code, then sign in with this username and the code
        as the password.
      </p>
      <form onSubmit={get}>
        <button>Get a code</button>
      </form>
      <div class="result">
        {error()}
        <Show when={code()}>
          {(c) => (
            <>
              <div>Username: {c().Username}</div>
              <div class="code">{c().Code}</div>
              <div>It works once, within {Math.round(c().ExpiresIn / 60)} minutes.</div>
            </>
          )}
        </Show>
      </div>
    </section>
  );
}

function SessionRow(props: { session: api.Session; onRevoked: () => void }) {
  const [busy, setBusy] = createSignal(false);
  const signOut = async () => {
    setBusy(true);
    try {
      await api.revoke(props.session.ID);
      props.onRevoked();
    } catch (err) {
      setBusy(false);
      alert(`Couldn't sign it out: ${api.message(err)}`);
    }
  };
  return (
    <tr>
      <td>{props.session.Device || "Unknown device"}</td>
      <td>
        {props.session.Client}
        {props.session.Kind === "browser" ? " (browser)" : ""}
      </td>
      <td>{ago(props.session.LastUsed)}</td>
      <td>
        <button class="quiet" disabled={busy()} onClick={signOut}>
          Sign out
        </button>
      </td>
    </tr>
  );
}

function Account() {
  const sessions = createMemo(() => api.sessions());
  const reload = () => refresh(sessions);
  // An app signed in with Quick Connect makes its session once it asks for
  // its token, a moment later.
  const reloadSoon = () => setTimeout(reload, 3000);
  const note = (text: string) => (
    <tr>
      <td colspan="4">{text}</td>
    </tr>
  );
  return (
    <main>
      <h1>Jellyfin apps and sessions</h1>
      <QuickConnect onSignedIn={reloadSoon} />
      <AppCode />
      <section>
        <h2>Sessions</h2>
        <p>Browsers and apps signed in as you. A session unused for a long while ends by itself.</p>
        <table>
          <thead>
            <tr>
              <th>Device</th>
              <th>App</th>
              <th>Last used</th>
              <th></th>
            </tr>
          </thead>
          <tbody>
            <Errored fallback={(err) => note(`Couldn't list sessions: ${api.message(err())}`)}>
              <Loading fallback={note("Loading…")}>
                <For each={sessions()} keyed={(s) => s.ID} fallback={note("None.")}>
                  {(s) => <SessionRow session={s()} onRevoked={reload} />}
                </For>
              </Loading>
            </Errored>
          </tbody>
        </table>
      </section>
      <p>
        <a href="/web/">Back to Jellyfin</a>
      </p>
    </main>
  );
}

render(() => <Account />, document.getElementById("app")!);
