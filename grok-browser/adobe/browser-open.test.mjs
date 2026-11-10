import test from 'node:test';
import assert from 'node:assert/strict';
import { mkdtemp, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { BrowserRegistration } from './browser.mjs';
import { PrivateRunStore } from './core.mjs';

test('open preserves restored Adobe pages after any durable signup submission', async t => {
  for (const marker of ['emailSubmitted', 'passwordSubmitted', 'submission.started']) {
    await t.test(marker, async subtest => {
      const directory = await mkdtemp(join(tmpdir(), 'adobe-browser-open-'));
      const store = new PrivateRunStore(directory, 'fixture');
      await store.init({ email: 'fixture@example.invalid' });
      if (marker === 'submission.started') await store.markSubmissionStarted();
      else await store.save({ [marker]: true, stage: 'awaiting_profile' });
      subtest.after(async () => {
        await store.close();
        await rm(directory, { recursive: true, force: true });
      });
      const stateBefore = await store.load();
      let navigations = 0;
      const fakePage = url => ({
        url: () => url,
        goto: async () => { navigations++; throw new Error('restored pages must not navigate'); },
        getByRole: () => { throw new Error('restored pages must not reopen signup'); },
      });
      const blank = fakePage('about:blank');
      const restored = fakePage('https://auth.services.adobe.com/signup');
      const unrelated = fakePage('https://unrelated.example.invalid/');
      const context = {
        pages: () => [blank, restored, unrelated],
        on: () => {},
        newPage: async () => { throw new Error('restored context must not create another page'); },
      };
      let launches = 0;
      const browser = new BrowserRegistration(store, {}, {
        launchBrowser: async (profileDir, options) => {
          launches++;
          assert.equal(profileDir, join(store.runDir, 'browser'));
          assert.deepEqual(options, { headless: false });
          return context;
        },
      });
      browser.status = async () => ({ restored: browser.page === restored });
      assert.deepEqual(await browser.open(), { restored: true });
      assert.equal(launches, 1);
      assert.equal(navigations, 0);
      assert.deepEqual(await store.load(), stateBefore);
    });
  }
});
