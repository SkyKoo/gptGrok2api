ARG GROK_BROWSER_BASE_IMAGE=mcr.microsoft.com/playwright/python:v1.61.0-noble
FROM mcr.microsoft.com/playwright:v1.65.0-noble AS adobe-node
FROM ${GROK_BROWSER_BASE_IMAGE}

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    TURNSTILE_HEADLESS=0 \
    TURNSTILE_CONCURRENCY=1 \
    CLOAKBROWSER_CACHE_DIR=/data/cloakbrowser \
    CLOAKBROWSER_AUTO_UPDATE=false

COPY --from=adobe-node /usr/bin/node /usr/local/bin/node
COPY --from=adobe-node /usr/lib/node_modules /usr/local/lib/node_modules
RUN ln -s /usr/local/lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm
WORKDIR /app
COPY grok-browser/adobe/package*.json ./adobe/
RUN cd adobe && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 npm ci --omit=dev --ignore-scripts
COPY grok-browser/adobe ./adobe
COPY grok-browser/adobe_flow.py ./adobe_flow.py
COPY grok-browser/requirements.txt ./requirements.txt
RUN python -m pip install --no-cache-dir -r requirements.txt
COPY grok-browser/server.py ./server.py
COPY grok-browser/xai_flow.py grok-browser/oauth_device.py grok-browser/browser_sessions.py ./
COPY grok-browser/signup_errors.py grok-browser/network_diagnostics.py ./
COPY grok-browser/common ./common
COPY grok-browser/turnstile ./turnstile
COPY grok-browser/run-docker.sh ./run-docker.sh
RUN chmod +x ./run-docker.sh

EXPOSE 8899
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=4 \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8899/health', timeout=3)"

CMD ["./run-docker.sh"]
