"""Run inside the candidate image with --network none; never launches a browser."""
import hashlib
import importlib.metadata
import json
import pathlib
import subprocess
import sys

sys.path.insert(0, '/app')
import adobe_flow
import browser_sessions
import server
import xai_flow
from cloakbrowser.config import (CHROMIUM_VERSION, get_cache_dir,
                                get_chromium_version, get_platform_tag)

root = pathlib.Path('/app')
manifest = json.load(sys.stdin)
observed = {}
for relative, expected in manifest['production_files'].items():
    target = root / relative
    digest = hashlib.sha256(target.read_bytes()).hexdigest()
    if digest != expected:
        raise RuntimeError('candidate worker source mismatch: ' + relative)
    observed[relative] = digest
node_script = '''
import fs from 'node:fs';
import * as cloak from '/app/adobe/node_modules/cloakbrowser/dist/index.js';
import {CHROMIUM_VERSION,getCacheDir,getChromiumVersion,getPlatformTag} from '/app/adobe/node_modules/cloakbrowser/dist/config.js';
const read=name=>JSON.parse(fs.readFileSync('/app/adobe/node_modules/'+name+'/package.json','utf8')).version;
if(typeof cloak.launchPersistentContext!=='function')throw Error('persistent API missing');
console.log(JSON.stringify({version:process.versions.node,cloakbrowser:read('cloakbrowser'),playwright:read('playwright'),cache:getCacheDir(),platform:getPlatformTag(),chromium_version:CHROMIUM_VERSION,platform_chromium_version:getChromiumVersion()}));
'''
node = json.loads(subprocess.check_output(['node', '--input-type=module', '-e', node_script], text=True))
python_version = importlib.metadata.version('cloakbrowser')
assert python_version == manifest['python_cloakbrowser']
assert node['cloakbrowser'] == manifest['node_cloakbrowser']
assert importlib.metadata.version('playwright') == manifest['python_playwright']
assert node['playwright'] == manifest['node_playwright']
assert int(node['version'].split('.')[0]) >= manifest['node_minimum_major']
assert get_platform_tag() == node['platform'] == 'linux-arm64'
assert str(get_cache_dir()) == node['cache']
assert CHROMIUM_VERSION == node['chromium_version']
assert get_chromium_version() == node['platform_chromium_version']
# Independent country lookup is part of this image contract. This command is
# offline and carries neither a URL nor Adobe/browser credentials.
curl_output = subprocess.check_output(['curl', '--version'], text=True, timeout=5)
curl_lines = curl_output.splitlines()
assert curl_lines and curl_lines[0].startswith('curl ')
curl_protocols = next(line.split(':', 1)[1].split() for line in curl_lines if line.startswith('Protocols:'))
curl_features = next(line.split(':', 1)[1].split() for line in curl_lines if line.startswith('Features:'))
assert {'http', 'https'} <= set(curl_protocols) and 'HTTPS-proxy' in curl_features
curl_version = curl_lines[0].split()[1]
paths = {route.path for route in server.app.routes}
assert {'/health', '/v1/adobe/run', '/v1/browser/reservations', '/v1/session/start', '/v1/oauth/authorize-device'} <= paths
origins = {module.__name__: module.__file__ for module in (adobe_flow, browser_sessions, server, xai_flow)}
assert all(path.startswith('/app/') for path in origins.values())
print(json.dumps({'python_cloakbrowser': python_version, 'python_playwright': importlib.metadata.version('playwright'), 'python_chromium_version': CHROMIUM_VERSION, 'python_platform_chromium_version': get_chromium_version(), 'node': node, 'curl_version': curl_version, 'country_lookup_transport_available': True, 'source_files_verified': len(observed), 'module_origins': origins, 'browser_started': False}))
