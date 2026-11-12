import { RegistrationError } from './core.mjs';

// 仅对应已核对的 Adobe 韩国必选组件；营销控件不在此列表中。
export const KOREA_REQUIRED_GROUP = 'MarketingConsent-SouthKorea-Required';
export const KOREA_REQUIRED_CONSENTS = Object.freeze({
  kr_storage: 'SouthKorea-StorageConsent',
  kr_transfer: 'SouthKorea-ServiceProviderConsent',
});

/** 根据当前可见组件建立有限计划，缺失、重复或地区不符时不猜测控件。 */
export function regionalConsentPlan(form, country) {
  const fields = form.fields.filter(field => Object.hasOwn(KOREA_REQUIRED_CONSENTS, field.regionalConsent || ''));
  const groups = form.regionalConsentGroupCount || 0;
  if (!groups && fields.length === 0) return [];
  if (country !== 'KR' || groups !== 1 || fields.length !== 2) {
    throw new RegistrationError('regional_consent_not_ready');
  }
  return Object.keys(KOREA_REQUIRED_CONSENTS).map(key => {
    const matches = fields.filter(field => field.regionalConsent === key);
    if (matches.length !== 1 || matches[0].type !== 'checkbox' || matches[0].disabled === true) {
      throw new RegistrationError('regional_consent_not_ready');
    }
    return { key, field: matches[0] };
  });
}

/** 已知必选同意在资料阶段等待批准，最终校验仍要求实际勾选。 */
export function regionalConsentPreflight(form, country, defer = false) {
  const required = new Set(regionalConsentPlan(form, country).map(item => item.field));
  return { ...form, fields: form.fields.map(field => {
    if (!required.has(field)) return field;
    if (defer && field.checked !== true) return { ...field, required: false, invalid: false,
      validity: { ...field.validity, valueMissing: false } };
    return { ...field, required: true };
  }) };
}
