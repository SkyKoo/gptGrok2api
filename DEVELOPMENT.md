# Account quotas and model routing — development branch

Baseline main: ef38a0f934200ff62b45236328772dba917b0556.
Baseline production image: gptgrok2api:sha-ef38a0f93420-20261010.
Branch: codex/cfm-account-model-routing. No merge without user confirmation.

1. Capability quotas, atomic reservations, scoped cooldowns and refresh — passed local tests, production deployment and one text/one image verification (c0ca414).
2. Deduplicated upstream model discovery and dynamic catalog — deployed and verified (4a2fa00); fixed an all_models aggregation omission before progressing. Three metadata refreshes and one gpt-6 Responses stream passed; cache survives restart.
3. Configurable routing, scoped rejection, model observability and maintenance admission — local implementation complete; deployment verification pending.

All persistence changes must be additive. Ordinary rollback replaces only the app
image, never restores old business data over newer writes. Before every deployment:
verify no active registrations/requests/image tasks; consistent backup; verify archive,
mounts, configuration, protected data and peer containers. Test old-main readability
of the new data before deployment. Retain each image and configuration checkpoint.
