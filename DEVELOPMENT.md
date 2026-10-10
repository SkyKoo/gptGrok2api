# Account quotas and model routing — development branch

Baseline main: ef38a0f934200ff62b45236328772dba917b0556.
Baseline production image: gptgrok2api:sha-ef38a0f93420-20261010.
Branch: codex/cfm-account-model-routing. No merge without user confirmation.

1. Capability quotas, atomic reservations, scoped cooldowns and refresh — passed local tests, production deployment and one text/one image verification (c0ca414).
2. Deduplicated upstream model discovery and dynamic catalog — deployed and verified (4a2fa00); fixed an all_models aggregation omission before progressing. Three metadata refreshes and one gpt-6 Responses stream passed; cache survives restart.
3. Configurable routing, scoped rejection, model observability and maintenance admission — deployed and verified (dbe8b27). One auto text request resolved to gpt-6; one reference edit succeeded via gpt-5-3. Pending quota reconciled; no credential changes during verification.

All persistence changes must be additive. Ordinary rollback replaces only the app
image, never restores old business data over newer writes. Before every deployment:
verify no active registrations/requests/image tasks; consistent backup; verify archive,
mounts, configuration, protected data and peer containers. Test old-main readability
of the new data before deployment. Retain each image and configuration checkpoint.

Final running code: dbe8b272257d67d47a9848a70a45aed2d8408d17.
Image: gptgrok2api:dev-dbe8b272257d-batch3.
Final consistent backup: /opt/gptgrok2api/backups/capability-batch3-20261010T162747Z.
All 698 business data files matched after the final switch: 151 accounts, 35 registration
records and 66 image tasks retained. Verification subsequently adds its own test records.
The baseline main image was started offline against a copy of the new accounts, cache
and configuration; all 151 accounts readable and copied files unchanged. Production
was not switched back to main. Image conversation default remains gpt-5-3; alternative
model mask/edit routing is covered by isolated fixtures, not a full live quality matrix.

This final documentation-only acceptance record follows the running code commit.
Main remains unmerged pending user review.
