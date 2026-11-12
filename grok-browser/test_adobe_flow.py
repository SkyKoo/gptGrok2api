import asyncio
import unittest
from unittest.mock import patch, AsyncMock
from browser_sessions import BrowserQueue, BrowserQueueError, BrowserStartupError
from adobe_flow import AdobeCommandRequest, AdobeProcess, adobe_command, launch_adobe

class AdobeWorkerTests(unittest.IsolatedAsyncioTestCase):
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

    async def test_init_close_failure_remains_failure_and_is_not_hidden_by_diagnostic(self):
        handle = AsyncMock()
        handle.command.return_value = {'stage': 'needs_manual', 'code': 'browser_launch_timeout'}
        handle.close.side_effect = RuntimeError('adobe_browser_close_unconfirmed')
        with patch('adobe_flow.asyncio.create_subprocess_exec', AsyncMock()), patch('adobe_flow.AdobeProcess', return_value=handle):
            with self.assertRaisesRegex(RuntimeError, '^adobe_browser_close_unconfirmed$'):
                await launch_adobe()

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
