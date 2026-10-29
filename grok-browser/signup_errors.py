"""Stable worker error semantics; never infer a timeout from retryability."""

REVIEW_REASONS = frozenset({
    "signup_unavailable", "email_already_registered", "email_invalid",
    "turnstile_incomplete", "cloudflare_challenge", "verification_code_invalid",
})


def signup_error_status(reason: str, retryable: bool) -> int:
    if reason == "email_already_registered":
        return 409
    if reason in REVIEW_REASONS:
        return 422
    if reason == "signup_rate_limited":
        return 429
    if reason == "proxy_connection_failed":
        return 502
    return 504 if retryable else 422
