import { test } from 'node:test'
import assert from 'node:assert/strict'
import { normalizeLogInputImages, logOutputSource } from '../src/api/logInputImages.ts'
const url = '/api/logs/input-images/' + 'a'.repeat(64) + '.png'
test('input positions including repeats remain distinct from output URLs', () => {
  const detail = { input_images: [{ index: 1, url }, { index: 2, url }], output_images: [{ url: '/images/output.png' }] }
  assert.deepEqual(normalizeLogInputImages(detail).map(i => i.index), [1, 2])
  assert.deepEqual(normalizeLogInputImages(detail).map(i => i.url), [url, url])
  assert.deepEqual(logOutputSource(detail), detail.output_images)
})
test('legacy dimensions produce unavailable numbered inputs, not fake results', () => {
  const detail = { request_meta: { image_size: { input_dimensions: [{ index: 1, width: 2048, height: 2048 }, { index: 2, width: 620, height: 620 }] } }, image_urls: [{ url: '/images/output.png' }] }
  assert.deepEqual(normalizeLogInputImages(detail).map(i => [i.index, i.width, i.height, i.url]), [[1, 2048, 2048, ''], [2, 620, 620, '']])
  assert.deepEqual(logOutputSource(detail), detail.image_urls)
  assert.deepEqual(normalizeLogInputImages({}), [])
})
test('administrator token is never sent to URLs from arbitrary log content', () => {
  for (const bad of ['https://other.example/image.png', '/api/accounts', url + '?key=secret', 'data:image/png;base64,test', '/api/logs/input-images/../secret']) {
    assert.equal(normalizeLogInputImages({ input_images: [{ url: bad }] })[0].url, '')
  }
  assert.deepEqual(logOutputSource({ input_images: [{ url }], request_meta: { url }, request_shape: { url }, url: '/images/legacy.png' }), { url: '/images/legacy.png' })
})
