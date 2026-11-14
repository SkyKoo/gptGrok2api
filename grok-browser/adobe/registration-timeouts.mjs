// 单步骤等待允许慢代理完成正常加载；整个 RPC 仍由 Worker 的固定总时限封顶。
// 只调整等待预算，不改变提交次数、验证码次数或人工验证观察窗口。
export const REGISTRATION_TIMEOUTS = Object.freeze({
  launch: 60_000,
  navigation: 90_000,
  form: 40_000,
  reload: 60_000,
  stage: 60_000,
  actionable: 20_000,
  click: 30_000,
  recovery: 30_000,
  restoration: 30_000,
  navigationObservation: 10_000,
  identity: 20_000,
  command: 300_000,
});
