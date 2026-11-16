"""Exercise the real Worker HTTP routes using an in-memory browser only."""
import asyncio
import importlib.util
import logging
import sys
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock, patch

HTTP_TESTS = bool(importlib.util.find_spec('fastapi') and importlib.util.find_spec('httpx'))
if HTTP_TESTS:
    import httpx
    sys.modules.setdefault('cloakbrowser', SimpleNamespace(launch_async=AsyncMock()))
    import server
    import browser_sessions


@unittest.skipUnless(HTTP_TESTS, 'requires fastapi and httpx (no real browser)')
class WorkerQueueRoutes(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.queue = browser_sessions.BrowserQueue(close_timeout=.05)
        self.launch = AsyncMock(side_effect=lambda **_: SimpleNamespace(close=AsyncMock()))
        self.patches = [patch.object(server,'queue',self.queue), patch.object(browser_sessions,'queue',self.queue),
                        patch.object(server,'_auth_token','worker-secret'), patch.object(server.cloakbrowser,'launch_async',self.launch,create=True)]
        for p in self.patches:p.start()
        self.client=httpx.AsyncClient(transport=httpx.ASGITransport(app=server.app),base_url='http://worker',headers={'Authorization':'Bearer worker-secret'})

    async def asyncTearDown(self):
        await self.client.aclose()
        await self.queue.shutdown()
        for p in reversed(self.patches):p.stop()

    async def ready(self,key):
        async with asyncio.timeout(2):
            while (await self.client.get('/v1/browser/reservations/'+key)).json()['status']!='ready':
                await asyncio.sleep(.01)

    async def test_signup_and_oauth_share_one_prepared_browser_queue(self):
        registration='a'*48;oauth='b'*48;active={}
        async def signup(**kwargs):
            active['signup']=kwargs['browser']
            return {'ok':True,'session_id':'native-test-session'}
        async def close(_):await active['signup'].close()
        async def consent(**kwargs):
            await kwargs['browser'].close()
            return {'status':'approved','code':''}
        with patch.object(server,'xai_start_session',signup),patch.object(server,'close_xai_session',close),patch.object(server,'authorize_device',consent):
            denied=await self.client.post('/v1/browser/reservations',headers={'Authorization':'bad'},json={'id':registration})
            self.assertEqual(denied.status_code,401)
            response=await self.client.post('/v1/browser/reservations',json={'id':registration,'proxy':'http://private:secret@example.test:8080'})
            self.assertNotIn('secret',response.text)
            await self.ready(registration)
            started=await self.client.post('/v1/session/start',json={'browser_lease':registration,'url':'https://accounts.x.ai/sign-up'})
            self.assertEqual(started.status_code,200)
            duplicate=await self.client.post('/v1/session/start',json={'browser_lease':registration,'url':'https://accounts.x.ai/sign-up'})
            self.assertEqual(duplicate.status_code,409)
            await self.client.post('/v1/browser/reservations',json={'id':oauth,'daily_direct':True})
            await asyncio.sleep(.25)
            status=(await self.client.get('/v1/browser/reservations/'+oauth)).json()
            self.assertEqual(status['status'],'waiting_browser')
            self.assertEqual(status['message'],'等待浏览器会话释放')
            self.assertEqual(self.launch.await_count,1)
            await self.client.post('/v1/session/close',json={'session_id':'native-test-session'})
            await self.ready(oauth)
            response=await self.client.post('/v1/oauth/authorize-device',json={'browser_lease':oauth,'verification_uri_complete':'https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH','user_code':'ABCD-EFGH','sso':'sensitive','expected_email':'selected@example.test'})
            self.assertEqual(response.json()['status'],'approved')
            self.assertEqual(self.launch.await_count,2)
            self.assertNotIn('proxy',self.launch.call_args.kwargs)
            self.assertEqual((await self.client.get('/health')).json()['capacity'],1)

    async def test_grok_reservation_respects_launch_signature_and_releases_slot(self):
        started = asyncio.Event()
        proceed = asyncio.Event()
        browser = SimpleNamespace(close=AsyncMock())
        calls = []

        # The real SDK forwards unknown keywords to Playwright. A permissive
        # **kwargs mock hid the startup_progress callback leaking into launch.
        async def strict_launch(*, humanize, headless, locale, args=None,
                                timezone=None, proxy=None, geoip=False):
            calls.append({'locale': locale, 'proxy': proxy})
            started.set()
            await proceed.wait()
            return browser

        async def consent(**kwargs):
            self.assertEqual(kwargs['browser'].entry.state, 'active')
            await kwargs['browser'].close()
            return {'status': 'approved', 'code': ''}

        key = 'f' * 48
        with patch.object(server.cloakbrowser, 'launch_async', strict_launch), \
                patch.object(server, 'authorize_device', consent):
            response = await self.client.post('/v1/browser/reservations',
                                              json={'id': key, 'daily_direct': True})
            self.assertEqual(response.status_code, 200)
            try:
                await asyncio.wait_for(started.wait(), timeout=1)
                status = (await self.client.get('/v1/browser/reservations/' + key)).json()
                self.assertEqual(status['status'], 'starting_browser')
                self.assertEqual((await self.client.get('/health')).json()['capacity'], 0)
            finally:
                proceed.set()
            await self.ready(key)
            self.assertEqual(calls, [{'locale': 'en-US', 'proxy': None}])
            response = await self.client.post('/v1/oauth/authorize-device', json={
                'browser_lease': key,
                'verification_uri_complete': 'https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH',
                'user_code': 'ABCD-EFGH', 'sso': 'fixture-sso',
                'expected_email': 'selected@example.test',
            })
            self.assertEqual(response.json()['status'], 'approved')
            browser.close.assert_awaited_once()
            self.assertEqual((await self.client.get('/health')).json()['capacity'], 1)
            self.assertEqual((await self.client.get('/v1/browser/reservations/' + key)).status_code, 404)
            # A later caller release is idempotent; it cannot consume another slot.
            self.assertEqual((await self.client.delete('/v1/browser/reservations/' + key)).json(),
                             {'status': 'released'})
            browser.close.assert_awaited_once()

    async def test_cancel_during_launch_reports_closing_and_waits_for_real_close(self):
        gate=asyncio.Event();browser=SimpleNamespace(close=AsyncMock())
        async def delayed(**_):await gate.wait();return browser
        self.launch.side_effect=delayed
        key='c'*48
        await self.client.post('/v1/browser/reservations',json={'id':key})
        await asyncio.sleep(.02)
        result=await self.client.delete('/v1/browser/reservations/'+key)
        self.assertEqual(result.json()['status'],'closing')
        self.assertEqual((await self.client.get('/health')).json()['capacity'],0)
        gate.set()
        await asyncio.sleep(.25)
        browser.close.assert_awaited_once()
        self.assertEqual((await self.client.get('/health')).json()['capacity'],1)

    async def test_reservation_normalizes_legacy_resin_url_before_browser_launch(self):
        key='d'*48
        raw='http://CFM-Grok.task-test:p%40ss%3A%2F%3F%23%25@resin:2260/'
        response=await self.client.post('/v1/browser/reservations',json={'id':key,'proxy':raw})
        self.assertEqual(response.status_code,200)
        await self.ready(key)
        self.assertEqual(self.launch.call_args.kwargs['proxy'],raw[:-1])
        await self.client.delete('/v1/browser/reservations/'+key)

    async def test_invalid_proxy_rejected_before_queue_admission_without_credentials(self):
        response=await self.client.post('/v1/browser/reservations',json={'id':'e'*48,'proxy':'http://user:secret@resin:2260/api'})
        self.assertEqual(response.status_code,422)
        self.assertEqual(response.json()['detail'],{'stage':'browser_start','error':'proxy_configuration_invalid'})
        self.assertNotIn('secret',response.text)
        self.assertFalse(self.queue.entries)
        self.launch.assert_not_awaited()

    async def test_browser_proxy_rejection_has_explicit_safe_error_and_no_retry(self):
        errors=[
            RuntimeError('Page.goto: net::ERR_NO_SUPPORTED_PROXIES at http://user:secret@resin:2260/'),
            server.InvalidBrowserProxy(),
        ]
        for exc in errors:
            with self.subTest(error=type(exc).__name__), patch.object(server,'xai_start_session',AsyncMock(side_effect=exc)) as start:
                response=await self.client.post('/v1/session/start',json={'url':'https://accounts.x.ai/sign-up'})
                self.assertEqual(response.status_code,422)
                self.assertEqual(response.json()['detail'],{'stage':'browser_start','error':'proxy_configuration_invalid'})
                self.assertNotIn('secret',response.text)
                start.assert_awaited_once()

    async def test_other_browser_errors_keep_their_existing_classification(self):
        with patch.object(server,'xai_start_session',AsyncMock(side_effect=RuntimeError('net::ERR_TUNNEL_CONNECTION_FAILED'))):
            response=await self.client.post('/v1/session/start',json={'url':'https://accounts.x.ai/sign-up'})
            self.assertEqual(response.status_code,502)
            self.assertEqual(response.json()['detail']['error'],'browser start failed')

    async def test_daily_direct_ignores_an_invalid_environment_proxy(self):
        with patch.dict('os.environ',{'TURNSTILE_PROXY':'http://user:secret@resin:2260/api'}):
            key='f'*48
            response=await self.client.post('/v1/browser/reservations',json={'id':key,'daily_direct':True})
            self.assertEqual(response.status_code,200)
            await self.ready(key)
            self.assertNotIn('proxy',self.launch.call_args.kwargs)
            await self.client.delete('/v1/browser/reservations/'+key)

    async def test_oauth_diagnostics_are_logged_without_sensitive_fields(self):
        value = {
            'status': 'uncertain',
            'code': 'oauth_callback_timeout',
            'stage': 'consent_callback',
            'detail': 'callback_wait_timeout',
            'elapsed_ms': 1200,
            'consent_submitted': True,
            'consent_state': 'attempted',
            'operation': 'wait_callback',
            'exception_type': 'navigation',
            'page_path': '/oauth2/device/consent',
            'network_failures': 2,
            'network_error_codes': 'ERR_TUNNEL_CONNECTION_FAILED,TIMED_OUT',
        }
        with patch.object(server, 'authorize_device', AsyncMock(return_value=value)):
            with self.assertLogs(server.log, level=logging.INFO) as records:
                response = await self.client.post('/v1/oauth/authorize-device', json={
                    'task_id': 'task_20261116090339.000000000',
                    'verification_uri_complete': 'https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH',
                    'user_code': 'ABCD-EFGH',
                    'sso': 'SSO-SECRET',
                    'expected_email': 'selected@example.test',
                })
        self.assertEqual(response.status_code, 200)
        self.assertIn('stage=consent_callback', records.output[0])
        self.assertIn('code=oauth_callback_timeout', records.output[0])
        self.assertIn('consent_submitted=True', records.output[0])
        self.assertIn('task=task_20261116090339.000000000', records.output[0])
        self.assertIn('consent_state=attempted', records.output[0])
        self.assertIn('operation=wait_callback', records.output[0])
        self.assertIn('exception_type=navigation', records.output[0])
        self.assertIn('network_failures=2', records.output[0])
        self.assertIn('network_error_codes=ERR_TUNNEL_CONNECTION_FAILED,TIMED_OUT', records.output[0])
        for secret in ('SSO-SECRET', 'selected@example.test', 'ABCD-EFGH'):
            self.assertNotIn(secret, records.output[0])

    async def test_oauth_log_boundary_revalidates_worker_fields(self):
        secret = 'SSO-SECRET selected@example.test ABCD-EFGH'
        value = {key:secret for key in ('status','code','stage','detail','operation','exception_type','page_path','consent_state','network_error_codes')}
        value['consent_submitted'] = True
        with self.assertLogs(server.log, level=logging.INFO) as records:
            result = server._oauth_device_result(secret, value)
        self.assertEqual(result['consent_state'], 'attempted')
        self.assertEqual(result['operation'], 'unknown')
        self.assertEqual(result['page_path'], '<other>')
        for text in ('SSO-SECRET','selected@example.test','ABCD-EFGH'):
            self.assertNotIn(text, str(result))
            self.assertNotIn(text, '\n'.join(records.output))

    async def test_oauth_task_handle_is_validated_before_authorization(self):
        authorize = AsyncMock()
        with patch.object(server, 'authorize_device', authorize):
            response = await self.client.post('/v1/oauth/authorize-device', json={
                'task_id':'task_bad\nINJECTED',
                'verification_uri_complete':'https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH',
                'user_code':'ABCD-EFGH','sso':'SSO-SECRET','expected_email':'selected@example.test'})
        self.assertEqual(response.status_code, 422)
        authorize.assert_not_awaited()

    async def test_handler_deadline_preserves_shared_consent_state(self):
        original_timeout = asyncio.timeout
        for consent_state in ('not_attempted','attempted','unknown'):
            with self.subTest(consent_state=consent_state):
                async def suspended(*, diagnostics, **kwargs):
                    diagnostics.consent_state = consent_state
                    diagnostics.operation = 'consent_click' if consent_state == 'attempted' else 'button_count'
                    await asyncio.Event().wait()
                with patch.object(server, 'authorize_device', suspended), \
                        patch.object(server.asyncio, 'timeout', lambda _: original_timeout(.01)):
                    with self.assertLogs(server.log, level=logging.INFO) as records:
                        response = await self.client.post('/v1/oauth/authorize-device', json={
                            'task_id':'task_deadline',
                            'verification_uri_complete':'https://accounts.x.ai/oauth2/device?user_code=ABCD-EFGH',
                            'user_code':'ABCD-EFGH','sso':'SSO-SECRET','expected_email':'selected@example.test'})
                value = response.json()
                self.assertEqual(value['status'], 'failed' if consent_state == 'not_attempted' else 'uncertain')
                self.assertEqual(value['consent_state'], consent_state)
                self.assertEqual(value['consent_submitted'], consent_state == 'attempted')
                self.assertEqual(value['exception_type'], 'timeout')
                self.assertEqual(value['code'], 'oauth_authorization_timeout')
                self.assertIn('task=task_deadline', records.output[0])
                self.assertNotIn('SSO-SECRET', records.output[0])


if __name__=='__main__':unittest.main()
