#!/usr/bin/env python3
"""Read-only post-maintenance probes; ordinary background compensation may run."""
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
import hashlib
import json
import os
from pathlib import Path
import re
import tarfile
import tempfile
import urllib.error
import urllib.request
import urllib.parse

import common
import paired_phases
import snapshot


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def get(url, token=None, *, timeout=30):
    headers = {'User-Agent': 'curl/8.5.0', 'Accept-Encoding': 'identity'}
    if token:
        # Administrative credentials are used only on the local stable ingress.
        common.require(url.startswith('http://127.0.0.1:8000/'), 'Authenticated probe must remain local')
        headers['Authorization'] = 'Bearer ' + token
    request = urllib.request.Request(url, headers=headers)
    try:
        with urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect).open(request, timeout=timeout) as response:
            return response.status, response.read()
    except urllib.error.HTTPError as response:
        return response.code, response.read()


def verify_stable_containers(host, current, baseline):
    """Allow only gateway health to settle after the maintenance hold."""
    front = current['frontdoor']
    def inspect_front():
        return json.loads(host.run(['docker', 'inspect', front], timeout=5))[0]
    paired_phases.wait_stable_health(inspect_front, baseline['containers'][front], common.container_summary)
    for name in common.DEPENDENCIES.values():
        info = host.inspect(name)
        common.require(common.healthy(info) and common.container_summary(info) == baseline['containers'][name],
                       'Stable database container changed')


def image_web_files(host, container):
    """Read the index and flat JS/CSS files in one exec, without extracting tar."""
    # The script is fixed text: container/path values are never interpolated.
    # Pass explicit files to tar so other assets or business mounts are not read.
    script = ('set -eu; test ! -L /app/web_dist; cd /app/web_dist; '
              'test -d assets; test ! -L assets; set -- index.html; '
              'for asset in assets/*.js assets/*.css; do '
              '[ -e "$asset" ] || continue; test -f "$asset"; test ! -L "$asset"; '
              'set -- "$@" "$asset"; done; '
              'exec tar -cf - "$@"')
    files = {}
    with tempfile.TemporaryFile() as bundle:
        host.run(['docker', 'exec', container, 'sh', '-c', script], output=bundle, timeout=30)
        common.require(os.fstat(bundle.fileno()).st_size <= 64 * 1024 * 1024,
                       'Image web asset bundle exceeds 64 MiB')
        bundle.seek(0)
        with tarfile.open(fileobj=bundle, mode='r|') as archive:
            for member in archive:
                common.require(member.isfile() and not member.issparse()
                               and (member.name == 'index.html'
                                    or re.fullmatch(r'assets/[A-Za-z0-9_.-]+\.(js|css)', member.name))
                               and member.name not in files,
                               'Unexpected, duplicate or linked image asset')
                common.require(len(files) < 4096 and 0 <= member.size <= 16 * 1024 * 1024,
                               'Image web asset exceeds verification limits')
                with archive.extractfile(member) as stream:
                    data = stream.read(member.size + 1)
                common.require(len(data) == member.size, 'Image web asset was truncated')
                files[member.name] = data
    common.require('index.html' in files, 'Image index is missing')
    return files


def verify_published_assets(index, files, bases, *, fetch=None):
    """Check the complete index dependency graph with at most four HTTP GETs."""
    fetch = fetch or get
    pending = set(re.findall(r'(?:src|href)="(/assets/[^"?#]+)"', index.decode()))
    common.require(pending and bases, 'Published asset graph or origin is missing')
    assets = {}
    while pending:
        asset = pending.pop()
        if asset in assets:
            continue
        common.require(re.fullmatch(r'/assets/[A-Za-z0-9_.-]+\.(js|css)', asset),
                       'Unexpected public asset path')
        common.require(asset[1:] in files, 'Image asset dependency is missing')
        data = files[asset[1:]]
        assets[asset] = data
        if asset.endswith('.js'):
            for name in re.findall(r'[\"\x27](?:\./|/assets/|assets/)([A-Za-z0-9_.-]+\.(?:js|css))[\"\x27]', data.decode()):
                pending.add('/assets/' + name)

    def verify_one(url, asset, expected):
        status, served = fetch(url + asset)
        common.require(status == 200 and served == expected, 'Published asset differs from image')

    # Only graph members are requested; stale additive assets remain excluded.
    # On failure, stop queued work and let the at-most-four in-flight GETs end.
    with ThreadPoolExecutor(max_workers=4) as pool:
        futures = [pool.submit(verify_one, base, asset, data)
                   for asset, data in sorted(assets.items()) for base in bases]
        try:
            for future in as_completed(futures):
                future.result()
        except BaseException:
            for future in futures:
                future.cancel()
            raise
    return [{'name': Path(asset).name, 'sha256': hashlib.sha256(data).hexdigest()}
            for asset, data in sorted(assets.items())]


def verify_egress_gets(admin):
    result = {}
    paths = {'openai': '/api/accounts?page=1&page_size=1',
             'grok': '/api/register/grok/accounts?page=1&page_size=1',
             'adobe': '/api/adobe/accounts?page=1&page_size=1'}
    for platform, path in paths.items():
        status, raw = get('http://127.0.0.1:8000' + path, admin)
        common.require(status == 200, 'Daily egress account listing failed')
        rows = json.loads(raw).get('items')
        common.require(isinstance(rows, list) and len(rows) == 1 and isinstance(rows[0].get('id'), str),
                       'Daily egress sample account unavailable')
        account_id = rows[0]['id']
        query = urllib.parse.urlencode({'platform': platform, 'account_id': account_id})
        status, raw = get('http://127.0.0.1:8000/api/proxy/binding?' + query, admin)
        common.require(status == 200, 'Daily egress binding GET failed')
        value = json.loads(raw)
        common.require(value.get('platform') == platform and value.get('account_id') == account_id
                       and value.get('policy') in ('inherit', 'explicit')
                       and type(value.get('available')) is bool, 'Daily egress policy contract differs')
        # The live admin may have intentionally selected an explicit policy.
        # Observe it without changing bindings or printing identities/history.
        result[platform] = {'policy': value['policy'], 'configuration_available': value['available']}
    return result


def verify_adobe_registration_get(admin):
    """Observe compensation without requesting a retry or exporting private rows."""
    status, raw = get('http://127.0.0.1:8000/api/register/adobe/jobs?page=1', admin)
    common.require(status == 200, 'Adobe registration progress GET failed')
    value = json.loads(raw)
    common.require(isinstance(value, dict) and type(value.get('running')) is bool
                   and isinstance(value.get('jobs'), list)
                   and type(value.get('jobs_total')) is int and value['jobs_total'] >= 0
                   and value.get('jobs_page') == 1,
                   'Adobe registration progress schema differs')
    common.require(not value.get('error') and not value.get('compensation_error'),
                   'Adobe registration journal or compensation scheduler reports an error')
    modes = {'resume_session': 0, 'retry_registration': 0, 'manual_reregistration': 0, 'saved_result': 0,
             'result_uncertain': 0, 'in_progress_or_superseded': 0}
    outcomes = {outcome: 0 for outcome in ('not_submitted', 'rejected', 'accepted', 'unknown')}
    states = {state: 0 for state in ('waiting', 'running', 'manual', 'manual_running',
              'exhausted', 'superseded', 'completed', 'saved_result')}
    no_plan = 0
    reregister_available = 0
    for job in value['jobs']:
        common.require(isinstance(job, dict) and job.get('recovery_mode') in
                       ('resume_session', 'retry_registration', 'manual_reregistration',
                        'saved_result', 'result_uncertain', '')
                       and all(type(job.get(key)) is bool for key in
                           ('can_retry', 'can_retry_registration', 'can_resume_registration', 'can_reregister'))
                       and isinstance(job.get('reregister_not_before'), str),
                       'Adobe registration recovery actions are missing')
        mode, outcome = job['recovery_mode'], job.get('creation_outcome')
        common.require(isinstance(outcome, str) and outcome in outcomes,
                       'Adobe creation outcome is missing or invalid')
        common.require(mode != 'retry_registration' or outcome in ('not_submitted', 'rejected'),
                       'Registration retry does not have a proven unsubmitted or rejected creation')
        common.require(mode != 'resume_session' or outcome == 'accepted',
                       'Registration continuation is not backed by accepted creation')
        common.require(mode not in ('result_uncertain', '') or outcome == 'unknown',
                       'Uncertain recovery mode differs from creation outcome')
        common.require(mode != '' or job.get('status') in ('running', 'queued', 'completed')
                       or isinstance(job.get('retry_job_id'), str) and bool(job['retry_job_id']),
                       'Hidden uncertainty is not an active, completed or superseded job')
        flags = ('can_retry', 'can_retry_registration', 'can_resume_registration')
        common.require(sum(job[key] for key in flags) <= 1
                       and (not job['can_retry'] or mode == 'saved_result')
                       and (not job['can_retry_registration'] or mode == 'retry_registration')
                       and (not job['can_resume_registration'] or mode == 'resume_session'),
                       'Adobe registration actions contradict the recovery mode')
        # An accepted but unfinished signup may only restart manually in a new
        # session. A saved account, media handoff or superseded task stays protected.
        common.require(not job['can_reregister'] or (
                       mode in ('retry_registration', 'result_uncertain') and outcome != 'accepted'
                       or mode == 'manual_reregistration' and outcome == 'accepted')
                       and not job.get('retry_job_id')
                       and job.get('status') not in ('running', 'queued', 'completed')
                       and not job.get('imported') and not job.get('verified'),
                       'Adobe manual registration contradicts a protected task or saved account')
        common.require(mode != 'manual_reregistration' or (
                       outcome == 'accepted' and not job.get('imported') and not job.get('verified')
                       and not any(job[key] for key in flags)),
                       'Adobe manual restart contract differs')
        reregister_available += int(job['can_reregister'])
        modes[mode or 'in_progress_or_superseded'] += 1
        outcomes[outcome] += 1
        retry = job.get('auto_retry')
        if retry is None:
            no_plan += 1
            continue
        common.require(isinstance(retry, dict) and retry.get('state') in states
                       and type(retry.get('attempts')) is int and retry['attempts'] >= 0
                       and type(retry.get('max_attempts')) is int and retry['max_attempts'] == 3,
                       'Adobe registration compensation schema differs')
        states[retry['state']] += 1
    # A resumed scheduler may adopt old terminal tasks, create a child, or finish
    # while this GET is in flight. Counts describe only this page and snapshot;
    # they are not frozen-state or account-creation success assertions.
    return {'get_verified': True, 'running': value['running'],
            'jobs_total': value['jobs_total'], 'sampled_jobs': len(value['jobs']),
            'sampled_recovery_modes': modes, 'sampled_creation_outcomes': outcomes,
            'sampled_compensation_states': states,
            'sampled_without_plan': no_plan,
            'sampled_manual_registration_available': reregister_available}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--operation-dir', type=Path, default=Path(__file__).resolve().parent)
    parser.add_argument('--baseline', type=Path, required=True)
    parser.add_argument('--worker-manifest', type=Path, required=True)
    parser.add_argument('--public-base', help='override the app container GO_PUBLIC_BASE_URL')
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    common.host_required()
    r, base = common.rolling(args.root), common.load(args.baseline)
    host = r.Host()
    state = common.load(args.operation_dir / 'release-state.json')
    current = common.load(args.root / 'current.json')
    common.require(state['status'] == 'success' and current['status'] == 'healthy'
                   and current['commit'] == common.COMMIT, 'Maintenance did not complete successfully')
    common.require(common.helper_hashes(args.operation_dir) == base['helpers'], 'Reviewed helper bytes changed')
    common.require(common.file_sha(args.worker_manifest) == base['worker_manifest_sha256'], 'Worker manifest changed')
    common.require(common.file_sha(args.operation_dir / 'worker-config-plan.json') == base['worker_config_plan_sha256'],
                   'Worker image-only configuration plan changed')
    common.require(common.verify_source(host, base['candidate']) == base['source'], 'Fixed source changed')
    common.check_image(host, current['image'], current['image_id'], common.COMMIT, app=True)
    common.check_image(host, base['current']['image'], base['current']['image_id'], base['current']['commit'], app=True)
    common.require(current['image_id'] == base['candidate']['image_id'], 'Unexpected active image')
    app = host.inspect(current['container'])
    common.require(common.healthy(app) and app['RestartCount'] == 0 and app['Image'] == current['image_id'],
                   'Active app restarted or is unhealthy')
    common.require(not host.inspect(base['current']['container'])['State']['Running'], 'Retired old app still running')
    control, gateway = host.control(current['container']), host.gateway(current['frontdoor'])
    common.require(control.get('phase') == 'active' and control.get('writer_lease') is True
                   and control.get('paused') is False, 'Candidate did not resume as the active writer')
    common.require(gateway['route']['held'] is False and gateway['route']['upstream'] == current['upstream'],
                   'Gateway route does not match active app')
    verify_stable_containers(host, current, base)
    previous = base['containers'][base['current']['container']]
    actual = common.container_summary(app)
    common.require(actual['environment_sha256'] == previous['environment_sha256']
                   and actual['networks'] == previous['networks'] and actual['ports'] == previous['ports'],
                   'App environment or networks changed')
    old_mounts = {mount['Destination']: mount for mount in previous['mounts']}
    new_mounts = {mount['Destination']: mount for mount in actual['mounts']}
    common.require(set(old_mounts) == set(new_mounts), 'App mount targets changed')
    for target in old_mounts:
        if target == '/app/runtime':
            common.require(old_mounts[target]['Source'] != new_mounts[target]['Source'], 'Old runtime was reused')
        else:
            common.require(old_mounts[target] == new_mounts[target], 'App business/static mount changed')
    common.run_helper(host, args.operation_dir, 'worker-release.py',
                      ['--root', args.root, '--operation-dir', args.operation_dir,
                       '--candidate-manifest', args.worker_manifest, '--action', 'verify-live'])
    common.check_image(host, base['worker']['image'], base['worker']['image_id'], common.COMMIT)
    common.require(current.get('grok_browser_container') == common.WORKER
                   and current.get('grok_browser_commit') == common.COMMIT
                   and current.get('grok_browser_image_id') == base['worker']['image_id'],
                   'Current Worker metadata differs from candidate')
    common.require(common.saved_switches(args.root) == base['switches'], 'Saved switches changed')
    common.require(common.file_sha(args.root / 'tools/backup-core.py') == base['candidate']['backup_tool_sha256'],
                   'Installed backup script omits candidate coverage')
    common.require(common.backup_policy_identity(args.root) == base['backup_policy'],
                   'Independently installed backup policy changed')
    backup = Path(state['backup'])
    backup_verification = snapshot.verify_backup(backup, runtime_files=r.RUNTIME_FILES, checkpoint=host.checkpoint)
    before = common.load(backup / 'business-before.json')
    expected_files = before['files']
    if common.load(backup / 'complete').get('media_included') is False:
        expected_files = {name: digest for name, digest in expected_files.items()
                          if not r.daily_media_file('data/' + name)}
    common.require(backup_verification['files'] == expected_files, 'Backup business file manifest differs')
    common.require((args.operation_dir / 'state-after.json').is_file(), 'Strict unchanged-state evidence missing')
    report = {'verified': True, 'verified_at': r.now(), 'commit': common.COMMIT, 'image_id': current['image_id'],
              'worker_image_id': base['worker']['image_id'], 'backup': str(backup),
              'backup_checksums_verified': backup_verification['checksums_verified'], 'switches_unchanged': True,
              'backup_metadata_verified': True,
              'raw_snapshot_verification': backup_verification['raw_snapshot'],
              'old_images_retained': True, 'health': [], 'assets': [],
              'real_generation_performed': False, 'registration_requested_by_verifier': False,
              'verification_requests_read_only': True, 'background_compensation_may_run': True,
              'data_restore_performed': False,
              'existing_model_catalog_cache_may_refresh': True}
    # Resolve the same configured key precedence as internal/config/config.go.
    # Neither keys nor raw account/config responses are written to any artifact.
    env = dict(value.split('=', 1) for value in app['Config']['Env'] if '=' in value)
    config = common.load(args.root / 'data/config.json')
    admin = (env.get('CHATGPT2API_ADMIN_KEY') or config.get('app_key')
             or config.get('admin_key') or env.get('CHATGPT2API_AUTH_KEY')
             or config.get('auth-key') or config.get('api_key'))
    common.require(isinstance(admin, str) and admin.strip(), 'Configured admin key unavailable for read-only probes')
    required_models = {'gemini-3.1-flash-image', 'veo-3.1-lite', 'firefly-image-4',
                       'firefly-image-5', 'firefly-video'}
    status, body = get('http://127.0.0.1:8000/v1/models', admin.strip())
    common.require(status == 200, 'Unified authenticated model catalog failed')
    model_ids = {row['id'] for row in json.loads(body)['data']}
    common.require(required_models <= model_ids, 'Adobe logical models absent from unified catalog')
    status, body = get('http://127.0.0.1:8000/api/adobe/accounts?page=1&page_size=1', admin.strip())
    common.require(status == 200, 'Authenticated Adobe account listing failed')
    accounts = json.loads(body)
    common.require(isinstance(accounts.get('items'), list) and isinstance(accounts.get('total'), int),
                   'Adobe account listing schema differs')
    status, body = get('http://127.0.0.1:8000/api/model-routing', admin.strip())
    common.require(status == 200, 'Model route configuration GET failed')
    routes = {row['model']: row for row in json.loads(body)['models']}
    common.require(all('adobe' in routes.get(model, {}).get('channels', []) for model in required_models),
                   'Adobe logical model routes are missing')
    common.require(set(routes.get('gpt-image-2.5-flare', {}).get('channels', [])) == {'chatgpt', 'adobe'},
                   'Shared logical image model lacks both channels')
    for row in routes.values():
        common.require(sorted(row['channels']) == sorted(row['priority']), 'Channel priority is incomplete')
    report['authenticated_gets'] = {'unified_models': True, 'adobe_accounts': True, 'model_routing': True,
                                    'required_models': sorted(required_models),
                                    'overlapping_model': 'gpt-image-2.5-flare'}
    status, body = get('http://127.0.0.1:8000/api/icloud/sources', admin.strip())
    common.require(status == 200, 'HME source projection GET failed')
    sources = json.loads(body).get('sources')
    common.require(isinstance(sources, list), 'HME source projection schema differs')
    hme = [source for source in sources if source.get('type') == 'icloud_hme']
    common.require(any(source.get('status') == 'available' for source in hme) and all(source.get('capabilities', {}).get('adobe') is True
                              for source in hme if source.get('status') == 'available'),
                   'Ready HME source does not expose Adobe registration')
    status, body = get('http://127.0.0.1:8000/api/adobe/accounts/relogin', admin.strip())
    common.require(status == 200 and isinstance(json.loads(body).get('items'), list),
                   'Adobe relogin progress GET failed')
    report['authenticated_gets'].update(hme_sources=True, adobe_relogin_progress=True)
    report['daily_egress_gets'] = verify_egress_gets(admin.strip())
    report['adobe_registration_get'] = verify_adobe_registration_get(admin.strip())
    public_base = args.public_base
    if public_base is None:
        public_base = env.get('GO_PUBLIC_BASE_URL')
        common.require(public_base, 'Published URL is missing from the active app configuration')
    del admin, config, env, body, accounts
    bases = ['http://127.0.0.1:8000']
    if public_base:
        bases.append(public_base.rstrip('/'))
    for url in bases:
        for path in ('/health', '/readyz'):
            status, _ = get(url + path)
            common.require(status == 200, 'Published health check failed')
            report['health'].append({'url': url + path, 'status': status})
        status, _ = get(url + '/api/adobe/accounts')
        common.require(status in (401, 403), 'Adobe admin endpoint auth guard differs')
    image_files = image_web_files(host, current['container'])
    index = image_files['index.html']
    status, local = get(bases[0] + '/')
    common.require(status == 200 and local == index, 'Origin HTML differs from current image')
    if len(bases) > 1:
        status, public = get(bases[1] + '/')
        common.require(status == 200, 'Public HTML unavailable')
        # The existing Cloudflare analytics beacon is the sole allowed addition.
        text, count = re.subn(r'<script\b(?=[^>]*\bsrc=[\"\x27]https://static\.cloudflareinsights\.com/beacon\.min\.js(?:/[a-zA-Z0-9]+)?[\"\x27])[^>]*>\s*</script>', '', public.decode())
        normalize = lambda value: re.sub(r'>\s+<', '><', value).strip()
        common.require(count in (0, 1) and normalize(text) == normalize(index.decode()),
                       'Public HTML is not the current index')
    report['assets'] = verify_published_assets(index, image_files, bases)
    r.atomic_json(args.output, report)
    print(json.dumps({'verified': True, 'commit': common.COMMIT, 'assets_verified': len(report['assets']),
                      'backup_verified': True, 'saved_switches_unchanged': True}))


if __name__ == '__main__':
    common.safe_main(main)
