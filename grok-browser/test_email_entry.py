"""Regressions for ignored entry clicks and duplicate-mailbox false positives."""
import asyncio
import json
import sys
import types
import unittest
from pathlib import Path
from types import SimpleNamespace
from unittest.mock import AsyncMock, Mock, patch

sys.path.insert(0, str(Path(__file__).resolve().parent))
sys.modules.setdefault('cloakbrowser', types.ModuleType('cloakbrowser'))
import xai_flow
from network_diagnostics import NetworkDiagnostics


class EntryTests(unittest.IsolatedAsyncioTestCase):
    async def run_entry(self, states, *, success_click=1, click_error=None):
        clock = [0.0]
        form = object()
        count = [0]
        button = SimpleNamespace(evaluate=AsyncMock(side_effect=states))
        async def click(**kwargs):
            self.assertNotIn('force', kwargs)
            self.assertGreater(kwargs['timeout'], 0)
            if click_error:
                raise click_error
            count[0] += 1
        button.click = AsyncMock(side_effect=click)
        async def visible(page, selectors):
            if selectors == xai_flow._EMAIL_INPUTS:
                return form if count[0] >= success_click else None
            return button
        async def pause(page, milliseconds):
            clock[0] += milliseconds / 1000
        page = SimpleNamespace()
        with (
            patch.object(xai_flow.time, 'monotonic', side_effect=lambda: clock[0]),
            patch.object(xai_flow, '_first_visible', side_effect=visible),
            patch.object(xai_flow, '_pause', side_effect=pause),
            patch.object(xai_flow, '_dismiss_entry_consent', new=AsyncMock()),
            patch.object(xai_flow, '_page_diagnostic', new=AsyncMock(side_effect=lambda p: {'email_entry': p._cfm_email_entry})),
        ):
            try:
                result = await xai_flow._wait_for_email_form(page, timeout_s=1)
                return result, page, button
            except xai_flow.XaiBrowserFlowError as exc:
                return exc, page, button

    async def test_waits_for_handler_before_clicking_visible_button(self):
        ready = {'document_state':'interactive', 'handler_ready':True, 'disabled':False}
        result, page, button = await self.run_entry([{**ready, 'handler_ready':False}, ready])
        self.assertNotIsInstance(result, Exception)
        self.assertEqual(button.evaluate.await_count, 2)
        button.click.assert_awaited_once()
        self.assertTrue(page._cfm_email_entry['form_visible'])

    async def test_completed_click_requires_form_and_may_retry_entry(self):
        ready = {'document_state':'interactive', 'handler_ready':True, 'disabled':False}
        result, page, button = await self.run_entry([ready] * 3, success_click=2)
        self.assertNotIsInstance(result, Exception)
        self.assertEqual(button.click.await_count, 2)
        self.assertTrue(page._cfm_email_entry['form_visible'])

    async def test_unbound_or_disabled_entry_times_out_without_click(self):
        for state in ({'handler_ready':False}, {'handler_ready':True, 'disabled':True}):
            result, page, button = await self.run_entry([state] * 10)
            self.assertIsInstance(result, xai_flow.XaiBrowserFlowError)
            self.assertEqual(str(result), 'email_input_missing')
            button.click.assert_not_awaited()
            self.assertFalse(result.diagnostic['email_entry']['form_visible'])
            self.assertLessEqual(len(result.diagnostic['email_entry']['states']), 8)

    async def test_blocked_click_does_not_force_or_leak_error(self):
        ready = {'handler_ready':True, 'disabled':False}
        result, page, button = await self.run_entry([ready] * 10, click_error=TimeoutError('password=SECRET'))
        self.assertIsInstance(result, xai_flow.XaiBrowserFlowError)
        self.assertEqual(page._cfm_email_entry['last_click_error'], 'timeout')
        self.assertEqual(page._cfm_email_entry['clicks'], 0)
        self.assertNotIn('SECRET', json.dumps(result.diagnostic))

    async def test_existing_form_does_not_click_entry(self):
        field = object()
        page = SimpleNamespace()
        with patch.object(xai_flow, '_first_visible', new=AsyncMock(return_value=field)), patch.object(xai_flow, '_dismiss_entry_consent', new=AsyncMock()) as dismiss:
            self.assertIs(await xai_flow._wait_for_email_form(page), field)
        dismiss.assert_not_awaited()

    async def test_timeout_stops_before_email_fill_and_submit(self):
        page=SimpleNamespace()
        session=SimpleNamespace(page=page,lock=asyncio.Lock())
        with (
            patch.object(xai_flow,'_session',new=AsyncMock(return_value=session)),
            patch.object(xai_flow,'_click_any',new=AsyncMock()),
            patch.object(xai_flow,'_first_visible',new=AsyncMock(return_value=None)),
            patch.object(xai_flow,'_wait_for_email_form',new=AsyncMock(side_effect=xai_flow.XaiBrowserFlowError('email_input_missing',stage='send_code',retryable=True))),
            patch.object(xai_flow,'_fill_email_like_user',new=AsyncMock()) as fill,
            patch.object(xai_flow,'_click_submit',new=AsyncMock()) as submit,
        ):
            with self.assertRaises(xai_flow.XaiBrowserFlowError):
                await xai_flow.send_email('test','unused@example.invalid')
        fill.assert_not_awaited()
        submit.assert_not_awaited()


class DuplicateEmailTests(unittest.IsolatedAsyncioTestCase):
    async def test_normal_navigation_never_reports_registered(self):
        for text in ('Create Your Grok Account Sign up with email Already have an account? Sign in',
                     '邮箱注册 已有账号？登录', 'Email already registered'):
            self.assertIsNone(xai_flow._classify_email_stage_error(text))
        page=SimpleNamespace(evaluate=AsyncMock(return_value='Sign up with email Already have an account?'))
        self.assertIsNone(await xai_flow._submitted_email_error(page))

    async def test_explicit_form_feedback_and_upstream_error_still_work(self):
        for text in ('This email address is already registered', 'Email already exists', '邮箱已注册'):
            page=SimpleNamespace(evaluate=AsyncMock(return_value=text))
            self.assertEqual(await xai_flow._submitted_email_error(page), 'email_already_registered')
        self.assertEqual(xai_flow._upstream_email_error('account:email-already-exists'), ('email_already_registered','email_already_exists'))

    async def test_structured_upstream_error_takes_priority_over_page_copy(self):
        page=SimpleNamespace(url='https://accounts.x.ai/sign-up',title=AsyncMock(return_value='Create Your Grok Account'))
        with patch.object(xai_flow,'_visible_control_summary',new=AsyncMock(return_value={})):
            result=await xai_flow._page_diagnostic(page,body='Try again later',upstream={'reason':'email_already_registered','code':'email_already_exists','status':400})
        self.assertEqual(result['reason'],'email_already_registered')


class LoadingTests(unittest.TestCase):
    def test_pending_first_party_scripts_settle_without_leaking_urls(self):
        tracker=NetworkDiagnostics()
        pending=SimpleNamespace(url='https://accounts.x.ai/private?token=SECRET',resource_type='script')
        failed=SimpleNamespace(url='https://auth.x.ai/private',resource_type='script',failure='net::ERR_TIMED_OUT')
        other=SimpleNamespace(url='https://stats.invalid/private',resource_type='script')
        fetch=SimpleNamespace(url='https://accounts.x.ai/private',resource_type='fetch')
        for req in (pending,failed,other,fetch):tracker.started(req)
        self.assertEqual(tracker.loading_snapshot()['pending_scripts'],2)
        tracker.finished(pending)
        tracker.failed(failed)
        result=tracker.loading_snapshot()
        self.assertEqual(result['pending_scripts'],0)
        self.assertEqual(result['completed_scripts'],1)
        self.assertEqual(result['failed_scripts'],1)
        self.assertNotIn('SECRET',json.dumps(result))
        self.assertNotIn('private',json.dumps(result))


if __name__ == '__main__':
    unittest.main()
