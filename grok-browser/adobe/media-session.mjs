import { RegistrationError } from './core.mjs';

export function isCompleteSherlock(value) {
  try {
    const fields=JSON.parse(Buffer.from(value,'base64').toString('utf8'));
    return ['sid','ark','bfp','ftr'].every(key=>typeof fields[key]==='string'&&fields[key].length>0);
  } catch {return false;}
}
export function mediaCookieHeader(items) {
  const sherlock=items.filter(c=>c.name==='sherlockToken'&&isCompleteSherlock(c.value)).sort((a,b)=>(b.path||'/').length-(a.path||'/').length || (b.domain||'').replace(/^\./,'').length-(a.domain||'').replace(/^\./,'').length)[0];
  if(!sherlock)return '';
  // Do not let an earlier partial/domain-specific cookie shadow a complete one.
  return [...items.filter(c=>c.name!=='sherlockToken'),sherlock].map(c=>`${c.name}=${c.value}`).join('; ');
}
export async function ensureMediaSession(page,context,{timeoutMs=30000,now=Date.now,pause=ms=>new Promise(resolve=>setTimeout(resolve,ms))}={}) {
  let cookie=mediaCookieHeader(await context.cookies('https://firefly.adobe.com/'));
  if(cookie)return cookie;
  // The public editor initializes the media session. No Generate action occurs.
  await page.goto('https://firefly.adobe.com/generate/image',{waitUntil:'domcontentloaded',timeout:45000});
  const deadline=now()+timeoutMs;
  while(now()<deadline){
    cookie=mediaCookieHeader(await context.cookies('https://firefly.adobe.com/'));
    if(cookie)return cookie;
    await pause(1000);
  }
  throw new RegistrationError('media_session_incomplete');
}
