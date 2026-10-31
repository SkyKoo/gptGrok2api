"""Regression checks for reading the native page's own Turnstile state."""
import asyncio
import json
import shutil
import subprocess
import sys
import time
import types
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.modules.setdefault("cloakbrowser", types.ModuleType("cloakbrowser"))
import xai_flow


@unittest.skipUnless(shutil.which("node"), "Node.js is required to execute page JavaScript")
class TurnstileJavaScriptTests(unittest.TestCase):
    def run_js(self, setup, assertions, script=xai_flow._TURNSTILE_RESPONSE_JS):
        source = "const assert = require('node:assert/strict');\n" + setup
        source += "\nconst probe = " + script + ";\n" + assertions
        result = subprocess.run(["node", "-e", source], capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_default_widget_uses_no_argument_and_stale_id_falls_back(self):
        for widget_id in ("", "removed-widget"):
            for script in (xai_flow._TURNSTILE_RESPONSE_JS, xai_flow._TURNSTILE_STATE_JS):
                with self.subTest(widget_id=widget_id, state=script == xai_flow._TURNSTILE_STATE_JS):
                    self.run_js(
                        "global.document = {querySelectorAll: () => []}; const calls = [];"
                        "global.window = {__cfmGrokTurnstileWidgetId: " + json.dumps(widget_id) + ","
                        "turnstile: {render() {}, getResponse(...args) {calls.push(args);"
                        "if (args.length) throw Error('unknown widget'); return 'fixture-response';}}};",
                        "const value = probe(); assert.equal(typeof value === 'object' ? value.token_present : Boolean(value), true);"
                        "assert.equal(calls.at(-1).length, 0);"
                        + ("assert.equal(calls.length, 1);" if not widget_id else "assert.equal(calls.length, 2);"),
                        script,
                    )

    def test_response_input_in_open_shadow_root_is_read(self):
        self.run_js(
            "const input = {value: 'fixture-response'};"
            "const shadow = {querySelectorAll: s => s === '*' ? [] : [input]};"
            "global.document = {querySelectorAll: s => s === '*' ? [{shadowRoot: shadow}] : []};"
            "global.window = {};",
            "assert.equal(probe(), 'fixture-response');",
        )

    def test_no_widget_or_response_does_not_claim_success(self):
        self.run_js(
            "global.document = {querySelectorAll: () => []}; global.window = {};",
            "assert.equal(probe().token_present, false);",
            xai_flow._TURNSTILE_STATE_JS,
        )

    def test_late_render_is_captured_and_numeric_zero_id_is_preserved(self):
        self.run_js(
            "global.window = {}; let install; let cutoff = false;"
            "global.setInterval = callback => {install = callback; return 1;};"
            "global.setTimeout = () => {cutoff = true;};",
            "probe(); assert.equal(cutoff, false);"
            "let originalCalled = false; window.turnstile = {render(container, options) {"
            "options.callback('fixture-response'); return 0;}};"
            "install(); const id = window.turnstile.render({}, {callback() {originalCalled = true;}});"
            "assert.equal(id, 0); assert.equal(window.__cfmGrokTurnstileWidgetId, '0');"
            "assert.equal(window.__cfmGrokTurnstileCallbackSeen, true); assert.equal(originalCalled, true);",
            "() => {" + xai_flow._TURNSTILE_CAPTURE_INIT_JS + "}",
        )


class TurnstileFrameAndDeadlineTests(unittest.IsolatedAsyncioTestCase):
    async def test_frame_tree_covers_closed_shadow_roots_and_rejects_lookalikes(self):
        element = SimpleNamespace(is_visible=AsyncMock(return_value=True))
        frames = [SimpleNamespace(url=url, frame_element=AsyncMock(return_value=element)) for url in (
            "https://challenges.cloudflare.com/widget",
            "https://brunhild.challenges.cloudflare.com/widget",
            "https://challenges.cloudflare.com.attacker.invalid/widget",
        )]
        page = SimpleNamespace(evaluate=AsyncMock(return_value={"iframe_count": 0}), frames=frames)
        state = await xai_flow._turnstile_state(page)
        self.assertEqual(state["iframe_count"], 2)
        self.assertEqual(state["visible_iframe_count"], 2)
        frames[-1].frame_element.assert_not_awaited()

    async def test_slow_checkbox_probe_respects_remaining_wait_budget(self):
        async def slow_click(*args, **kwargs):
            await asyncio.sleep(10)
        page = SimpleNamespace(evaluate=AsyncMock(return_value={}))
        started = time.monotonic()
        with patch.object(xai_flow, "_click_turnstile_checkbox", side_effect=slow_click), \
             patch.object(xai_flow, "_page_diagnostic", new=AsyncMock(return_value={})):
            with self.assertRaises(xai_flow.XaiBrowserFlowError):
                await xai_flow._native_turnstile_token(page, timeout_s=0.05)
        self.assertLess(time.monotonic() - started, 0.5)


if __name__ == "__main__":
    unittest.main()
