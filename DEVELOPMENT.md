# Account quotas and model routing — development branch

Baseline main: ef38a0f934200ff62b45236328772dba917b0556.
Baseline production image: gptgrok2api:sha-ef38a0f93420-20261010.
Branch: codex/cfm-account-model-routing. No merge without user confirmation.

1. Capability quotas, atomic reservations, scoped cooldowns and refresh — in progress.
2. Deduplicated upstream model discovery and dynamic catalog — pending first deployment verification.
3. Configurable routing and model observability — pending second deployment verification.

All persistence changes must be additive. Ordinary rollback replaces only the app
image, never restores old business data over newer writes. Before every deployment:
verify no active registrations/requests/image tasks; consistent backup; verify archive,
mounts, configuration, protected data and peer containers. Test old-main readability
of the new data before deployment. Retain each image and configuration checkpoint.
