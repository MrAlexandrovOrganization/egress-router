FROM ghcr.io/sagernet/sing-box:v1.14.0 AS singbox
FROM python:3.13-alpine
COPY --from=singbox /usr/local/bin/sing-box /usr/local/bin/sing-box
COPY manager.py /app/manager.py
RUN chmod 755 /app/manager.py
ENTRYPOINT ["/app/manager.py"]
