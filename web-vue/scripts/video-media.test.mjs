import { test } from 'node:test'
import assert from 'node:assert/strict'
import { isVideoUrl } from '../src/lib/mediaType.ts'
import { logOutputSource } from '../src/api/logInputImages.ts'

test('video previews recognize media paths without relying on query strings', () => {
  for (const url of ['/images/video/clip.mp4', '/images/video/clip.WEBM?x=1', 'https://example.test/clip.mov']) assert.equal(isVideoUrl(url), true)
  for (const url of ['/images/image.png?video=clip.mp4', '/images/clip.mp4.meta.json', '/images/clip.png', 'data:image/png;base64,AAAA']) assert.equal(isVideoUrl(url), false)
})

test('video log results are separate from references and arbitrary diagnostic URLs', () => {
  const videos = [{ url: '/images/video/clip.mp4' }]
  assert.deepEqual(logOutputSource({ output_videos: videos, request_shape: { url: '/private/reference.png' }, monitor: { url: 'https://private.invalid' } }), videos)
})
