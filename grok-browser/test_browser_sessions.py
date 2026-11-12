import asyncio
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock
from browser_sessions import BrowserQueue, BrowserQueueError, BrowserStartupError, BrowserStartupCleanupPending, startup_progress_event


class LifecycleTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self):
        self.queue = BrowserQueue(1, ready_ttl=2, close_timeout=.03, license_cooldown=.05)

    async def asyncTearDown(self):
        for e in self.queue.entries.values():
            e.retry_at = 0
            if e.close_task and not e.close_task.done():
                e.close_task.cancel()
        await self.queue.shutdown()

    async def ready(self, key):
        async with asyncio.timeout(2):
            while self.queue.status(key)['status'] != 'ready':
                await asyncio.sleep(.01)

    def browser(self):
        return SimpleNamespace(close=AsyncMock())

    async def test_opted_in_progress_is_live_bounded_and_detached_from_status(self):
        gate = asyncio.Event()
        async def launch(*, startup_progress):
            for sequence in range(1, 40):
                startup_progress({"sequence":sequence, "stage":"adobe_loading_account_page",
                    "status":"started", "elapsed_ms":10, "duration_ms":0, "private":"PRIVATE"})
            await gate.wait()
            return self.browser()
        self.queue.enqueue('adobe', {}, launch, startup_progress=True)
        for _ in range(50):
            if self.queue.status('adobe').get('startup_events'): break
            await asyncio.sleep(.01)
        status = self.queue.status('adobe')
        self.assertEqual(status['status'], 'starting_browser')
        self.assertEqual(len(status['startup_events']), 32)
        self.assertNotIn('PRIVATE', str(status))
        status['startup_events'][0]['stage'] = 'PRIVATE'
        status['startup_events'].clear()
        self.assertEqual(len(self.queue.status('adobe')['startup_events']), 32)
        self.assertNotIn('PRIVATE', str(self.queue.status('adobe')))
        gate.set()
        await self.ready('adobe')
        self.assertEqual(len(self.queue.status('adobe')['startup_events']), 32)

    async def test_progress_failures_survive_without_changing_retry_policy(self):
        async def launch(*, startup_progress):
            base = {"stage":"starting_browser", "elapsed_ms":10, "duration_ms":10}
            startup_progress({**base, "sequence":1, "status":"started"})
            startup_progress({**base, "sequence":1, "status":"started"})
            startup_progress({**base, "sequence":2, "status":"failed", "exception_type":"timeout"})
            raise BrowserStartupError('browser_launch_timeout')
        launcher = AsyncMock(side_effect=launch)
        self.queue.enqueue('adobe', {}, launcher, startup_progress=True)
        await asyncio.sleep(.1)
        status = self.queue.status('adobe')
        self.assertEqual(status['status'], 'failed')
        self.assertEqual([event['status'] for event in status['startup_events']], ['started','failed'])
        self.assertEqual(status['startup_events'][-1]['exception_type'], 'timeout')
        launcher.assert_awaited_once()

    async def test_restored_adobe_page_failure_preserves_stage_and_fixed_code(self):
        async def launch(*, startup_progress):
            startup_progress({"stage":"adobe_restoring_registration_page", "sequence":1,
                "status":"failed", "elapsed_ms":15000, "duration_ms":15000, "exception_type":"timeout"})
            raise BrowserStartupError('recovery_page_observation_timeout')
        launcher = AsyncMock(side_effect=launch)
        self.queue.enqueue('adobe', {}, launcher, startup_progress=True)
        await asyncio.sleep(.1)
        status = self.queue.status('adobe')
        self.assertEqual(status['status'], 'failed')
        self.assertEqual(status['diagnostic_code'], 'recovery_page_observation_timeout')
        self.assertEqual(status['startup_events'][0]['stage'], 'adobe_restoring_registration_page')
        launcher.assert_awaited_once()

    async def test_unopted_launcher_receives_original_signature_only(self):
        async def launch(*, proxy):
            self.assertEqual(proxy, 'PRIVATE')
            return self.browser()
        self.queue.enqueue('grok', {'proxy':'PRIVATE'}, launch)
        await self.ready('grok')
        self.assertNotIn('startup_events', self.queue.status('grok'))

    async def test_license_retry_preserves_progress_with_reservation_sequence(self):
        LicenseError = type('CloakBrowserLicenseError', (RuntimeError,), {})
        calls = []
        async def launch(*, startup_progress, proxy):
            calls.append(proxy)
            base = {"stage":"starting_browser", "elapsed_ms":0, "duration_ms":0}
            startup_progress({**base, "sequence":1, "status":"started"})
            startup_progress({**base, "sequence":2, "status":"failed" if len(calls) == 1 else "completed"})
            if len(calls) == 1:
                raise LicenseError('session limit reached')
            return self.browser()
        self.queue.enqueue('adobe', {'proxy':'PRIVATE'}, launch, startup_progress=True)
        await self.ready('adobe')
        events = self.queue.status('adobe')['startup_events']
        self.assertEqual([event['sequence'] for event in events], [1,2,3,4])
        self.assertEqual([event['status'] for event in events], ['started','failed','started','completed'])
        self.assertEqual(calls, ['PRIVATE','PRIVATE'])

    def test_progress_projection_rejects_unbounded_and_wrong_type_fields(self):
        valid = {"sequence":1, "stage":"starting_browser", "status":"started", "elapsed_ms":0, "duration_ms":0}
        for value in (None, [], {**valid, 'sequence':True}, {**valid, 'sequence':33},
            {**valid, 'stage':[]}, {**valid, 'status':{}}, {**valid, 'duration_ms':-1},
            {**valid, 'elapsed_ms':150001}, {**valid, 'elapsed_ms':1.0}):
            self.assertIsNone(startup_progress_event(value))
        self.assertEqual(startup_progress_event({**valid, 'private':'PRIVATE', 'exception_type':'PRIVATE'}), valid)

    async def test_registration_owns_slot_until_close_then_oauth_runs_fifo(self):
        first, second, third = self.browser(), self.browser(), self.browser()
        launch = AsyncMock(side_effect=[first,second,third])
        self.queue.enqueue('registration', {}, launch)
        await self.ready('registration')
        browser = self.queue.claim('registration')
        self.queue.enqueue('oauth', {}, launch)
        self.queue.enqueue('registration2', {}, launch)
        await asyncio.sleep(.25)
        self.assertEqual(launch.await_count,1)
        with self.assertRaises(BrowserQueueError): self.queue.claim('registration')
        await browser.close()
        await self.ready('oauth')
        self.assertEqual(launch.await_count,2)
        await self.queue.claim('oauth').close()
        await self.ready('registration2')
        self.assertEqual(launch.await_count,3)

    async def test_close_failure_does_not_release_capacity(self):
        first=self.browser();first.close.side_effect=RuntimeError('secret')
        launch=AsyncMock(side_effect=[first,self.browser()])
        self.queue.enqueue('one',{},launch);await self.ready('one')
        owned=self.queue.claim('one')
        self.queue.enqueue('two',{},launch)
        with self.assertRaisesRegex(BrowserQueueError,'browser_close_pending'):await owned.close()
        await asyncio.sleep(.25)
        self.assertEqual(launch.await_count,1)
        self.assertEqual(self.queue.available,0)
        first.close.side_effect=None
        self.queue.entries['one'].retry_at=0
        await owned.close()
        await self.ready('two')

    async def test_hanging_close_is_bounded_and_late_success_releases(self):
        done=asyncio.Event();first=self.browser();first.close.side_effect=done.wait
        launch=AsyncMock(side_effect=[first,self.browser()])
        self.queue.enqueue('one',{},launch);await self.ready('one')
        owned=self.queue.claim('one');self.queue.enqueue('two',{},launch)
        with self.assertRaises(BrowserQueueError):await owned.close()
        self.assertEqual(launch.await_count,1)
        done.set();await self.ready('two')
        first.close.assert_awaited_once()

    async def test_failed_initializer_retains_unconfirmed_cleanup_and_sanitizes_status(self):
        gate = asyncio.Event()
        first = self.browser()
        first.close.side_effect = gate.wait
        pending = BrowserStartupCleanupPending(first, 'browser_launch_timeout')
        pending.code = 'PRIVATE_CREDENTIAL'
        launch = AsyncMock(side_effect=[pending, self.browser()])
        with self.assertLogs('grok-browser.queue', level='WARNING') as logs:
            self.queue.enqueue('one', {'proxy':'PRIVATE_PROXY'}, launch)
            self.queue.enqueue('two', {}, launch)
            await asyncio.sleep(.1)
        view = self.queue.status('one')
        self.assertEqual(view['status'], 'closing')
        self.assertEqual(view['diagnostic_code'], 'browser_launch_failed')
        self.assertIs(self.queue.entries['one'].browser, first)
        self.assertEqual(self.queue.available, 0)
        self.assertEqual(launch.await_count, 1)
        self.assertNotIn('PRIVATE', str(view) + '\n'.join(logs.output))
        gate.set()
        await self.ready('two')
        self.assertNotIn('one', self.queue.entries)
        self.assertEqual(launch.await_count, 2)
        first.close.assert_awaited_once()

    async def test_cancellation_during_launch_closes_late_browser(self):
        gate=asyncio.Event();first=self.browser()
        async def launch(**_):await gate.wait();return first
        self.queue.enqueue('one',{},launch)
        await asyncio.sleep(.01)
        with self.assertRaises(BrowserQueueError):await self.queue.close('one')
        self.assertEqual(self.queue.available,0)
        gate.set()
        await asyncio.sleep(.3)
        first.close.assert_awaited_once()
        self.assertNotIn('one',self.queue.entries)

    async def test_abandoned_ready_and_cancelled_waiter_are_cleaned(self):
        first=self.browser();launch=AsyncMock(return_value=first)
        self.queue.enqueue('one',{},launch);await self.ready('one')
        self.queue.enqueue('two',{},launch);await self.queue.close('two')
        self.queue.entries['one'].touched-=3
        await asyncio.sleep(.3)
        first.close.assert_awaited_once()
        self.assertFalse(self.queue.entries)

    async def test_license_busy_waits_and_retries_only_launch_with_same_options(self):
        LicenseError=type('CloakBrowserLicenseError',(RuntimeError,),{})
        launch=AsyncMock(side_effect=[LicenseError('session limit reached for your plan'),self.browser()])
        self.queue.enqueue('one',{'proxy':'SECRET'},launch)
        await asyncio.sleep(.02)
        view=self.queue.status('one')
        self.assertEqual(view['status'],'waiting_browser')
        self.assertEqual(self.queue.available,0)
        self.assertNotIn('SECRET',str(view))
        self.assertEqual(launch.await_count,1)
        await self.ready('one')
        self.assertEqual(launch.await_count,2)
        self.assertEqual(launch.call_args.kwargs,{'proxy':'SECRET'})

    async def test_general_launch_failure_is_not_retried(self):
        launch=AsyncMock(side_effect=RuntimeError('secret'))
        self.queue.enqueue('one',{},launch)
        await asyncio.sleep(.3)
        self.assertEqual(self.queue.status('one')['code'],'browser_start_failed')
        self.assertEqual(launch.await_count,1)

    async def test_startup_diagnostic_is_fixed_without_changing_queue_failure_or_retry(self):
        error = BrowserStartupError('browser_launch_timeout')
        launch = AsyncMock(side_effect=error)
        with self.assertLogs('grok-browser.queue', level='WARNING') as logs:
            self.queue.enqueue('one', {'proxy': 'PRIVATE_PROXY'}, launch)
            await asyncio.sleep(.3)
        view = self.queue.status('one')
        self.assertEqual(view['status'], 'failed')
        self.assertEqual(view['code'], 'browser_start_failed')
        self.assertEqual(view['diagnostic_code'], 'browser_launch_timeout')
        self.assertEqual(launch.await_count, 1)
        self.assertIn('code=browser_launch_timeout', '\n'.join(logs.output))
        self.assertNotIn('PRIVATE_PROXY', str(view) + '\n'.join(logs.output))

    async def test_startup_diagnostic_revalidates_modified_exception_code(self):
        error = BrowserStartupError('browser_launch_timeout')
        error.code = 'PRIVATE_PROXY'
        launch = AsyncMock(side_effect=error)
        with self.assertLogs('grok-browser.queue', level='WARNING') as logs:
            self.queue.enqueue('one', {}, launch)
            await asyncio.sleep(.3)
        self.assertEqual(self.queue.status('one')['diagnostic_code'], 'browser_launch_failed')
        self.assertNotIn('PRIVATE_PROXY', str(self.queue.status('one')) + '\n'.join(logs.output))

    async def test_adobe_profile_and_license_failures_are_diagnostic_only_and_not_retried(self):
        for code in ('browser_launch_profile_unavailable', 'browser_launch_license_denied'):
            with self.subTest(code=code):
                launch = AsyncMock(side_effect=BrowserStartupError(code))
                with self.assertLogs('grok-browser.queue', level='WARNING') as logs:
                    self.queue.enqueue(code, {}, launch)
                    await asyncio.sleep(.3)
                view = self.queue.status(code)
                self.assertEqual(view['status'], 'failed')
                self.assertEqual(view['code'], 'browser_start_failed')
                self.assertEqual(view['diagnostic_code'], code)
                self.assertEqual(launch.await_count, 1)
                self.assertEqual(self.queue.cooldown_until, 0)
                self.assertIn('code=' + code, '\n'.join(logs.output))

    async def test_duplicate_reservation_does_not_start_twice(self):
        launch=AsyncMock(return_value=self.browser())
        self.queue.enqueue('one',{},launch);self.queue.enqueue('one',{},launch)
        await self.ready('one')
        self.assertEqual(launch.await_count,1)

if __name__=='__main__':unittest.main()


class ShutdownTests(unittest.IsolatedAsyncioTestCase):
    async def test_shutdown_keeps_late_launch_owned_and_closes_it(self):
        queue=BrowserQueue(close_timeout=.1)
        browser=SimpleNamespace(close=AsyncMock())
        async def launch(**_):
            await asyncio.sleep(.05)
            return browser
        queue.enqueue('one',{},launch)
        await asyncio.sleep(.01)
        await queue.shutdown()
        browser.close.assert_awaited_once()
        self.assertFalse(queue.entries)
