# Adobe integration validation

Checked locally in Docker on 2026-11-11 (Asia/Shanghai). These results are for the
`codex/adobe-integration` development branch. No production service was deployed
or restarted, and `main` was not merged.

## Free model admission

Only complete parameter combinations confirmed with the existing Web Free
account enter the Adobe allowlist. Public discovery visibility is not sufficient.

| Logical model | Adobe implementation | Enabled parameter tier | Evidence |
| --- | --- | --- | --- |
| `gemini-3.1-flash-image` | `gemini-flash / nano-banana-3` | One 1024×1024 PNG, text input | Node protocol success on 2026-11-10; new Go client success on 2026-11-11 |
| `veo-3.1-lite` | `veo / 3.1-lite-generate` | One 1920×1080 / 1080p / 8s / 16:9 video, no audio, text input | Node protocol success on 2026-11-10; current Go and Node control requests rejected as recorded below |

Unverified image sizes, image references/masks, multiple outputs, video audio and
other video tiers are rejected before Adobe submission. Their availability must
be separately qualified before they can be advertised.

The current discovery ID for GPT Image 2.5 Sunburst is
`gpt-image / gpt-image-2.5-prism`. A bounded Web Free test with 1024×1024, one output,
`detailLevel:3`, current nonce and metadata returned HTTP 403 `access_error` with
no upstream task. The Adobe Sunburst variant is therefore excluded from the Free
allowlist. The existing ChatGPT logical model remains available; multi-channel
priority behavior is covered by fixtures without advertising this rejected route.

## Go protocol validation

The client sends one generation POST. It never retries that POST, changes account,
or changes channel. Redirects are disabled. Nonce generation uses the Adobe
account identity and JavaScript-compatible UTF-16 truncation of the prompt.
Current video parameters use top-level `duration` and
`generationSettings.aspectRatio`.

One real Gemini request using the new Go client completed in approximately 22.8
seconds. The output was downloaded anonymously and decoded as a 1024×1024 PNG:

- Bytes: 1,413,113.
- SHA-256: `194b91d8f6811a21a83f7a2b55239f6ed39c1a4bfa55e72d5b925cda46a98196`.
- Before, immediate after, and 35 seconds after generation:
  `total=10, used=0, available=10`.

The first new Go Veo request returned HTTP 403 `access_error`, with no task link.
A read-only comparison found the same endpoint, model ID/version, dimensions,
resolution, duration, audio flag, metadata, nonce scheme, session and explicit
proxy as the earlier successful Node request. One concrete difference was
`Accept: application/json` versus the verified Node `Accept: */*`. After aligning
that header, one separately authorized comparison request still returned the
same 403 and no task. After independently verifying the real browser/IMS refresh
chain, a final explicitly authorized test used the new same-identity token and
the original complete media cookie retained intact by `Store.ReplaceCredentials`.
It also returned HTTP 403 `access_error`, with no task. All three generation
failures are retained, and generation tests stopped. No request was automatically
replayed. The cause is not established; HTTP/TLS transport and current account
entitlements/session are distinct remaining possibilities. This result does not
invalidate the earlier real Free video success or establish current Go video
success.

The prior successful video was H.264, 1920×1080, 8 seconds, 24 fps, with no audio.
The new Go client also polled this existing task to `COMPLETED` and anonymously
downloaded exactly the same 2,614,195 bytes without submitting a new job.
Balance and polling negotiated HTTP/2; the public object download used HTTP/1.1.
A separate forced-HTTP/1.1 diagnostic failed to connect for balance and polling;
no production transport setting was changed on this evidence.
The Go-downloaded file also passed a full `ffmpeg` decode in an ephemeral Docker
container. `ffprobe` confirmed H.264, 1920×1080, 8 seconds, 24 fps and no audio
stream. Its SHA-256 was
`f6e0de1a03c7426ab1d90a57cd6d3216e3e61c2eb7128fc551a464ae5a8e111b`.

## Credentials and quota observations

An IMS refresh attempt using only cookies exported for `firefly.adobe.com`
returned a token belonging to a different identity. It was rejected; the original
account and token were not overwritten. This proves that a successful HTTP token
response alone is insufficient. The implementation exports IMS-scoped cookies
separately, requests `guest_allowed=false`, checks the refreshed identity, and
uses credential revision checks before saving. Browser fallback uses the shared
worker queue and the saved profile, with the same identity check.

A copied-profile Worker token refresh then succeeded: the token changed and its account
identity matched. The new Go client read a valid balance using that token. A Go
IMS refresh using the newly exported IMS-scoped cookies also succeeded, changed
the token again, preserved identity and returned a valid balance afterward.
This verifies browser refresh and the subsequent direct HTTP refresh path.

The copied-profile Firefly cookie initially contained only the Sherlock `sid` and
`ftr` fields, without `ark`/`bfp`. An attempted video preflight with this refreshed export
was stopped by local session validation before any generation POST. This is a
separate limitation from token refresh: an authenticated token and valid credit
balance do not prove a complete media session. The existing complete session is
not overwritten with an incomplete replacement. The final implementation retains
the entire prior complete media cookie when the refreshed cookie is partial or
absent, while updating the verified token/IMS credentials with a revision check.
It does not assemble new challenge fields. Public account state explicitly reports
`media_session_status: preserved` and `media_session_refreshed: false`. If neither
the old nor new cookie is complete, refresh cannot make the account media-ready.
A new complete, changed media cookie is reported as `refreshed`; initial imports
are reported as `imported`.

This preservation path was itself exercised through the real Go encrypted store:
the token changed, account identity stayed fixed, the original cookie remained
byte-for-byte identical, and balance returned HTTP 200. The final independent
Veo test described above then failed with 403. The complete media session was not
newly established in this round, and new Go video submission success remains
unverified.

Repeated successful image generation and the earlier video success did not
change the observed 10/0/10 balance, including a delayed 35-second observation.
These reads do not identify a per-generation price or prove unlimited access.
CFM therefore:

- Stores one shared Adobe account balance for all Adobe models.
- Distinguishes known zero, unknown, stale, and failed queries.
- Runs only one media request per Adobe account at a time.
- Reconciles after a 30-second settlement interval instead of subtracting a
  fabricated fixed cost.
- Retains an ambiguous submission reservation until explicit reconciliation;
  an unchanged balance cannot prove that no generation occurred.
- Refreshes credentials before selection, never to replay a failed generation.

Maintenance uses account-bound egress, skips active media requests and deployment
maintenance, limits each scan to four accounts, and backs off failed checks.
`adobe_maintenance.enabled` defaults to true. Credentials near expiry use scoped
IMS refresh and then the shared browser fallback if a profile exists.

## Persistence and validation boundaries

`adobe_accounts.json.enc` and `adobe_jobs/*.json.enc` use AES-GCM with the same
configured administrative secret derivation as the existing OAuth store. Backups
must retain these encrypted files, the corresponding configuration/secret and
browser profiles; restoring only the ciphertext without the matching secret is
insufficient. Public account JSON uses an explicit safe projection and never
returns tokens, cookies, passwords, browser profiles or signed media URLs.

Focused Docker tests cover credential lifetime and identity, nonce truncation,
unsupported Free tiers, one-POST semantics, terminal-state polling, anonymous
media download, URL allowlists, encryption/redaction, credential revisions,
shared quota reservations, ambiguous submissions, restart continuation, account
routing and admin-only list access. Further regressions cover preserving registration
ownership on reimport, renewing a pinned task's token without generation or quota
reconciliation, browser fallback maintenance locking, one read-only 401 recovery,
and preserving a complete media cookie during token-only refresh. Additional gateway and UI validation is
recorded in the implementation delivery.

Raw request state, account exports and generated media are private local test
artifacts under the Adobe standalone tool's `.private-docker/api-tests/` tree.
They are not committed to this repository. No email, token, cookie, password,
license, or signed output URL is included in this document.

## Read-only request comparison after the final rejection

No further generation was submitted for this review. An offline transport fixture
compares the Go request to the complete successful Node video payload retained
on 2026-11-10. It supplies the same original prompt and normalizes only the random
seed for comparison. Every payload field, method, path, query, explicit security
header, UUID format and an independently calculated nonce match. The regression
is `TestVeoPayloadMatchesVerifiedNodeProtocol`.

| Item | Comparison result |
| --- | --- |
| Submit method, host/path and query | Identical POST endpoint; no query parameters |
| Model ID/version | `veo / 3.1-lite-generate` in both |
| Output count and seed | One output; one random integer from 1 through 999,999 |
| Size, resolution, duration, aspect ratio | 1920×1080, `1080p`, 8, `16:9` in both |
| Audio, references, input storage | `false`, empty list, `storeInputs:true` in both |
| Generation metadata | `text2video / ff-video-generate` in both |
| Nonce | SHA-256 of the same identity and first 256 JavaScript UTF-16 units; golden fixture matches |
| Authentication/session headers | Bearer token, `clio-playground-web`, complete Sherlock header and UUID v4; no Cookie header sent to generation |
| Origin, Referer and client hints | Source defaults match the verified Node constructor exactly |
| Accept | The initial difference was corrected to `*/*`; later rejections persisted |
| Configured proxy | Both live harnesses explicitly used the same local HTTP proxy endpoint |

The real new Go test prompts and random seeds differed from the historical Node
success. They remained simple product-shot prompts. No evidence establishes them
as the cause of rejection. The fixture uses the historical prompt to prevent an
unnoticed serialization or truncation difference.

The historical record saved header names, not all concrete header values or the
exit IP. Matching source defaults and configured proxy do not prove an identical
historical User-Agent override or physical exit IP. Go authenticated reads were
observed negotiating HTTP/2; the original Node fetch transport and Go TLS
fingerprints are not byte-identical. No additional application-level field
mismatch was found, and no transport fingerprint or session change is asserted
as the cause of the 403. This review produced an offline regression, not a new
claimed live video success.


## Final Node control with the same refreshed credentials

A single bounded Node HTTP request then used the exact private account export
produced by the final Go store CAS: the same new token and complete prior media
cookie. Its configured proxy, prompt, model ID/version, dimensions, resolution,
duration, aspect ratio, audio setting and metadata matched the final Go request.
Only the new request's random seed/UUID and submission time differed. The existing
Node protocol tool was reused through a temporary diagnostic copy; no browser
was started and no webpage generation button was clicked.

The Node control also returned HTTP 403 `access_error`, with no poll link and no
created task. Balance before and after was HTTP 200 with `10 / 0 / 10`. Exactly
one POST was sent, its exclusive submission marker was retained, and no further
generation was attempted. This is a separate diagnostic request, not application
retry behavior.

The current rejection is therefore reproducible outside the new Go client; the
evidence does not support attributing it solely to Go HTTP/TLS behavior. It also
does not distinguish media-session validity, current video entitlement, the
request itself, or upstream policy. No speculative transport workaround was
implemented. Successful historical video generation and the new Go read/download
verification remain valid separate observations.
