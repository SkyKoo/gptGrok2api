ARG GROK_BROWSER_BASE_IMAGE=mcr.microsoft.com/playwright/python:v1.61.0-noble
FROM ${GROK_BROWSER_BASE_IMAGE}

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    TURNSTILE_HEADLESS=0 \
    TURNSTILE_CONCURRENCY=1

WORKDIR /app
COPY grok-browser/requirements.txt ./requirements.txt
RUN python -m pip install --no-cache-dir -r requirements.txt
COPY grok-browser/server.py ./server.py
COPY grok-browser/xai_flow.py ./xai_flow.py
COPY grok-browser/signup_errors.py grok-browser/network_diagnostics.py ./
COPY grok-browser/common ./common
COPY grok-browser/turnstile ./turnstile
COPY grok-browser/run-docker.sh ./run-docker.sh
RUN chmod +x ./run-docker.sh

EXPOSE 8899
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=4 \
  CMD python -c "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8899/health', timeout=3)"

CMD ["./run-docker.sh"]
