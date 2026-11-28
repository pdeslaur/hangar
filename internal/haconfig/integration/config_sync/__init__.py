"""ha-config-sync's agent inside Home Assistant.

It serves HTTP on a Unix socket in the configuration directory, which only
the containers sharing that directory reach: ha-config-sync asks it to check
the configuration, to reload what it deployed, and to tell people how a
deploy or a sync went, without a token.
"""

from __future__ import annotations

import logging
import os

from aiohttp import web

from homeassistant.components import persistent_notification
from homeassistant.config import async_check_ha_config_file
from homeassistant.const import EVENT_HOMEASSISTANT_STOP, __version__
from homeassistant.core import Event, HomeAssistant
from homeassistant.exceptions import HomeAssistantError
from homeassistant.helpers import config_validation as cv
from homeassistant.helpers.typing import ConfigType

_LOGGER = logging.getLogger(__name__)

DOMAIN = "config_sync"
CONFIG_SCHEMA = cv.empty_config_schema(DOMAIN)

# The directory, under the configuration directory, holding the socket.
DIRECTORY = ".config_sync"
SOCKET = "api.sock"

# The reload service of each domain ha-config-sync deploys. "all" is for
# packages, which can hold any integration's configuration.
RELOADS = {
    "automation": ("automation", "reload"),
    "script": ("script", "reload"),
    "scene": ("scene", "reload"),
    "all": ("homeassistant", "reload_all"),
}

# What ha-config-sync reports on: deploying from git, or syncing the UI's
# edits to git. Each has its own notification.
KINDS = ("deploy", "sync")


async def async_setup(hass: HomeAssistant, config: ConfigType) -> bool:
    """Serve ha-config-sync on the socket until Home Assistant stops."""
    directory = hass.config.path(DIRECTORY)
    path = os.path.join(directory, SOCKET)
    await hass.async_add_executor_job(_prepare, directory, path)

    async def check(request: web.Request) -> web.Response:
        errors = await async_check_ha_config_file(hass)
        return web.json_response({"errors": errors or ""})

    async def reload(request: web.Request) -> web.Response:
        body = await request.json()
        domains = body.get("domains")
        if not isinstance(domains, list) or not all(d in RELOADS for d in domains):
            return _error(400, f"domains must be a list of {sorted(RELOADS)}")
        for domain in domains:
            try:
                await hass.services.async_call(*RELOADS[domain], blocking=True)
            except HomeAssistantError as err:
                return _error(500, f"reloading {domain}: {err}")
        return web.json_response({})

    async def failed(request: web.Request) -> web.Response:
        body = await request.json()
        kind, sha, message = body.get("kind"), str(body.get("sha", "")), str(body.get("message", ""))
        if kind not in KINDS:
            return _error(400, f"kind must be one of {KINDS}")
        persistent_notification.async_create(
            hass,
            message,
            title=f"Config {kind} failed",
            notification_id=f"{DOMAIN}_{kind}",
        )
        hass.bus.async_fire(f"{DOMAIN}_failed", {"kind": kind, "sha": sha, "message": message})
        return web.json_response({})

    async def succeeded(request: web.Request) -> web.Response:
        body = await request.json()
        kind, sha = body.get("kind"), str(body.get("sha", ""))
        if kind not in KINDS:
            return _error(400, f"kind must be one of {KINDS}")
        persistent_notification.async_dismiss(hass, f"{DOMAIN}_{kind}")
        hass.bus.async_fire(f"{DOMAIN}_succeeded", {"kind": kind, "sha": sha})
        return web.json_response({})

    async def version(request: web.Request) -> web.Response:
        return web.json_response({"version": __version__})

    app = web.Application()
    app.add_routes(
        [
            web.post("/check", check),
            web.post("/reload", reload),
            web.post("/failed", failed),
            web.post("/succeeded", succeeded),
            web.get("/version", version),
        ]
    )
    runner = web.AppRunner(app, access_log=None)
    await runner.setup()
    await web.UnixSite(runner, path).start()
    await hass.async_add_executor_job(os.chmod, path, 0o600)
    _LOGGER.info("Serving ha-config-sync on %s", path)

    async def stop(event: Event) -> None:
        await runner.cleanup()

    hass.bus.async_listen_once(EVENT_HOMEASSISTANT_STOP, stop)
    return True


def _prepare(directory: str, path: str) -> None:
    """Make the socket's directory, and remove a socket left by a crash."""
    os.makedirs(directory, mode=0o700, exist_ok=True)
    try:
        os.remove(path)
    except FileNotFoundError:
        pass


def _error(status: int, message: str) -> web.Response:
    return web.json_response({"error": message}, status=status)
