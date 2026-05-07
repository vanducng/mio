FROM python:3.12-slim

WORKDIR /app
COPY receiver/ /app/

ENV PYTHONUNBUFFERED=1 \
    LISTEN_HOST=0.0.0.0 \
    LISTEN_PORT=8765 \
    LOG_PATH=/appdata/messages.jsonl

EXPOSE 8765
HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=10s \
  CMD python -c "import urllib.request,sys; sys.exit(0 if urllib.request.urlopen('http://127.0.0.1:8765/healthz', timeout=3).status==200 else 1)"

CMD ["python", "/app/server.py"]
