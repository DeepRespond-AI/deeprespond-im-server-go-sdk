"""连接真实 Demo / harness 的安全回归（会注册随机测试账号并吊销整个测试应用的会话）。

只用于独立的 SDK harness 测试应用，不连接生产环境。
IM_DEMO_TEST_URL=http://127.0.0.1:18392 IM_TEST_HARNESS=http://127.0.0.1:18890 python demo/testdata/security_e2e.py
"""

from __future__ import annotations

import asyncio
import base64
import contextlib
import inspect
import json
import os
import secrets
from collections.abc import Callable
from typing import Any

import httpx
import websockets

BASE = os.environ.get("IM_DEMO_TEST_URL", "http://127.0.0.1:18392").rstrip("/")
CONTROL = os.environ.get("IM_TEST_HARNESS", "http://127.0.0.1:18890").rstrip("/")
PASSWORD = "Demo-test-Passw0rd!"


class Browser:
    def __init__(self, username: str) -> None:
        self.username = username
        self.http = httpx.AsyncClient(base_url=BASE, trust_env=False, timeout=20)
        self.ws: Any = None
        self.reader: asyncio.Task[None] | None = None
        self.pending: dict[int, asyncio.Future[Any]] = {}
        self.pushes: list[dict[str, Any]] = []
        self.seq = 0

    async def login(self, password: str = PASSWORD) -> None:
        response = await self.http.post("/api/login", json={"username": self.username, "password": password})
        response.raise_for_status()
        cookie = "; ".join(f"{key}={value}" for key, value in self.http.cookies.items())
        parameters = inspect.signature(websockets.connect).parameters
        options: dict[str, Any] = {"additional_headers" if "additional_headers" in parameters else "extra_headers": {"Cookie": cookie}}
        if "proxy" in parameters:
            options["proxy"] = None
        self.ws = await websockets.connect(BASE.replace("http", "ws", 1) + "/ws", **options)
        self.reader = asyncio.create_task(self._read())
        await self.wait_push("bootstrap")

    async def _read(self) -> None:
        async for raw in self.ws:
            frame = json.loads(raw)
            if frame["type"] == "push":
                self.pushes.append(frame)
            elif frame["type"] == "reply" and frame["id"] in self.pending:
                future = self.pending.pop(frame["id"])
                if frame["ok"]:
                    future.set_result(frame.get("data"))
                else:
                    future.set_exception(RuntimeError(frame.get("error")))

    async def call(self, action: str, **params: Any) -> Any:
        self.seq += 1
        future = asyncio.get_running_loop().create_future()
        self.pending[self.seq] = future
        await self.ws.send(json.dumps({"id": self.seq, "action": action, "params": params}))
        return await asyncio.wait_for(future, timeout=15)

    async def wait_push(self, event: str, predicate: Callable[[Any], bool] = lambda data: True) -> Any:
        async with asyncio.timeout(15):
            while True:
                found = next((frame["data"] for frame in self.pushes if frame["event"] == event and predicate(frame["data"])), None)
                if found is not None:
                    return found
                await asyncio.sleep(0.05)

    async def close(self) -> None:
        if self.ws is not None:
            await self.ws.close()
        if self.reader is not None:
            self.reader.cancel()
            with contextlib.suppress(asyncio.CancelledError):
                await self.reader
        await self.http.aclose()


async def denied(browser: Browser, action: str, **params: Any) -> None:
    try:
        await browser.call(action, **params)
    except RuntimeError:
        return
    raise AssertionError(f"{action} unexpectedly allowed")


async def main() -> None:
    browsers = [Browser(f"mr351_{name}_{secrets.token_hex(4)}") for name in ("a", "b", "c")]
    checks: list[str] = []

    def passed(name: str) -> None:
        checks.append(name)
        print(f"PASS {name}", flush=True)

    async with httpx.AsyncClient(base_url=CONTROL, trust_env=False, timeout=20) as control:
        environment = (await control.get("/env")).json()

        async def admin(method: str, path: str, body: Any = None) -> Any:
            response = await control.post("/openapi", json={"method": method, "path": path, "body": body})
            response.raise_for_status()
            return response.json() if response.content else None

        try:
            for browser in browsers:
                response = await browser.http.post("/api/register", json={"username": browser.username, "password": PASSWORD})
                response.raise_for_status()
                await browser.login()
            a, b, outsider = browsers
            sent = await a.call("send_text", id=f"peer:{b.username}", text="first message")
            cid = sent["conversation_id"]
            await b.wait_push("msgs", lambda data: data["conversation_id"] == cid)
            await denied(outsider, "history", id=cid)
            await denied(outsider, "recall", id=cid, seq=sent["seq"])
            await denied(b, "recall", id=cid, seq=sent["seq"])
            passed("unrelated history and foreign recall rejected")

            group = await a.call("create_group", name="MR351 authorization", members=[b.username])
            gid = group["group_id"]
            await denied(outsider, "rename_group", id=gid, name="forbidden")
            await denied(outsider, "group_members", group_id=gid)
            await denied(b, "rename_group", id=gid, name="forbidden")
            passed("nonmember and ordinary member cannot use group admin privileges")

            for index in range(31):
                sent = await a.call("send_text", id=cid, text=f"history-{index}")
            opened = await b.call("open_conv", id=cid)
            assert len(opened["messages"]) == 30 and opened["has_more"]
            older = await b.call("history", id=cid, before_seq=opened["next_before_seq"])
            assert len(older["messages"]) == 2
            passed("full first page and older-history cursor")

            edited = opened["messages"][-2]
            await admin("PATCH", f"/messages/{edited['message_id']}", {"body": {"text": "edited from OpenAPI"}})
            patch = await b.wait_push(
                "msgs_changed", lambda data: data.get("seq") == edited["seq"] and data.get("body", {}).get("text") == "edited from OpenAPI"
            )
            assert patch["edited"]
            passed("real signed edit callback replaces message body")

            await b.call("delete_msgs", id=cid, seqs=[sent["seq"]])
            new_b = Browser(b.username)
            browsers.append(new_b)
            await new_b.login()
            assert sent["seq"] not in [m["seq"] for m in (await new_b.call("open_conv", id=cid))["messages"]]
            assert sent["seq"] in [m["seq"] for m in (await a.call("open_conv", id=cid))["messages"]]
            passed("per-user deletion survives another device login")
            await new_b.call("clear_conv", id=cid)
            assert not (await new_b.call("open_conv", id=cid))["messages"]
            await b.wait_push("msgs_reset", lambda data: data["conversation_id"] == cid)
            passed("clear empties history and resets all browser caches")

            html = b"<!doctype html><script>window.reviewProof = true;</script>"
            upload = await a.http.post("/api/upload", files={"file": ("review.html", html, "text/html")})
            upload.raise_for_status()
            url = upload.json()["file_info"]["url"]
            download = await a.http.get(
                "/api/media", params={"u": base64.urlsafe_b64encode(url.encode()).decode().rstrip("="), "ct": "text/html"}
            )
            assert download.content == html and download.headers["content-type"] == "application/octet-stream"
            assert download.headers["content-disposition"].startswith("attachment")
            assert "sandbox" in download.headers["content-security-policy"]
            passed("uploaded HTML is downloaded despite forged MIME parameter")

            current = (await a.call("sessions"))["current"]
            await a.call("set_password", old=PASSWORD, new=PASSWORD + "2")
            assert (await a.call("sessions"))["current"] == current
            await a.call("send_text", id=cid, text="still logged in after password change")
            passed("password change preserves device and rotates working tokens")

            outsider_id = (await outsider.call("sessions"))["current"]
            await outsider.call("logout")
            active = await admin("GET", f"/users/{outsider.username}/sessions")
            assert outsider_id not in [item["session_id"] for item in active["items"]]
            passed("logout removes real IM device session")

            await control.post("/console/reauth")
            response = await control.post(
                "/console", json={"method": "POST", "path": f"/apps/{environment['app_name']}/user-sessions/revoke-all"}
            )
            response.raise_for_status()
            fresh = Browser(a.username)
            browsers.append(fresh)
            await fresh.login(PASSWORD + "2")
            await a.wait_push("kicked")
            await fresh.call("sessions")
            passed("global relogin callback kicks old devices and preserves new login")
            print(f"{len(checks)}/{len(checks)} real-service regression groups passed", flush=True)
        finally:
            for browser in browsers:
                with contextlib.suppress(httpx.HTTPError):
                    await browser.http.post("/api/logout")
                await browser.close()


if __name__ == "__main__":
    asyncio.run(main())
