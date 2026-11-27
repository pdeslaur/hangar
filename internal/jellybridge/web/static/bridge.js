// Signs jellyfin-web in through jellyfin-bridge, and keeps it signed in.
//
// The bridge's login page calls jellyfinBridge.store() with the session it
// made. In jellyfin-web, where the bridge adds this script, it refreshes
// the token when the bridge's RefreshBefore is all it has left, and sends
// jellyfin-web's own login and server pages to the bridge's: signing in is
// the bridge's. Once jellyfin-web signs out, which the bridge marks with a
// cookie, it signs out of the single sign-on too.
(() => {
	const expiresKey = "jellyfin-bridge-expires";
	const refreshBeforeKey = "jellyfin-bridge-refresh-before";
	const signedOutCookie = "jellyfin-bridge-signed-out";
	const loginPage = "/sso/login";
	const logoutPage = "/sso/logout";

	// store stores a session from the bridge, {AccessToken, UserId,
	// ServerId, DeviceId, ExpiresIn, RefreshBefore}, where jellyfin-web
	// reads its credentials, with the browser's device ID.
	function store(session) {
		localStorage.setItem("_deviceId2", session.DeviceId);
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

	// refresh asks the bridge for a new token for this browser's session.
	async function refresh() {
		const resp = await fetch("/sso/session", { method: "POST" });
		if (!resp.ok) {
			throw new Error(await resp.text());
		}
		store(await resp.json());
	}

	window.jellyfinBridge = { store };

	if (location.pathname === loginPage) {
		return;
	}

	// signInPage is where jellyfin-web's login page sends the browser: the
	// bridge's, or, once jellyfin-web signed out, the single sign-on's
	// logout.
	function signInPage() {
		if (!document.cookie.split("; ").includes(signedOutCookie + "=1")) {
			return loginPage;
		}
		document.cookie = signedOutCookie + "=; Path=/; Max-Age=0; Secure; SameSite=Lax";
		return logoutPage;
	}

	// A token that expired while the page was closed, or the computer
	// asleep: sign in again before jellyfin-web starts with it.
	const storedExpires = Number(localStorage.getItem(expiresKey));
	if (storedExpires && storedExpires - Date.now() < 10 * 1000) {
		location.replace(loginPage);
		return;
	}

	// addMenuItems adds "My devices", the bridge's account page, after the
	// Quick Connect item of jellyfin-web's user menu, which has no setting
	// for it: the avatar's menu in the new layout, the user's page in the
	// classic one. The item is a copy of Quick Connect's, styled alike,
	// which React doesn't know of: a plain link. Without a Quick Connect
	// item, as with Quick Connect off, there's none, and the side menu's
	// link from config.json stays.
	// The new layout hides the classic one's, by its class, which the copy
	// keeps.
	const quickConnectItems = '#app-user-menu a[href$="/quickconnect"], a.lnkQuickConnectPreferences:not([data-jellyfin-bridge])';
	function addMenuItems() {
		for (const quickConnect of document.querySelectorAll(quickConnectItems)) {
			if (quickConnect.nextElementSibling?.dataset.jellyfinBridge) {
				continue;
			}
			const item = quickConnect.cloneNode(true);
			item.dataset.jellyfinBridge = "devices";
			item.setAttribute("href", "/sso/");
			item.classList.remove("Mui-focusVisible", "Mui-selected");
			// The label: the first text that isn't an icon's.
			const walker = document.createTreeWalker(item, NodeFilter.SHOW_TEXT);
			for (let text = walker.nextNode(); text; text = walker.nextNode()) {
				if (text.textContent.trim() && !text.parentElement.closest(".material-icons")) {
					text.textContent = "My devices";
					break;
				}
			}
			// The icon: an SVG in the new layout, a class in the classic one.
			const svg = item.querySelector("svg");
			if (svg) {
				const icon = document.createElement("span");
				icon.className = "material-icons";
				icon.setAttribute("aria-hidden", "true");
				icon.textContent = "devices";
				svg.replaceWith(icon);
			}
			item.querySelector(".material-icons.phonelink_lock")?.classList.replace("phonelink_lock", "devices");
			quickConnect.after(item);
		}
	}
	new MutationObserver(addMenuItems).observe(document.documentElement, { childList: true, subtree: true });

	let leaving = false;
	function onRoute() {
		if (!leaving && /^#\/(login|selectserver)\b/.test(location.hash)) {
			leaving = true;
			location.replace(signInPage());
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
			await refresh();
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
