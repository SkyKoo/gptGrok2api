import test from 'node:test'
import assert from 'node:assert/strict'
import { setMediaCapabilities, validateMediaCombination } from '../src/lib/mediaCapabilities.ts'
import { buildVideoForm, studioReferenceLimit } from '../src/lib/studioMedia.ts'
import { mailboxDeletionBlocked, mailboxRegistrationDisabled, mailboxRegistrationRetired } from '../src/views/mailboxRegistrationPolicy.ts'

const image = { kind: 'image', qualities: ['auto'], sizes: ['1024x1024'], max_count: 1, max_reference_images: 0, variants: [{ qualities: ['auto'], sizes: ['1024x1024'], max_count: 1, max_reference_images: 0 }] }
const video = { kind: 'video', sizes: ['1920x1080'], resolutions: ['1080p'], durations: [8], audio: false, max_reference_images: 0, variants: [{ sizes: ['1920x1080'], resolutions: ['1080p'], durations: [8], audio: false, max_reference_images: 0, max_count: 1 }] }

test('Veo free tier sends logical model and supported options without channel or Grok preset', t => {
 setMediaCapabilities({ 'veo-3.1-lite': video }); t.after(() => setMediaCapabilities())
 const form = buildVideoForm('paper plane', { model: 'veo-3.1-lite', seconds: 8, size: '1920x1080', resolution: '1080p', audio: false, preset: 'spicy' }, [])
 assert.equal(form.get('model'), 'veo-3.1-lite')
 assert.equal(form.get('seconds'), '8'); assert.equal(form.get('resolution_name'), '1080p')
 assert.equal(form.get('generate_audio'), 'false')
 assert.equal(form.has('provider'), false); assert.equal(form.has('channel'), false); assert.equal(form.has('preset'), false)
 assert.throws(() => buildVideoForm('test', { model: 'veo-3.1-lite', seconds: 4, size: '1920x1080', resolution: '1080p' }, []), /参数组合/)
 assert.throws(() => buildVideoForm('test', { model: 'veo-3.1-lite', seconds: 8, size: '1920x1080', resolution: '1080p', audio: true }, []), /参数组合/)
})
test('free image enforces count and excludes unverified references', t => {
 setMediaCapabilities({ 'gemini-3.1-flash-image': image }); t.after(() => setMediaCapabilities())
 assert.equal(studioReferenceLimit('image', 'gemini-3.1-flash-image'), 0)
 assert.doesNotThrow(() => validateMediaCombination('gemini-3.1-flash-image', {size:'1024x1024',count:1,references:0}))
 for (const request of [{quality:'high'}, {count:2}, {references:1}, {size:'2048x2048'}]) assert.throws(() => validateMediaCombination('gemini-3.1-flash-image', {size:'1024x1024',count:1,...request}), /参数组合/)
})
test('union of capabilities does not create unsupported cross-channel parameter combinations', t => {
 setMediaCapabilities({ combined: { kind: 'video', variants: [
  { sizes: ['1280x720'], resolutions: ['720p'], durations: [6], max_reference_images: 2 },
  { sizes: ['1920x1080'], resolutions: ['1080p'], durations: [8], max_reference_images: 0 },
 ] } }); t.after(() => setMediaCapabilities())
 assert.throws(() => validateMediaCombination('combined',{size:'1920x1080',resolution:'1080p',seconds:6}), /参数组合/)
 assert.throws(() => validateMediaCombination('combined',{size:'1920x1080',resolution:'1080p',seconds:8,references:1}), /参数组合/)
 assert.doesNotThrow(() => validateMediaCombination('combined',{size:'1280x720',resolution:'720p',seconds:6,references:2}))
})
test('Adobe mailbox claims and reservations prevent deletion and policies stay independent', () => {
 for (const mailbox of [{adobe_claimed:true}, {adobe_reserved:true}]) assert.equal(mailboxDeletionBlocked(mailbox), true)
 assert.equal(mailboxRegistrationDisabled({ adobe_disabled:true },'adobe'), true)
 assert.equal(mailboxRegistrationDisabled({ adobe_disabled:true },'openai'), false)
 assert.equal(mailboxRegistrationRetired({ adobe_retired:true },'adobe'), true)
 assert.equal(mailboxRegistrationRetired({ adobe_retired:true },'grok'), false)
})
