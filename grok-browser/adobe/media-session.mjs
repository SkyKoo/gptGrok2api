import { RegistrationError } from './core.mjs';

// The official client forwards this opaque value; its internal fields are not
// a supported readiness contract. Never fabricate or merge session fields.
export function isUsableSherlock(value) {
  return typeof value === 'string' && value.length > 0 && value.length <= 65536 &&
    /^[\x21-\x7e]+$/.test(value) && !/[;,]/.test(value);
}
export function mediaCookieHeader(items) {
  const sherlock=items.filter(c=>c.name==='sherlockToken'&&isUsableSherlock(c.value)).sort((a,b)=>(b.path||'/').length-(a.path||'/').length || (b.domain||'').replace(/^\./,'').length-(a.domain||'').replace(/^\./,'').length)[0];
  if(!sherlock)return '';
  return [...items.filter(c=>c.name!=='sherlockToken'),sherlock].map(c=>`${c.name}=${c.value}`).join('; ');
}
export async function ensureMediaSession(page,context,{timeoutMs=30000,now=Date.now,pause=ms=>new Promise(resolve=>setTimeout(resolve,ms))}={}) {
  let cookie=mediaCookieHeader(await context.cookies('https://firefly.adobe.com/'));
  if(cookie)return cookie;
  await page.goto('https://firefly.adobe.com/generate/image',{waitUntil:'domcontentloaded',timeout:45000});
  const deadline=now()+timeoutMs;
  while(now()<deadline){
    cookie=mediaCookieHeader(await context.cookies('https://firefly.adobe.com/'));
    if(cookie)return cookie;
    await pause(1000);
  }
  throw new RegistrationError('media_session_incomplete');
}
