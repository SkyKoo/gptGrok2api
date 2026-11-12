import asyncio
import unittest
from unittest.mock import patch, AsyncMock
from browser_sessions import BrowserQueue, BrowserQueueError, BrowserStartupError, BrowserStartupCleanupPending
from adobe_flow import AdobeCommandRequest, AdobeProcess, adobe_command, launch_adobe

class AdobeWorkerTests(unittest.IsolatedAsyncioTestCase):
    async def test_resume_action_uses_the_same_claimed_profile_process(self):
        from types import SimpleNamespace
        handle = AdobeProcess(None)
        handle.command = AsyncMock(return_value={"stage":"awaiting_verification", "submission_started":True,
            "verification_requested_at":"2026-11-12T00:00:00Z"})
        entry = SimpleNamespace(browser=handle, closing=False, state="active", touched=0)
        with patch("adobe_flow.queue", SimpleNamespace(entries={"a"*32:entry})):
            result = await adobe_command(AdobeCommandRequest(browser_lease="a"*32, action="resume"))
        handle.command.assert_awaited_once_with({"action":"resume", "code":""})
        self.assertTrue(result["submission_started"])
        self.assertEqual(result["verification_requested_at"], "2026-11-12T00:00:00Z")

    async def test_fixed_rpc_error_preserves_authoritative_submission_marker(self):
        import json
        for marker in (True, False, "PRIVATE"):
            class Process:
                returncode = None
                def __init__(self):
                    self.stdin = self
                    self.stdout = self
                def write(self, raw): self.request = json.loads(raw)
                async def drain(self): pass
                async def readline(self):
                    return json.dumps({"id":self.request["id"], "ok":False,
                        "code":"recovery_login_result_unknown", "submission_started":marker}).encode()+b"\n"
            result = await AdobeProcess(Process()).command({"action":"resume"})
            self.assertEqual(result["code"], "recovery_login_result_unknown")
            if isinstance(marker, bool): self.assertIs(result["submission_started"], marker)
            else: self.assertNotIn("submission_started", result)

    async def test_init_failure_preserves_safe_code_and_closes_before_raising(self):
        for code in BrowserStartupError.CODES:
            handle = AsyncMock()
            handle.command.return_value = {'stage': 'needs_manual', 'code': code}
            with patch('adobe_flow.asyncio.create_subprocess_exec', AsyncMock()), patch('adobe_flow.AdobeProcess', return_value=handle):
                with self.assertRaises(BrowserStartupError) as raised:
                    await launch_adobe(proxy='PRIVATE_PROXY')
            self.assertEqual(raised.exception.code, code)
            handle.close.assert_awaited_once()

    async def test_init_failure_drops_untrusted_error_and_diagnostics(self):
        for code in ('PRIVATE_PROXY', 'browser_launch_timeout\nPRIVATE', None, ['PRIVATE']):
            handle = AsyncMock()
            handle.command.return_value = {'stage': 'needs_manual', 'code': code, 'diagnostics': {'private': 'PRIVATE'}}
            with patch('adobe_flow.asyncio.create_subprocess_exec', AsyncMock()), patch('adobe_flow.AdobeProcess', return_value=handle):
                with self.assertRaises(BrowserStartupError) as raised:
                    await launch_adobe()
            self.assertEqual(str(raised.exception), 'browser_launch_failed')
            handle.close.assert_awaited_once()

    async def test_init_close_failure_transfers_handle_with_fixed_diagnostic(self):
        handle = AsyncMock()
        handle.command.return_value = {'stage': 'needs_manual', 'code': 'browser_launch_timeout'}
        handle.close.side_effect = RuntimeError('adobe_browser_close_unconfirmed')
        with patch('adobe_flow.asyncio.create_subprocess_exec', AsyncMock()), patch('adobe_flow.AdobeProcess', return_value=handle):
            with self.assertRaises(BrowserStartupCleanupPending) as raised:
                await launch_adobe()
        self.assertIs(raised.exception.browser, handle)
        self.assertEqual(raised.exception.code, 'browser_launch_timeout')
        self.assertEqual(str(raised.exception), 'browser_close_pending')

    async def test_failed_init_keeps_queue_slot_until_original_handle_closes(self):
        q = BrowserQueue(close_timeout=.01)
        handle = AsyncMock()
        handle.command.return_value = {'stage': 'needs_manual', 'code': 'browser_launch_timeout'}
        handle.close.side_effect = RuntimeError('PRIVATE_CLOSE_FAILURE')
        next_browser = AsyncMock()
        next_launch = AsyncMock(return_value=next_browser)
        try:
            with patch('adobe_flow.asyncio.create_subprocess_exec', AsyncMock()) as spawn, \
                 patch('adobe_flow.AdobeProcess', return_value=handle):
                q.enqueue('adobe', {'proxy':'PRIVATE_PROXY'}, launch_adobe)
                q.enqueue('grok', {}, next_launch)
                for _ in range(50):
                    if q.entries['adobe'].closing and q.entries['adobe'].close_task is not None and q.entries['adobe'].close_task.done(): break
                    await asyncio.sleep(.01)
                self.assertEqual(q.status('adobe')['status'], 'closing')
                self.assertEqual(q.status('adobe')['diagnostic_code'], 'browser_launch_timeout')
                self.assertIs(q.entries['adobe'].browser, handle)
                self.assertEqual(q.available, 0)
                self.assertEqual(q.status('grok')['status'], 'waiting_browser')
                next_launch.assert_not_awaited()
                spawn.assert_awaited_once()
                handle.command.assert_awaited_once()
                self.assertEqual(q.entries['adobe'].options, {})
                self.assertNotIn('PRIVATE', str(q.status('adobe')))
                handle.close.side_effect = None
                q.entries['adobe'].retry_at = 0
                await q.close('adobe')
                for _ in range(50):
                    if q.status('grok')['status'] == 'ready': break
                    await asyncio.sleep(.01)
                self.assertNotIn('adobe', q.entries)
                next_launch.assert_awaited_once()
                self.assertEqual(q.status('grok')['status'], 'ready')
                handle.command.assert_awaited_once()
        finally:
            handle.close.side_effect = None
            for entry in q.entries.values(): entry.retry_at = 0
            await q.shutdown()

    async def test_shared_queue_retains_capacity_until_adobe_close_finishes(self):
        q = BrowserQueue(close_timeout=.01)
        gate = asyncio.Event()
        class Handle:
            async def close(self): await gate.wait()
        async def launcher(): return Handle()
        q.enqueue('adobe', {}, launcher)
        for _ in range(50):
            if q.status('adobe')['status']=='ready': break
            await asyncio.sleep(.01)
        q.claim('adobe')
        self.assertEqual(q.available,0)
        with self.assertRaises(BrowserQueueError): await q.close('adobe')
        self.assertEqual(q.available,0)
        gate.set(); await asyncio.sleep(.01)
        self.assertEqual(q.available,1)
        await q.shutdown()

    async def test_adobe_command_rejects_grok_handle(self):
        q=BrowserQueue()
        async def launcher(): return object()
        q.enqueue('a'*32,{},launcher)
        for _ in range(50):
            if q.status('a'*32)['status']=='ready': break
            await asyncio.sleep(.01)
        with patch('adobe_flow.queue',q):
            with self.assertRaises(BrowserQueueError):
                await adobe_command(AdobeCommandRequest(browser_lease='a'*32))
        q.entries.clear(); await q.shutdown()

    async def test_lost_response_taints_process_and_blocks_next_command(self):
        class Input:
            def __init__(self): self.writes=0
            def write(self,raw): self.writes+=1
            async def drain(self): pass
        class Output:
            async def readline(self): return b'broken\n'
        class Process:
            returncode=None
            stdin=Input()
            stdout=Output()
        process=Process();handle=AdobeProcess(process)
        with self.assertRaises(Exception): await handle.command({'action':'run'})
        with self.assertRaises(RuntimeError): await handle.command({'action':'run'})
        self.assertEqual(process.stdin.writes,1)

    async def test_adobe_http_reservation_blocks_grok_until_closed(self):
        import httpx
        import server
        import adobe_flow
        from types import SimpleNamespace
        from unittest.mock import AsyncMock
        q=BrowserQueue(close_timeout=.1)
        commands=[]
        class Handle(AdobeProcess):
            def __init__(self): super().__init__(None)
            async def command(self,payload):
                commands.append(payload['action'])
                return {'stage':'awaiting_verification','code':'verification_required'}
            async def close(self): commands.append('close')
        handle=Handle()
        grok=SimpleNamespace(close=AsyncMock())
        with patch.object(server,'queue',q),patch.object(adobe_flow,'queue',q),patch.object(server,'_auth_token','secret'),patch.object(server,'launch_adobe',AsyncMock(return_value=handle)),patch.object(server.cloakbrowser,'launch_async',AsyncMock(return_value=grok),create=True):
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=server.app),base_url='http://worker',headers={'Authorization':'Bearer secret'}) as client:
                a='a'*32;g='g'*32
                response=await client.post('/v1/browser/reservations',json={'id':a,'provider':'adobe','profile_id':'fixture','email':'fixture@example.test'})
                self.assertEqual(response.status_code,200)
                for _ in range(50):
                    if q.status(a)['status']=='ready':break
                    await asyncio.sleep(.01)
                response=await client.post('/v1/adobe/run',json={'browser_lease':a})
                self.assertEqual(response.json()['stage'],'awaiting_verification')
                await client.post('/v1/browser/reservations',json={'id':g,'provider':'grok'})
                self.assertEqual(q.status(g)['status'],'waiting_browser')
                self.assertEqual(commands,['run'])
                await client.delete('/v1/browser/reservations/'+a)
                for _ in range(50):
                    if q.status(g)['status']=='ready':break
                    await asyncio.sleep(.01)
                self.assertEqual(q.status(g)['status'],'ready')
                self.assertEqual(commands,['run','close'])
                self.assertEqual((await client.get('/health')).json()['service'],'cfm-browser-worker')
                await client.delete('/v1/browser/reservations/'+g)
        await q.shutdown()

    async def test_resume_reservation_requires_recovery_identity_and_preserves_original_profile_options(self):
        import httpx
        import server
        import adobe_flow
        q = BrowserQueue(close_timeout=.1)
        handle = AsyncMock(spec=AdobeProcess)
        launcher = AsyncMock(return_value=handle)
        profile = {"firstName":"Fixture", "lastName":"Person", "country":"US"}
        base = {"id":"r"*32, "provider":"adobe", "profile_id":"original-profile",
            "email":"fixture@example.test", "adobe_mode":"resume", "adobe_profile":profile,
            "proxy":"socks5://fixture:password@127.0.0.1:1080"}
        with patch.object(server, "queue", q), patch.object(adobe_flow, "queue", q), \
             patch.object(server, "_auth_token", "secret"), patch.object(server, "launch_adobe", launcher):
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=server.app),base_url="http://worker",
                headers={"Authorization":"Bearer secret"}) as client:
                for changes in ({}, {"adobe_recovery_id":"../escape"}, {"adobe_recovery_id":"x"*81},
                    {"adobe_recovery_id":"child", "profile_id":"../escape"},
                    {"adobe_recovery_id":"child", "adobe_require_new":True},
                    {"adobe_mode":"refresh", "adobe_require_new":True}):
                    response = await client.post("/v1/browser/reservations", json={**base, **changes})
                    self.assertEqual(response.status_code,422)
                launcher.assert_not_awaited()
                response = await client.post("/v1/browser/reservations", json={**base, "adobe_recovery_id":"child-1"})
                self.assertEqual(response.status_code,200)
                for _ in range(50):
                    if q.status(base["id"])["status"] == "ready": break
                    await asyncio.sleep(.01)
                launcher.assert_awaited_once_with(profile_id="original-profile", email="fixture@example.test",
                    profile=profile, proxy=base["proxy"], mode="resume", require_new=False, recovery_id="child-1", account_id="", password="")
                await client.delete("/v1/browser/reservations/"+base["id"])
        await q.shutdown()

    async def test_new_registration_forwards_explicit_new_profile_requirement(self):
        import httpx
        import server
        q = BrowserQueue(close_timeout=.1)
        launcher = AsyncMock(return_value=AsyncMock(spec=AdobeProcess))
        with patch.object(server, 'queue', q), patch.object(server, '_auth_token', 'secret'), \
             patch.object(server, 'launch_adobe', launcher):
            async with httpx.AsyncClient(transport=httpx.ASGITransport(app=server.app),base_url='http://worker',
                headers={'Authorization':'Bearer secret'}) as client:
                for index, require_new in enumerate((True, False)):
                    job_id = str(index) * 32
                    response = await client.post('/v1/browser/reservations', json={'id':job_id,
                        'provider':'adobe', 'profile_id':'fixture-profile', 'email':'fixture@example.test',
                        'adobe_mode':'registration', 'adobe_require_new':require_new})
                    self.assertEqual(response.status_code, 200)
                    for _ in range(50):
                        if q.status(job_id)['status'] == 'ready': break
                        await asyncio.sleep(.01)
                    self.assertEqual(launcher.await_args.kwargs['require_new'], require_new)
                    self.assertEqual(launcher.await_args.kwargs['mode'], 'registration')
                    await client.delete('/v1/browser/reservations/'+job_id)
        await q.shutdown()
