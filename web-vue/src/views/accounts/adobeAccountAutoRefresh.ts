type Timer = ReturnType<typeof setTimeout>
interface RefreshEnvironment {
  visible?: () => boolean
  setTimer?: (callback: () => void, delay: number) => Timer
  clearTimer?: (timer: Timer) => void
  subscribeVisibility?: (callback: () => void) => () => void
}

// List polling observes server work only; opening this page never starts a job.
export function startAdobeAccountAutoRefresh(refresh: () => Promise<void>, pending: () => boolean, environment: RefreshEnvironment = {}): () => void {
  const visible = environment.visible || (() => document.visibilityState !== 'hidden')
  const setTimer = environment.setTimer || setTimeout
  const clearTimer = environment.clearTimer || clearTimeout
  const subscribe = environment.subscribeVisibility || ((callback: () => void) => {
    document.addEventListener('visibilitychange', callback)
    return () => document.removeEventListener('visibilitychange', callback)
  })
  let stopped = false, running = false, timer: Timer | undefined
  const schedule = () => { if (!stopped) timer = setTimer(() => { timer = undefined; void run() }, 10000) }
  const run = async () => {
    if (stopped || running) return
    running = true
    try { if (visible() && pending()) await refresh() }
    catch { /* A transient list error does not stop observing server work. */ }
    finally { running = false; schedule() }
  }
  const unsubscribe = subscribe(() => {
    if (!visible() || stopped || running) return
    if (timer !== undefined) { clearTimer(timer); timer = undefined }
    void run()
  })
  schedule()
  return () => { stopped = true; if (timer !== undefined) clearTimer(timer); unsubscribe() }
}
