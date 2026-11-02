import asyncio
import unittest
from types import SimpleNamespace
from unittest.mock import AsyncMock
from browser_sessions import BrowserQueue, BrowserQueueError


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
