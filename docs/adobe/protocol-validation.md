# Adobe integration validation

## Latest local video success (2026-11-12, 10:20–10:21 CST)

The user's Omni Flash 1.2 request returned a real HTTP/2 408 after 492 ms
(1,607 ms overall). It selected the second account's previous-day media capture,
revision 3, rather than the first account's corrected collector session. Its
720p / 5 seconds / 16:9 / audio=true parameters matched the official contract.
The account scheduler accepted token/format validity but had no selected-account
media preparation. This was a confirmed coverage gap, not proof of a unique
cause for every upstream 408.

New image/video requests now pin the account, perform one normal browser media
collection, then use its updated credentials for preflight, upload and HTTP
submission. This policy does not infer an upstream token TTL. Failed preparation
stops without submission or channel/account fallback. Preparation markers and
credential updates are atomic; interrupted pre-submit tasks are closed only
after checking their durable phase. Existing accepted/unknown requests are not
replayed. Video serialization also now omits empty referenceBlobs as the current
official mapper does; no speculative seeds/modelSpecificPayload were added.

One distinct paper-boat prompt was submitted through CFM `POST /v1/videos`,
model `gemini-omni-flash-1.2`, 720p, 5 seconds, audio=true. The application itself
collected the first account's media session, revision 7→8, approximately 42
seconds after admission. The task completed in 94 seconds overall, with the
original upstream `gemini-omni/omni-flash-1.2` handle recorded as COMPLETED.
Request ID: `5640993f-0be4-4b58-a656-05a132c65a7d`.

The CFM content endpoint returned HTTP 200 and a 3,240,574-byte MP4, SHA-256
`a8e873c28dbfe04e07747ee239727eb1a0697d7f0615c8b8192af478ce89fe82`.
FFprobe confirmed 1280×720 H.264, exactly 5 seconds of video, and 48 kHz stereo
AAC (container duration 5.034667 seconds). Both streams decoded fully; a sampled
frame shows the red paper boat on a garden pond. The video pool changed 1→0
(used 0→1); the account returned to idle. Exactly one video was added, all six
image task IDs/statuses/errors were unchanged, and the user's original 408 task
and its account credentials were preserved. No claim is made about its outcome.

Both Go packages passed final race testing, including full request contracts,
zero-submit preparation failures, restart and concurrent-update protections;
the TypeScript/Vite application build passed. The local app image is
`01d227ed03a882354f28aed0044088f08176aa6aa3af0aa7bdf1fcaad8f511e8`.
Worker and database were unchanged, and production was not deployed.

## Automatic task reconciliation (2026-11-12, local Docker)

The user requested system-managed task handling instead of manual review.
Regression fixtures cover original-handle polling/download without resubmission,
unknown results, the persisted 30-minute grace period, concurrent relogin and
task changes, damaged journals, atomic audits, and independent quota/permission
retry. Routing failures before HTTP now checkpoint a shared five-minute backoff
without overwriting newer account or quota state. Both Go packages passed race
testing; 30 Adobe frontend unit tests, TypeScript/Vite build and isolated desktop/
mobile browser checks passed.

At 09:50:43 CST, the normal local scheduler automatically released the second
test account's old handle-less 408 reservation. It had first queried quota and
recorded `reviewed_by=system`, `method=automatic_no_handle`, `outcome=unknown`.
No manual release API or new generation request was used. This confirms local
occupancy handling, not whether that historical upstream request generated or
consumed quota. A 30-second settlement fence remains before the next quota read.
At 09:51:41 CST, the scheduler refreshed both quota and generation permissions
successfully; the account returned to idle with no pending reservation. Account
credentials and all six original image task IDs, statuses and errors were
unchanged. The first account's previous audits were preserved, and no image task
was added. Only the local app was updated; Worker, database and production were
not restarted or deployed by this verification.

## Latest local image success (2026-11-12, 09:13 CST)

After a fresh normal Worker relogin with the corrected collector, one Nano Banana
2.1 image completed through CFM's unified `POST /api/image-tasks/generations`
and the Go HTTP adapter. The browser was used only for session collection.
The media session was approximately 62 seconds old at submission; settlement
was complete and current model permission/quota were checked before the single
generation request. The upstream `gemini-flash/nano-banana-2026-09` job reached
`COMPLETED` in about 30 seconds; HTTP 408 did not recur in this run.

The CFM download returned a fully decoded 1024×1024 PNG, 1,434,347 bytes,
SHA-256 `2974d85e9f43931e50788fec28fdf5e45cd33f65dab7cd4f0500ebbf6e82ad08`.
Its yellow vase and three white daisies matched the new prompt. Daily image
availability changed from 3 to 2 (used 0 to 1); the separate image taste pool
remained 3 and daily video pool remained 1. The first account returned to idle;
the second account's unknown reservation, all prior failures and two audit
records remain intact. No generation was replayed.

This is a successful local image validation of the fixed collection path, not
proof of video recovery, long-lived media validity or a unique explanation for
every historical 408. The fixes remain local and uncommitted; production was
not changed.

## Submission diagnostics and initialization follow-up (2026-11-12)

After an explicit audited release of the first local account's old reservation,
one separately authorized, fresh Nano Banana 2.1 request was submitted through
CFM's unified asynchronous image API (1024×1024, one output). Adobe returned an
actual HTTP/2 408 response in 516 ms, with top-level `error_code:timeout_error`, no
`x-access-error`, and no trusted poll URL. The new unknown reservation remains;
the previous task and its audit remain in history. It was not replayed.

Failed jobs now retain a bounded submission diagnostic: received-response flag,
HTTP status/protocol, elapsed time, fixed error source/code and recognized access
error category. They do not retain raw responses or arbitrary headers. This
distinguishes a fast upstream rejection from CFM's own deadline without changing
unknown-result handling.

Read-only Go and Node preflights using the same account, proxy, UA and request
body both reported free entitlement for Nano Banana 2 and 2.1. They do not carry
the media session headers and cannot prove that the submission service accepts
those credentials. Official asset history contained four older assets, including
three positive-control IDs, and no new result. Missing history is not proof that
no work or charge occurred and does not authorize release.

A normal browser refresh observed the same SPL source updating its official ARP
value repeatedly for approximately 6.8 seconds after its first nonempty emission.
Thus first-nonempty capture is premature. This is a demonstrated lifecycle defect,
not yet proof that it caused HTTP 408; no generation using the later captured
value has been tested. Token contents and length are not validity criteria.

The revised collector requires one observed SDK instance, callback measurements
for all three vendors in the verified editor configuration, and 250 ms with no
change in the official value or measurement snapshot, within the original
30-second deadline. Multiple instances or missing evidence time out. The
snapshot is transient and is checked again with the final identity; it is never
stored with account credentials. These are collection-consistency checks, not
proof of upstream acceptance.

One real local Docker refresh with this code completed in 26.6 seconds (media
collection 10.176 seconds), observed one SDK instance and all three callback
measurements, and advanced the credential revision while preserving both unknown
reservations. Final targeted tests passed 30/30 in the Worker image. No further
generation was included in that refresh verification.

A subsequent explicitly authorized single-image attempt at 09:01 CST was blocked
before upstream submission: the helper created the CFM task about 23 seconds
after audited release, inside the store's 30-second settlement interval. The
result was local `no eligible account with confirmed quota for this model`, not
an Adobe 408. No account was selected and no upstream job was created. The
helper now checks settlement and reread quota before submission; its consumed
single-attempt guard remains. No second task was submitted. The captured media
session was also about 8 hours old by then, so this attempt establishes neither
current media validity nor recovery from 408.

Correction to the older video transport investigation below: the retained Node
control response explicitly includes `x-access-error:taste_exhausted`. That
particular 403 is an experience-quota rejection, not evidence of Go transport
incompatibility. This header was absent from the current image 408 response.

## Account recovery follow-up (2026-11-11)

On `codex/adobe-account-recovery`, a local Docker relogin completed with the
existing account profile and proxy after disabling restored tabs for the dedicated
worker. The context contained one page; the official SPL authentication supplied
a nonempty ARP token and matching identity. CFM persisted `spl_runtime/present`.
The complete task took about 47 seconds and retained its existing unknown media
submission. This verified session renewal, not a new password/OTP exchange or
successful generation.

The current official Nano Banana 2.1 builders (`cX`, `lX`, `Gxe` in the inspected
Firefly `current-index.js`) send `groundSearch:false` and `modelSpecificPayload:{}`
alongside `resolution:"1K"` and `generationSettings.aspectRatio`, without `size`.
The Go adapter now includes those two previously omitted defaults only for
`gemini-flash/nano-banana-2026-09`. The mock HTTP transport checks the complete
request contract; it does not prove that these defaults caused the prior 408.

Official UGS module 212587 classifies HTTP 408 with `error_code:"timeout_error"`
as `backend_overload`. That classification alone cannot establish an actual
capacity problem or rule out session compatibility. The failed CFM submissions
have no trusted poll link; their unknown reservations are retained without replay.
Earlier Nano Banana 2 success (`nano-banana-3`) is a different model version.

## Earlier integration validation

Checked locally in Docker on 2026-11-11 (Asia/Shanghai). These results are for the
`codex/adobe-integration` development branch. No production service was deployed
or restarted, and `main` was not merged.

## Earlier two-model admission (superseded below)

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


## New account, official free entitlements and expanded capabilities (2026-11-11)

This section supersedes the earlier two-model allowlist and the inference that
missing internal Sherlock fields mean an unusable session. Historical requests
and failures above remain separate observations.

A new user-supplied mailbox was registered once in the local Docker browser. The
user supplied the email OTP; the helper verified the Firefly identity and saved
private credentials. The new account is separate from the earlier account.
No credentials, mailbox address, OTP, signed URL or raw account identifier is
included here.

The official feature-auth-profile request returned 28 model results. Fourteen
were authorized using free resources; all fourteen then passed their complete
UGS generation-preflight payloads with HTTP 200. The admitted catalog is eight
image and six video models listed in README. Sunburst, Nano Banana Pro, standard
Veo and the other excluded candidates did not receive free authorization for the
queried account/parameters. Static web visibility alone was not used for admission.

The fresh account exposes three distinct shared pools:

| Pool | Provisioned / consumed | Refresh |
| --- | --- | --- |
| Complimentary image generations | 3 / 0 | P1D |
| Complimentary video generations | 1 / 0 | P1D |
| GPT Image 2 limited taste | 3 / 0 | No refresh period supplied |

The account UI now shows each pool once. General credits (previously 10/0/10)
remain separate and do not determine free generation eligibility. Each actual
request checks its model/parameter preflight before uploading or generating;
paid resources are rejected. Cached explicit model denial or insufficient shared
quota is filtered before binding, without switching account/channel afterward.
Missing authorization data is unknown/error, never a fabricated zero balance.

Public discovery, current UGS web overrides and preflight confirm:

- Nano Banana 2 reference input, including the user-observed 4:3 1K tier (1200x896),
  ten explicit 1K aspect/size mappings, and web reference limit six.
- Nano Banana 2.1 and the other admitted models have their own schemas and limits.
- Veo Lite accepts 1080p/8s audio and a first-frame reference. First/last frames use
  usage=frame and order=1/2; the client fits reference frames to the output canvas.
- Omni 1.1/1.2 audio is required rather than optional.
- The current UGS uploads raw image bytes to the third-party generation base's
  /v2/storage/image and uses images[0].id. No presigned upload flow was invented.
- Firefly first-party branded models use the third-party UGS route in current web
  overrides; a model's brand does not determine its HTTP endpoint.
- Official ARP handling forwards an opaque token. It does not parse or require
  sid/ark/bfp/ftr. No challenge fields are fabricated or merged.

Actual generation results are distinct from preflight success:

| Test | Free preflight | Upload | Generation |
| --- | --- | --- | --- |
| Nano Banana 2, 4:3, one reference | 200, authorized | 200, image ID | 408 timeout_error, no task handle |
| Veo Lite, 1080p, 8s, audio, first frame | 200, authorized | 200, image ID | 408 timeout_error, no task handle |
| GPT Image 2.5 Flare, 1024 square, text, new CFM Go client | 200, authorized | Not needed | 408 timeout_error, no task handle |

Each was a separate single submission, not a replay or fallback. The two Node
requests retained private markers; the Go client explicitly records
SUBMISSION_UNKNOWN for 408. No generated file or audio track was obtained, so
reference fidelity and actual audio output remain unverified. No further
submission was made after these tests.

A bounded normal editor initialization using the same new profile and proxy
waited until the prompt editor was visible/enabled and IMS ready (about 12s).
Identity matched. The official media cookie changed but still contained sid/ftr;
the token and browser user agent stayed unchanged. The subsequent distinct Go
Flare test still timed out. The runtime authentication service token was not
observable through the known public page element, so equality with the exported
cookie is not established. These facts do not establish missing challenge fields,
reference input, audio, model permission, Go transport, or quota as the 408 cause.
The tests do not constitute a complete live generation acceptance.

## Media-session scheduling validation (2026-11-12)

The local scheduler now prefers trusted ready sessions before least-recently-used
selection. The five-minute process-local reuse window is a CFM policy, not an
observed Adobe expiration guarantee. Background warmup, foreground preparation,
credential maintenance and quota observations share account gates. Quota-only
updates do not extend or invalidate readiness; imports and IMS-only updates cannot
establish readiness. Failures never replay a previously bound generation.

Full offline Docker race tests passed for both Adobe and HTTP packages, including
reuse across distinct tasks, successful browser login reuse, restart invalidation,
actual egress changes, cancellation, persistent backoff, preparation coalescing,
busy-account fairness and maintenance due rechecks. The local application build
passed and was activated only on loopback port 28473; production was not updated.

A distinct Nano Banana 2.1 request was accepted at 11:19:12 CST. The scheduler
selected the second test account, whose previous media capture remained imported.
Browser preparation returned `adobe_relogin_media_session_incomplete` after about
57 seconds. No prepared-task marker or upstream-generation journal was created;
credentials remained unchanged, the reservation was released and a five-minute
preparation backoff was persisted. The request was not resubmitted and the planned
second request was not sent. This verifies failure handling, but does not establish
live cached-session reuse or its latency. The Worker returned no retained detailed
runtime diagnostic through this preparation path, so the precise cause of incomplete
collection was not established from this attempt.

Validation includes complete media combinations, upload failure with zero
generation requests, opaque sessions, 408 unknown handling, shared pool parsing,
model-aware account selection, audio idempotency and restart persistence, actual
PNG/JPEG/WebP decoding, and explicit output encoding mismatch. Docker-only Go,
Worker and frontend regressions are separate from live upstream success.

### Second-account recovery and live cache reuse (2026-11-12)

The preceding blocked attempt remains in history. A subsequent normal read-only
Worker refresh of the same second account, profile and configured HTTP proxy
succeeded in 27,773 ms before the final-verification change. Therefore the original
incomplete-session failure cannot be attributed uniquely to that change: the old
path discarded the detailed Worker diagnostic.

Two implementation gaps were repaired. Final identity verification now waits
boundedly for the official asynchronous observations; changed identity, token,
authentication source or SDK proof still fails immediately. Worker diagnostics
now survive through a strict fixed-enum/count projection into safe task errors,
monitor output and encrypted preparation backoff records. Integration testing
also found that HTTP relogin did not pass its resolved egress to the Store's cache
layer; the same pinned context now reaches both Store and browser.

Normal CFM relogin completed at 11:45:15 CST and saved credential revision 5.
Two distinct requests then used `/api/image-tasks/generations`, model
`gemini-3.1-flash-image-2.1`, size `1024x1024`, `n=1`. The browser only established
the session; generation, polling and download used CFM's Go HTTP path.

| Request | Start / terminal (CST) | Total | Preparation | Actual output |
| --- | --- | --- | --- | --- |
| Green ceramic cup | 11:46:04 / 11:46:26 | 22,462 ms | Cache hit, 5 ms | 1024×1024 PNG, 1,350,980 bytes |
| Wooden toy horse | 11:48:19 / 11:48:40 | 20,553 ms | Cache hit, 4 ms | 1024×1024 PNG, 1,430,112 bytes |

Both persisted routes bind the same second account. Credential revision and media
capture time remained unchanged; neither request recollected its media session.
Both images were downloaded through CFM, their PNG headers/dimensions and distinct
SHA-256 hashes checked, and their contents visually inspected. Actual daily image
availability was 3 → 2 → 1; video and GPT Image 2 taste pools were unchanged.
The first account remained unchanged, both accounts settled to idle, and monitoring
had zero active calls. The original failed task remains failed and was not replayed.

Docker race tests passed for Adobe and HTTP packages; final diagnostic and HTTP
relogin regressions also passed. Node tests passed 177/177 and Python shared-Worker
tests passed 83/83 on the new Worker image. Local app/Worker builds and diff checks
passed. This verifies the second account's current image generation and bounded
cache reuse; it does not establish an upstream session lifetime or test all models.
Only the local preview on loopback port 28473 was updated, without a production
release. Private validation evidence remains under the ignored
`artifacts/adobe-408-20261112/second-session/` directory.

### Cold video preparation and fresh-quota selection (2026-11-12)

A later Studio Omni Flash 1.2 request at 12:03:58 CST selected the first account
after the media cache expired. It failed after 59,119 ms of preparation with
`media_session_incomplete [unavailable]`. Its persisted route remained at
`media_session_preparing`; no video generation was submitted. The earlier two
successful cached image requests used the second account and do not validate this
first account's later cold initialization.

The initial runtime observer had grouped several causes into `unavailable`.
These now have fixed reasons, with bounded failure-only page/timing observations
in the Worker. Identity, SDK lifecycle and vendor-readiness checks remain strict;
the 30-second media collection budget and 250 ms stability window are unchanged.
A normal diagnostic of the same first account failed after 41,927 ms with
`pending/token_settling`, all three vendors complete and one observed authentication
instance. Another normal diagnostic completed in 20,725 ms. Neither saved returned
credentials to CFM or generated media. These observations establish intermittent
initialization, not the unique cause of the original generic failure or evidence
that quota exhaustion caused that media failure.

A separate deterministic selection bug was confirmed: stale model permissions
were treated as eligible, so the foreground path skipped quota refresh. Live reads
then confirmed the first account had no video authorization and zero available
video generations; the second had authorization and one available generation.
Selection now requires a fresh model observation before binding, refreshes unknown
or stale observations independently of background maintenance, and checks quota
revision/readiness again under the final binding lock. Failed observations share
a 30-second identity/egress-bound backoff and do not consume the next request's
four-candidate observation budget. Bound tasks still never change account/channel
or replay generation. Authorized fair-use resources retain their distinct no-counter
semantics; missing counters for counted resources are not treated as unlimited.

After only the local app and Worker were updated, one distinct new request used
the unified `/v1/videos` endpoint: Omni Flash 1.2, 720p, 16:9, 5 seconds, audio on,
with a yellow ceramic wind-chime prompt. Both account observations were stale at
entry. The application refreshed them, excluded the first account, bound the second
and collected its cold media session once. Results:

| Check | Observed result |
| --- | --- |
| Request start / completed (CST) | 12:52:18 / 12:53:38 |
| Total / media preparation | 79,794 ms / 31,515 ms, cache miss |
| Actual media | H.264, 1280×720, 5.000 s video; AAC stereo 48 kHz, 5.034667 s container |
| CFM download | HTTP 200, video/mp4, 2,157,767 bytes |
| Video quota | Second account 1 → 0; image quota unchanged |
| Final state | Both accounts idle; no active call; browser capacity released |

The MP4 fully decoded with FFmpeg and an extracted frame matched the wind-chime
scene. The original failed checkpoint remained identical and was not replayed.
The first account's credentials were unchanged; the second advanced from revision
5 to 6 through normal collection. Adobe/HTTP Docker race tests and final selection
regressions passed, as did 188 Node tests. The local binary and runtime checksum
matched. Production was not updated. Private evidence remains under
`artifacts/adobe-408-20261112/video-session-unavailable/` and the separate
`video-session-timing/` directory.
