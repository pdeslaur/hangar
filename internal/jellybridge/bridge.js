// Signs jellyfin-web in through jellyfin-bridge, and keeps it signed in.
//
// The bridge's login page calls jellyfinBridge.signIn(). In jellyfin-web,
// where the bridge adds this script, it refreshes the token when the
// bridge's RefreshBefore is all it has left, and sends jellyfin-web's own
// login and server pages to the bridge's: signing in is the bridge's.
(() => {
	const expiresKey = "jellyfin-bridge-expires";
	const refreshBeforeKey = "jellyfin-bridge-refresh-before";
	const loginPage = "/sso/login";

	// signIn asks the bridge for a token for this browser, by jellyfin-web's
	// own device ID, and stores it where jellyfin-web reads its credentials.
	async function signIn() {
		let deviceId = localStorage.getItem("_deviceId2");
		if (!deviceId) {
			// Made as jellyfin-web makes it when it has none.
			deviceId = btoa([navigator.userAgent, Date.now()].join("|")).replaceAll("=", "1");
			localStorage.setItem("_deviceId2", deviceId);
		}
		const resp = await fetch("/sso/session", {
			method: "POST",
			headers: { "Content-Type": "application/json" },
			body: JSON.stringify({ DeviceId: deviceId }),
		});
		if (!resp.ok) {
			throw new Error(await resp.text());
		}
		const session = await resp.json();
		const credentials = JSON.parse(localStorage.getItem("jellyfin_credentials") || "{}");
		const servers = (credentials.Servers || []).filter((s) => s.Id !== session.ServerId);
		servers.unshift({
			Id: session.ServerId,
			AccessToken: session.AccessToken,
			UserId: session.UserId,
			ManualAddress: location.origin,
			LastConnectionMode: 2,
			DateLastAccessed: Date.now(),
		});
		credentials.Servers = servers;
		localStorage.setItem("jellyfin_credentials", JSON.stringify(credentials));
		localStorage.setItem(expiresKey, String(Date.now() + session.ExpiresIn * 1000));
		localStorage.setItem(refreshBeforeKey, String(session.RefreshBefore * 1000));
		// The running jellyfin-web, if any, sends the new token from now on.
		if (window.ApiClient && typeof window.ApiClient.serverId === "function" && window.ApiClient.serverId() === session.ServerId) {
			window.ApiClient.setAuthenticationInfo(session.AccessToken, session.UserId);
		}
	}

	window.jellyfinBridge = { signIn };

	if (location.pathname === loginPage) {
		return;
	}

	let leaving = false;
	function onRoute() {
		if (!leaving && /^#\/(login|selectserver)\b/.test(location.hash)) {
			leaving = true;
			location.replace(loginPage);
		}
	}
	// jellyfin-web's router moves with the History API, which fires no
	// event of its own: watch its calls, and poll in case one is missed.
	for (const method of ["pushState", "replaceState"]) {
		const original = history[method];
		history[method] = function (...args) {
			const result = original.apply(this, args);
			onRoute();
			return result;
		};
	}
	window.addEventListener("hashchange", onRoute);
	window.addEventListener("popstate", onRoute);
	setInterval(onRoute, 1000);
	onRoute();

	let refreshing = false;
	async function check() {
		const expires = Number(localStorage.getItem(expiresKey));
		const refreshBefore = Number(localStorage.getItem(refreshBeforeKey));
		if (!expires || !refreshBefore || refreshing) {
			return;
		}
		const left = expires - Date.now();
		if (left > refreshBefore) {
			return;
		}
		refreshing = true;
		try {
			await signIn();
		} catch (e) {
			// The sign-on's session is gone, most likely: once the token is
			// too, sign in again through it.
			console.warn("jellyfin-bridge: refreshing the token failed", e);
			if (left < 60 * 1000) {
				location.replace(loginPage);
			}
		} finally {
			refreshing = false;
		}
	}
	setInterval(check, 60 * 1000);
	document.addEventListener("visibilitychange", () => {
		if (document.visibilityState === "visible") {
			check();
		}
	});
	check();
})();
