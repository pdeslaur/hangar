// The login page, at /sso/login: it signs jellyfin-web in with bridge.js,
// which the page loads first, then opens it.
import { createMemo, Errored, Loading } from "solid-js";
import { render } from "@solidjs/web";
import "./style.css";

declare const jellyfinBridge: { signIn(): Promise<void> };

function Login() {
  const signedIn = createMemo(async () => {
    await jellyfinBridge.signIn();
    location.replace("/web/");
    return true;
  });
  return (
    <main class="status">
      <Errored
        fallback={(err, reset) => (
          <>
            <p>Signing in failed: {err() instanceof Error ? (err() as Error).message : String(err())}</p>
            <p>
              <button onClick={reset}>Try again</button>
            </p>
          </>
        )}
      >
        <Loading fallback={<p>Signing in…</p>}>
          <p>{signedIn() ? "Opening Jellyfin…" : ""}</p>
        </Loading>
      </Errored>
    </main>
  );
}

render(() => <Login />, document.getElementById("app")!);
